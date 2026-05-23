package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/GipsyChef/cluster-optimizer/internal/analyzer"
	"github.com/GipsyChef/cluster-optimizer/internal/applier"
	"github.com/GipsyChef/cluster-optimizer/internal/classifier"
	"github.com/GipsyChef/cluster-optimizer/internal/collector"
	"github.com/GipsyChef/cluster-optimizer/internal/nudger"
	"github.com/GipsyChef/cluster-optimizer/internal/plan"
	"github.com/GipsyChef/cluster-optimizer/internal/store"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "cluster-optimizer: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	var clusterID string
	var output string
	var timeout time.Duration
	var nudge bool
	var autoApply bool
	var targetsPath string
	flags := flag.NewFlagSet("cluster-optimizer", flag.ContinueOnError)
	flags.StringVar(&clusterID, "cluster-id", envOr("CLUSTER_OPTIMIZER_CLUSTER_ID", "default"), "stable cluster identifier")
	flags.StringVar(&output, "output", envOr("OUTPUT_FORMAT", "json"), "json or text")
	flags.DurationVar(&timeout, "timeout", 25*time.Second, "collection timeout")
	flags.BoolVar(&nudge, "nudge", envBoolOr("CLUSTER_OPTIMIZER_NUDGE", false), "actively nudge pods to run on fewer nodes (dry-run unless CLUSTER_OPTIMIZER_NUDGE_LIVE=true)")
	flags.BoolVar(&autoApply, "auto-apply", envBoolOr("CLUSTER_OPTIMIZER_AUTOAPPLY_FLAG", false), "request live in-cluster auto-apply (requires CLUSTER_OPTIMIZER_AUTOAPPLY=true env to actually mutate)")
	flags.StringVar(&targetsPath, "targets", envOr("CLUSTER_OPTIMIZER_TARGETS", "/etc/cluster-optimizer/remediation-targets.json"), "path to remediation-targets.json")
	if err := flags.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	targets, err := classifier.LoadTargets(targetsPath)
	if err != nil {
		// Loading targets is best-effort; an unreadable file should not
		// prevent the advisory run, but it does prevent any remediation.
		fmt.Fprintf(os.Stderr, "cluster-optimizer: failed to load targets at %q: %v\n", targetsPath, err)
	}
	cls := classifier.New(clusterID, targets)

	snapshot, err := collector.Collect(ctx, clusterID)
	if err != nil {
		return err
	}
	report := analyzer.AnalyzeWith(snapshot, cls)

	var occurrences map[string]int64
	if table := os.Getenv("DYNAMODB_TABLE"); table != "" {
		writer, err := store.NewDynamoDBWriter(ctx, table)
		if err != nil {
			return err
		}
		// Fetch existing recs BEFORE the planner runs so PutReport's bump
		// doesn't inflate this run's count, and hand the same map to
		// PutReport so it doesn't re-Query for first_seen_at / occurrences.
		existing, _ := writer.ExistingRecommendations(ctx, clusterID)
		occurrences = make(map[string]int64, len(existing))
		for key, rec := range existing {
			occurrences[key] = rec.Occurrences
		}
		if err := writer.PutReport(ctx, report, existing); err != nil {
			return err
		}
	}

	switch output {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	case "text":
		fmt.Print(renderText(report))
	default:
		return fmt.Errorf("unsupported output format %q", output)
	}

	// Build a plan and, when there is work to log, hand it to the applier.
	// The applier internally chooses dry-run vs live based on its gates,
	// so we don't need to gate on autoApply here — only on having actions.
	policy := plan.DefaultPolicy()
	p := plan.Build(report, snapshot, cls, policy, occurrences)
	if len(p.Actions) > 0 {
		clientset, clientErr := collector.GetClientset()
		if clientErr != nil {
			fmt.Fprintf(os.Stderr, "cluster-optimizer: cannot build clientset for applier: %v\n", clientErr)
		} else {
			opts := applier.NewOptions()
			opts.AutoApply = autoApply
			opts.AutoApplyEnvSet = envBoolOr("CLUSTER_OPTIMIZER_AUTOAPPLY", false)
			_ = applier.Apply(ctx, clientset, p, opts)
		}
	}

	if nudge {
		clientset, err := collector.GetClientset()
		if err != nil {
			return fmt.Errorf("failed to get kubernetes clientset for active nudging: %w", err)
		}
		nudgeOpts := nudger.NewOptions()
		nudgeOpts.Live = envBoolOr("CLUSTER_OPTIMIZER_NUDGE_LIVE", false)
		if err := nudger.NudgePods(ctx, clientset, nudgeOpts); err != nil {
			return fmt.Errorf("active nudging failed: %w", err)
		}
	}

	return nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envBoolOr(key string, fallback bool) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return value
}

func renderText(report analyzer.Report) string {
	out := fmt.Sprintf("Cluster: %s\nGenerated: %s\n\nSummary:\n", report.ClusterID, report.GeneratedAt.Format(time.RFC3339))
	for key, value := range report.Summary {
		out += fmt.Sprintf("- %s: %v\n", key, value)
	}
	out += "\nFindings:\n"
	if len(report.Findings) == 0 {
		return out + "- No findings.\n"
	}
	for _, finding := range report.Findings {
		scope := finding.Workload
		if finding.Namespace != "" && scope != "" {
			scope = finding.Namespace + "/" + scope
		}
		if scope == "" {
			scope = "cluster"
		}
		out += fmt.Sprintf("- [%s] %s %s: %s\n", finding.Severity, scope, finding.RuleID, finding.Recommendation)
		out += fmt.Sprintf("  Evidence: %s\n  Risk: %s\n", finding.Evidence, finding.Risk)
	}
	return out
}
