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
	"github.com/GipsyChef/cluster-optimizer/internal/collector"
	"github.com/GipsyChef/cluster-optimizer/internal/nudger"
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
	flags := flag.NewFlagSet("cluster-optimizer", flag.ContinueOnError)
	flags.StringVar(&clusterID, "cluster-id", envOr("CLUSTER_OPTIMIZER_CLUSTER_ID", "default"), "stable cluster identifier")
	flags.StringVar(&output, "output", envOr("OUTPUT_FORMAT", "json"), "json or text")
	flags.DurationVar(&timeout, "timeout", 25*time.Second, "collection timeout")
	flags.BoolVar(&nudge, "nudge", envBoolOr("CLUSTER_OPTIMIZER_NUDGE", false), "actively nudge pods to run on fewer nodes")
	if err := flags.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	snapshot, err := collector.Collect(ctx, clusterID)
	if err != nil {
		return err
	}
	report := analyzer.Analyze(snapshot)

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

	if table := os.Getenv("DYNAMODB_TABLE"); table != "" {
		writer, err := store.NewDynamoDBWriter(ctx, table)
		if err != nil {
			return err
		}
		if err := writer.PutReport(ctx, report); err != nil {
			return err
		}
	}

	if nudge {
		clientset, err := collector.GetClientset()
		if err != nil {
			return fmt.Errorf("failed to get kubernetes clientset for active nudging: %w", err)
		}
		if err := nudger.NudgePods(ctx, clientset); err != nil {
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
