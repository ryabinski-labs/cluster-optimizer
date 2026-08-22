package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/GipsyChef/cluster-optimizer/internal/analyzer"
	"github.com/GipsyChef/cluster-optimizer/internal/applier"
	"github.com/GipsyChef/cluster-optimizer/internal/capacity"
	"github.com/GipsyChef/cluster-optimizer/internal/classifier"
	"github.com/GipsyChef/cluster-optimizer/internal/collector"
	"github.com/GipsyChef/cluster-optimizer/internal/model"
	"github.com/GipsyChef/cluster-optimizer/internal/nudger"
	"github.com/GipsyChef/cluster-optimizer/internal/plan"
	"github.com/GipsyChef/cluster-optimizer/internal/podgc"
	"github.com/GipsyChef/cluster-optimizer/internal/store"
	"github.com/GipsyChef/cluster-optimizer/internal/usage"
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
	var gcCompletedPods bool
	var gcNamespace string
	var gcMinAge time.Duration
	var gcMaxDeletions int
	var cordonTTL time.Duration
	var nodeFloor int
	var surviveNodeLoss bool
	var usageProvider string
	var prometheusURL string
	var usageLookback time.Duration
	flags := flag.NewFlagSet("cluster-optimizer", flag.ContinueOnError)
	flags.StringVar(&clusterID, "cluster-id", envOr("CLUSTER_OPTIMIZER_CLUSTER_ID", "default"), "stable cluster identifier")
	flags.StringVar(&output, "output", envOr("OUTPUT_FORMAT", "json"), "json or text")
	flags.DurationVar(&timeout, "timeout", 25*time.Second, "collection timeout")
	flags.BoolVar(&nudge, "nudge", envBoolOr("CLUSTER_OPTIMIZER_NUDGE", false), "actively nudge pods to run on fewer nodes (dry-run unless CLUSTER_OPTIMIZER_NUDGE_LIVE=true)")
	flags.BoolVar(&autoApply, "auto-apply", envBoolOr("CLUSTER_OPTIMIZER_AUTOAPPLY_FLAG", false), "request live in-cluster auto-apply (requires CLUSTER_OPTIMIZER_AUTOAPPLY=true env to actually mutate)")
	flags.StringVar(&targetsPath, "targets", envOr("CLUSTER_OPTIMIZER_TARGETS", "/etc/cluster-optimizer/remediation-targets.json"), "path to remediation-targets.json")
	flags.BoolVar(&gcCompletedPods, "gc-completed-pods", envBoolOr("CLUSTER_OPTIMIZER_GC_COMPLETED_PODS", false), "clean up completed (Succeeded/Failed) pods (dry-run unless CLUSTER_OPTIMIZER_GC_COMPLETED_PODS_LIVE=true)")
	flags.StringVar(&gcNamespace, "gc-namespace", envOr("CLUSTER_OPTIMIZER_GC_NAMESPACE", ""), "limit completed-pod cleanup to one namespace (default: all namespaces)")
	flags.DurationVar(&gcMinAge, "gc-min-age", envDurationOr("CLUSTER_OPTIMIZER_GC_MIN_AGE", 0), "only delete completed pods that finished at least this long ago (e.g. 1h)")
	flags.IntVar(&gcMaxDeletions, "gc-max-deletions", envIntOr("CLUSTER_OPTIMIZER_GC_MAX_DELETIONS", 0), "cap completed-pod deletions per run, oldest first (0 = no cap)")
	flags.DurationVar(&cordonTTL, "cordon-ttl", envDurationOr("CLUSTER_OPTIMIZER_CORDON_TTL", nudger.DefaultCordonTTL), "reverse a cordon this tool placed once it has stood this long with nothing acting on it (0 = never reap)")
	flags.IntVar(&nodeFloor, "node-floor", envIntOr("CLUSTER_OPTIMIZER_NODE_FLOOR", capacity.DefaultFloor), "smallest node count per pool the minimum-safe-node search may recommend")
	flags.BoolVar(&surviveNodeLoss, "survive-node-loss", envBoolOr("CLUSTER_OPTIMIZER_SURVIVE_NODE_LOSS", true), "require that the recommended node count still places every pod after losing one node")
	flags.StringVar(&usageProvider, "usage-provider", envOr("CLUSTER_OPTIMIZER_USAGE_PROVIDER", "auto"), "usage evidence source: auto, prometheus, or instant")
	flags.StringVar(&prometheusURL, "prometheus-url", envOr("CLUSTER_OPTIMIZER_PROMETHEUS_URL", ""), "base URL of a Prometheus-compatible API, e.g. http://prometheus.monitoring:9090")
	flags.DurationVar(&usageLookback, "usage-lookback", envDurationOr("CLUSTER_OPTIMIZER_USAGE_LOOKBACK", usage.DefaultLookback), "window for percentile usage queries (e.g. 168h for one week)")
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
	snapshot, err := collector.Collect(ctx, clusterID)
	if err != nil {
		return err
	}
	cls := classifier.NewWithNamespaceLabels(clusterID, targets, namespaceLabels(snapshot.Namespaces))
	// Usage evidence is resolved before analysis so the capacity search sizes
	// each pod by the more demanding of its request and its observed p95.
	// Resolve never fails: it degrades to a weaker source and records why, so
	// a cluster with no Prometheus still gets a report — one that is simply
	// marked non-actionable rather than silently treated as trustworthy.
	usageSet := usage.Resolve(ctx, usage.Config{
		Mode:          usageProvider,
		PrometheusURL: prometheusURL,
		Lookback:      usageLookback,
		PodKeys:       usage.PodKeys(snapshot.Pods),
	}, usage.InstantFromPods(snapshot.Pods))

	capacityResult := capacity.Analyze(snapshot, usageSet, capacity.Config{
		Floor:           nodeFloor,
		SurviveNodeLoss: surviveNodeLoss,
	})

	report := analyzer.AnalyzeWithOptions(snapshot, analyzer.Options{
		Classifier: cls,
		Capacity:   &capacityResult,
	})

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
	gcLive := envBoolOr("CLUSTER_OPTIMIZER_GC_COMPLETED_PODS_LIVE", false)

	status := store.EngineStatus{
		AutoApplyEnabled: autoApply,
		AutoApplyLive:    autoApply && autoApplyEnv,
		NudgeEnabled:     nudge,
		NudgeLive:        nudge && nudgeLive,
		GCEnabled:        gcCompletedPods,
		GCLive:           gcCompletedPods && gcLive,
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
		nudgeOpts.CordonTTL = cordonTTL
		nudgeResult, err := nudger.NudgePodsWithResult(ctx, clientset, nudgeOpts)
		if err != nil {
			return fmt.Errorf("active nudging failed: %w", err)
		}
		status.HaltActive = status.HaltActive || nudgeResult.Halted
		if nudgeResult.HaltReason != "" {
			status.HaltReason = nudgeResult.HaltReason
		}
		for _, event := range cordonReapEvents(nudgeResult, status.LastRunAt) {
			events = append(events, event)
			status.LastRunActions++
			if event.Applied {
				status.LastRunApplied++
			}
			if event.Error != "" {
				status.LastRunErrors++
			}
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

	if gcCompletedPods {
		clientset, err := collector.GetClientset()
		if err != nil {
			return fmt.Errorf("failed to get kubernetes clientset for completed-pod cleanup: %w", err)
		}
		gcOpts := podgc.NewOptions()
		gcOpts.Live = gcLive
		gcOpts.Namespace = gcNamespace
		gcOpts.MinAge = gcMinAge
		gcOpts.MaxDeletions = gcMaxDeletions
		gcResult, err := podgc.CleanCompletedPodsWithResult(ctx, clientset, gcOpts)
		if err != nil {
			return fmt.Errorf("completed-pod cleanup failed: %w", err)
		}
		status.HaltActive = status.HaltActive || gcResult.Halted
		if gcResult.HaltReason != "" {
			status.HaltReason = gcResult.HaltReason
		}
		if event, ok := podGCEvent(gcResult, status.LastRunAt); ok {
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

func namespaceLabels(namespaces []model.Namespace) map[string]map[string]string {
	labels := make(map[string]map[string]string, len(namespaces))
	for _, namespace := range namespaces {
		labels[namespace.Name] = namespace.Labels
	}
	return labels
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
		if cls.IsProviderManaged(skip.Namespace, skip.Workload) {
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

// cordonReapEvents turns the stale-cordon pass into audit rows — one per node
// whose cordon was reversed, plus one per failure.
//
// These get their own rows rather than folding into the consolidation event
// because they describe the opposite kind of act: the nudger removes capacity,
// the reaper gives it back. An operator scanning the feed for "why did this
// node come back" should find that answer stated plainly, and a run of these
// rows is the signal that drained nodes are never actually being removed.
func cordonReapEvents(result nudger.Result, ts time.Time) []store.RemediationEvent {
	var events []store.RemediationEvent
	for _, node := range result.Reap.Uncordoned {
		events = append(events, store.RemediationEvent{
			Timestamp:  ts,
			Mode:       result.Mode,
			Kind:       "uncordon_stale",
			TargetNode: node,
			Applied:    result.Mode == "live",
			Reason:     "cordon outlived its TTL with nothing acting on it; node returned to service",
		})
	}
	for _, msg := range result.Reap.Errors {
		events = append(events, store.RemediationEvent{
			Timestamp: ts,
			Mode:      result.Mode,
			Kind:      "uncordon_stale",
			Applied:   false,
			Error:     msg,
		})
	}
	return events
}

// podGCEvent collapses one completed-pod cleanup pass into a single audit row.
// ok=false covers the "engine ran but found nothing and wasn't halted" case so
// we don't flood the feed with empty rows on every CronJob tick.
func podGCEvent(result podgc.Result, ts time.Time) (store.RemediationEvent, bool) {
	if result.Candidates == 0 && !result.Halted {
		return store.RemediationEvent{}, false
	}
	reason := result.HaltReason
	if reason == "" && result.Mode == "dry-run" && result.Candidates > 0 {
		reason = fmt.Sprintf("%d completed pod(s) eligible for cleanup", result.Candidates)
	}
	return store.RemediationEvent{
		Timestamp:      ts,
		Mode:           result.Mode,
		Kind:           "delete_completed_pod",
		Namespace:      result.Namespace,
		Deleted:        result.Deleted,
		DeletionErrors: result.DeletionErrors,
		HaltActive:     result.Halted,
		Reason:         reason,
		Applied:        result.Mode == "live" && result.Deleted > 0 && result.DeletionErrors == 0,
	}, true
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

func envIntOr(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return value
}

func renderText(report analyzer.Report) string {
	out := fmt.Sprintf("Cluster: %s\nGenerated: %s\n\nSummary:\n", report.ClusterID, report.GeneratedAt.Format(time.RFC3339))
	// Summary keys are printed in a stable order so successive runs diff
	// cleanly; Go's map iteration would otherwise shuffle them every time.
	keys := make([]string, 0, len(report.Summary))
	for key := range report.Summary {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "capacity" {
			out += renderCapacity(report.Summary[key])
			continue
		}
		out += fmt.Sprintf("- %s: %v\n", key, report.Summary[key])
	}
	out += "\nFindings:\n"
	if len(report.Findings) == 0 {
		return out + "- No findings.\n"
	}
	return out + renderFindings(report.Findings)
}

// renderCapacity prints the minimum-safe-node verdict as a compact block
// rather than dumping the struct: the per-pool rows and their binding
// constraints are the part an operator acts on.
func renderCapacity(value any) string {
	result, ok := value.(*capacity.Result)
	if !ok {
		return fmt.Sprintf("- capacity: %v\n", value)
	}
	out := fmt.Sprintf("- capacity: %d nodes → %d minimum safe (%d releasable), evidence %s/%s\n",
		result.CurrentNodes, result.MinimumSafeNodes, result.RemovableNodes,
		result.UsageFidelity, orDash(result.UsageSource))
	if !result.Actionable {
		out += "    advisory only — not every pool has evidence strong enough to act on\n"
	}
	if result.UsageNote != "" {
		out += fmt.Sprintf("    note: %s\n", result.UsageNote)
	}
	for _, pool := range result.Pools {
		target := "—"
		if pool.Status != capacity.StatusIndeterminate {
			target = fmt.Sprintf("%d", pool.MinimumSafeNodes)
		}
		out += fmt.Sprintf("    %-24s %d → %-3s %-14s binding: %s\n",
			pool.Pool, pool.CurrentNodes, target, pool.Status, orDash(pool.BindingConstraint))
	}
	return out
}

func orDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

func renderFindings(findings []analyzer.Finding) string {
	var out string
	for _, finding := range findings {
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
