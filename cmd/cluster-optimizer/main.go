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
	var writer *store.DynamoDBWriter
	if table := os.Getenv("DYNAMODB_TABLE"); table != "" {
		var werr error
		writer, werr = store.NewDynamoDBWriter(ctx, table)
		if werr != nil {
			return werr
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
	autoApplyEnv := envBoolOr("CLUSTER_OPTIMIZER_AUTOAPPLY", false)
	nudgeLive := envBoolOr("CLUSTER_OPTIMIZER_NUDGE_LIVE", false)

	status := store.EngineStatus{
		AutoApplyEnabled: autoApply,
		AutoApplyLive:    autoApply && autoApplyEnv,
		NudgeEnabled:     nudge,
		NudgeLive:        nudge && nudgeLive,
		LastRunAt:        time.Now().UTC(),
		LastClusterID:    clusterID,
	}
	var events []store.RemediationEvent

	if len(p.Actions) > 0 {
		clientset, clientErr := collector.GetClientset()
		if clientErr != nil {
			fmt.Fprintf(os.Stderr, "cluster-optimizer: cannot build clientset for applier: %v\n", clientErr)
		} else {
			opts := applier.NewOptions()
			opts.AutoApply = autoApply
			opts.AutoApplyEnvSet = autoApplyEnv
			result := applier.Apply(ctx, clientset, p, opts)
			status.HaltActive = status.HaltActive || result.Halted
			events = append(events, applierEvents(result, status.LastRunAt)...)
			status.LastRunActions += len(result.Outcomes)
			for _, outcome := range result.Outcomes {
				if outcome.Applied {
					status.LastRunApplied++
				}
				if outcome.Error != "" {
					status.LastRunErrors++
				}
			}
		}
	}

	if nudge {
		clientset, err := collector.GetClientset()
		if err != nil {
			return fmt.Errorf("failed to get kubernetes clientset for active nudging: %w", err)
		}
		nudgeOpts := nudger.NewOptions()
		nudgeOpts.Live = nudgeLive
		nudgeResult, err := nudger.NudgePodsWithResult(ctx, clientset, nudgeOpts)
		if err != nil {
			return fmt.Errorf("active nudging failed: %w", err)
		}
		status.HaltActive = status.HaltActive || nudgeResult.Halted
		if nudgeResult.HaltReason != "" {
			status.HaltReason = nudgeResult.HaltReason
		}
		if event, ok := nudgerEvent(nudgeResult, status.LastRunAt); ok {
			events = append(events, event)
			status.LastRunActions++
			if event.Applied {
				status.LastRunApplied++
			}
			if event.Error != "" {
				status.LastRunErrors++
			}
		}
	}

	// Planner skips become first-class audit events when the workload is
	// in the remediation allowlist. This is what answers "why didn't you
	// patch X?" on the dashboard without log diving.
	events = append(events, skipperEvents(p.Skipped, status.LastRunAt, cls)...)

	if writer != nil {
		if err := writer.PutRemediations(ctx, clusterID, events); err != nil {
			fmt.Fprintf(os.Stderr, "cluster-optimizer: failed to persist remediation events: %v\n", err)
		}
		if err := writer.PutEngineStatus(ctx, clusterID, status); err != nil {
			fmt.Fprintf(os.Stderr, "cluster-optimizer: failed to persist engine status: %v\n", err)
		}
	}

	return nil
}

// skipperEvents converts planner SkippedReasons into RemediationEvents the UI
// can render. We only emit skips for workloads in the remediation allowlist
// to avoid flooding the feed: every CronJob tick would otherwise emit a skip
// for every system/provider-managed finding in the cluster.
func skipperEvents(skipped []plan.SkippedReason, ts time.Time, cls *classifier.Classifier) []store.RemediationEvent {
	events := make([]store.RemediationEvent, 0, len(skipped))
	for _, skip := range skipped {
		if cls == nil {
			continue
		}
		if _, hasTarget := cls.TargetFor(skip.Namespace, skip.Workload); !hasTarget {
			continue
		}
		events = append(events, store.RemediationEvent{
			Timestamp: ts,
			Kind:      "skip",
			Namespace: skip.Namespace,
			Workload:  skip.Workload,
			RuleID:    skip.RuleID,
			Reason:    skip.Reason,
		})
	}
	return events
}

// applierEvents converts the applier outcome list into RemediationEvents the
// UI can render. before/after values are 0 when a side (cpu or mem) wasn't
// touched — the JSON omitempty tag drops the noise.
func applierEvents(result applier.Result, ts time.Time) []store.RemediationEvent {
	events := make([]store.RemediationEvent, 0, len(result.Outcomes))
	mode := "live"
	if result.DryRun {
		mode = "dry-run"
	}
	for _, outcome := range result.Outcomes {
		event := store.RemediationEvent{
			Timestamp:       ts,
			Mode:            mode,
			Kind:            "patch_request",
			Namespace:       outcome.Action.Namespace,
			Workload:        outcome.Action.WorkloadName,
			WorkloadKind:    outcome.Action.WorkloadKind,
			Container:       outcome.Action.Container,
			RuleID:          outcome.Action.FindingRuleID,
			Applied:         outcome.Applied,
			Reason:          outcome.Reason,
			Error:           outcome.Error,
			HaltActive:      result.Halted,
			OccurrenceCount: outcome.Action.OccurrenceCount,
		}
		if outcome.Action.NewCPUm > 0 {
			event.BeforeCPUm = outcome.Action.CurrentCPUm
			event.AfterCPUm = outcome.Action.NewCPUm
		}
		if outcome.Action.NewMemMiB > 0 {
			event.BeforeMemMiB = outcome.Action.CurrentMemMiB
			event.AfterMemMiB = outcome.Action.NewMemMiB
		}
		events = append(events, event)
	}
	return events
}

// nudgerEvent collapses one consolidation pass into a single audit row. The
// ok=false return covers the "engine ran but nothing to report" case
// (cluster too small, no target found) so we don't flood the feed with
// empty rows on every CronJob tick.
func nudgerEvent(result nudger.Result, ts time.Time) (store.RemediationEvent, bool) {
	if result.TargetNode == "" && !result.Halted && result.NotFeasibleReason == "" {
		return store.RemediationEvent{}, false
	}
	reason := result.NotFeasibleReason
	if result.Halted && result.HaltReason != "" {
		reason = result.HaltReason
	}
	event := store.RemediationEvent{
		Timestamp:      ts,
		Mode:           result.Mode,
		Kind:           "cordon_evict",
		TargetNode:     result.TargetNode,
		Evicted:        result.Evicted,
		EvictionErrors: result.EvictionErrors,
		HaltActive:     result.Halted,
		Reason:         reason,
		Applied:        result.Mode == "live" && result.TargetNode != "" && result.EvictionErrors == 0,
	}
	return event, true
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
