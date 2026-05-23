// Package plan turns analyzer findings into a concrete, auditable list of
// PlannedActions. Every executor (live applier, future PR emitter, nudger)
// consumes the same Plan, which makes "what would happen" inspectable in
// dry-run logs before any executor runs.
//
// Plan is pure: no I/O, no clientset. Build takes a Snapshot, findings, and
// a Classifier; emits a Plan. Filtering by confidence, evidence count, and
// provider_managed happens here so executors stay simple.
package plan

import (
	"fmt"

	"github.com/GipsyChef/cluster-optimizer/internal/analyzer"
	"github.com/GipsyChef/cluster-optimizer/internal/classifier"
	"github.com/GipsyChef/cluster-optimizer/internal/model"
)

// ActionKind enumerates the supported mutations. Each maps to a distinct
// safety policy and executor path.
type ActionKind string

const (
	// PatchRequest lowers a container's CPU and/or memory request on a
	// pod-template-bearing workload (Deployment, DaemonSet, StatefulSet).
	PatchRequest ActionKind = "patch_request"
)

// PlannedAction is a single concrete change. The executor reads this
// verbatim; the planner is responsible for safety, the executor is not.
type PlannedAction struct {
	Kind         ActionKind `json:"kind"`
	Namespace    string     `json:"namespace"`
	WorkloadKind string     `json:"workload_kind"`
	WorkloadName string     `json:"workload_name"`
	Container    string     `json:"container,omitempty"`
	// CurrentCPUm / NewCPUm: -1 means "do not change CPU". Memory same.
	CurrentCPUm     int64  `json:"current_cpu_m"`
	NewCPUm         int64  `json:"new_cpu_m"`
	CurrentMemMiB   int64  `json:"current_memory_mib"`
	NewMemMiB       int64  `json:"new_memory_mib"`
	FindingRuleID   string `json:"finding_rule_id"`
	Reason          string `json:"reason"`
	Confidence      string `json:"confidence"`
	OccurrenceCount int64  `json:"occurrence_count"`
}

// Plan is the full list of actions plus context about why some findings did
// not produce actions. Reasons is keyed by "RuleID|Namespace|Workload" and
// is intended for log/debug output.
type Plan struct {
	Actions []PlannedAction `json:"actions"`
	Skipped []SkippedReason `json:"skipped,omitempty"`
}

// SkippedReason documents a finding that the planner deliberately did not
// turn into an action. Surfaced in dry-run output so the operator can see
// why an opportunity was not pursued.
type SkippedReason struct {
	RuleID    string `json:"rule_id"`
	Namespace string `json:"namespace,omitempty"`
	Workload  string `json:"workload,omitempty"`
	Reason    string `json:"reason"`
}

// Policy controls how the planner filters findings into actions. All fields
// must have safe defaults; the zero value is the most conservative.
type Policy struct {
	// MinConfidence is the lowest analyzer confidence that may produce a
	// live action. "high" is the only safe default.
	MinConfidence string
	// MinOccurrences is the minimum number of consecutive runs a finding
	// must appear in before it produces a live action. 0 means
	// "persistence-based gating disabled" (will only act on confidence
	// alone, see RequirePersistence).
	MinOccurrences int64
	// RequirePersistence, when true, refuses to plan any live action unless
	// the caller supplied an Occurrences map (i.e. DynamoDB is wired up
	// and we can verify multi-run agreement).
	RequirePersistence bool
	// CPUFloorMilli is the absolute minimum CPU request the planner will
	// ever propose. Default 10m.
	CPUFloorMilli int64
	// MemoryFloorMiB is the absolute minimum memory request the planner
	// will ever propose. Default 32 MiB.
	MemoryFloorMiB int64
	// HeadroomNumerator/Denominator define the multiplier applied to
	// observed usage when proposing a new request: new = observed * N/D.
	// Defaults to 3/2 (50% headroom over observed).
	HeadroomNumerator   int64
	HeadroomDenominator int64
	// MaxFractionalTrim caps how much we will shrink a request in one
	// pass, expressed as numerator/denominator. Default 1/2 (no single
	// pass cuts more than 50%).
	MaxTrimNumerator   int64
	MaxTrimDenominator int64
	// MaxActions caps how many actions the planner will emit per run.
	// Defaults to 1 — small, observable blast radius per CronJob tick.
	MaxActions int
}

// DefaultPolicy returns the safest reasonable policy. Callers should start
// here and only widen it after deliberate review.
func DefaultPolicy() Policy {
	return Policy{
		MinConfidence:       "high",
		MinOccurrences:      3,
		RequirePersistence:  true,
		CPUFloorMilli:       10,
		MemoryFloorMiB:      32,
		HeadroomNumerator:   3,
		HeadroomDenominator: 2,
		MaxTrimNumerator:    1,
		MaxTrimDenominator:  2,
		MaxActions:          1,
	}
}

// confidenceRank lets us compare analyzer confidence strings; higher is
// stronger evidence.
var confidenceRank = map[string]int{"low": 1, "medium": 2, "high": 3}

// Build assembles a Plan from the report's findings using the supplied
// snapshot for current resource values and the classifier for safety
// checks. The occurrences map is keyed by analyzer recommendation key
// (RuleID + "\x00" + Namespace + "\x00" + Workload) and is the same format
// the persistence layer uses. Pass nil if persistence is not wired up.
func Build(report analyzer.Report, snapshot model.Snapshot, c *classifier.Classifier, policy Policy, occurrences map[string]int64) Plan {
	policy = applyDefaults(policy)
	wantConf := confidenceRank[policy.MinConfidence]
	plan := Plan{}
	workloads := indexWorkloads(snapshot.Workloads)
	for _, finding := range report.Findings {
		switch finding.RuleID {
		case "memory-request-over-provisioned", "cpu-request-over-provisioned":
			// Trim candidates.
		default:
			continue
		}
		if c == nil || c.IsProviderManaged(finding.Namespace, finding.Workload) {
			plan.Skipped = append(plan.Skipped, skipped(finding, "workload is provider-managed"))
			continue
		}
		target, hasTarget := c.TargetFor(finding.Namespace, finding.Workload)
		if !hasTarget || !c.IsRemediable(finding.RuleID, finding.Namespace, finding.Workload) {
			plan.Skipped = append(plan.Skipped, skipped(finding, "no remediation target supports this rule"))
			continue
		}
		if target.Container == "" {
			plan.Skipped = append(plan.Skipped, skipped(finding, "remediation target is missing container name"))
			continue
		}
		if confidenceRank[finding.Confidence] < wantConf {
			plan.Skipped = append(plan.Skipped, skipped(finding,
				fmt.Sprintf("confidence %q below minimum %q", finding.Confidence, policy.MinConfidence)))
			continue
		}
		var occCount int64
		if occurrences != nil {
			occCount = occurrences[occurrenceKey(finding)]
		}
		if policy.RequirePersistence && occurrences == nil {
			plan.Skipped = append(plan.Skipped, skipped(finding, "persistence is required but not configured"))
			continue
		}
		if occCount < policy.MinOccurrences {
			plan.Skipped = append(plan.Skipped, skipped(finding,
				fmt.Sprintf("seen %d times, need %d", occCount, policy.MinOccurrences)))
			continue
		}
		workload, ok := workloads[finding.Namespace+"/"+finding.Workload]
		if !ok {
			plan.Skipped = append(plan.Skipped, skipped(finding, "workload not found in snapshot"))
			continue
		}
		action, ok := planRequestTrim(workload, finding, target, policy)
		if !ok {
			plan.Skipped = append(plan.Skipped, skipped(finding, "no safe trim available"))
			continue
		}
		action.OccurrenceCount = occCount
		plan.Actions = append(plan.Actions, action)
		if policy.MaxActions > 0 && len(plan.Actions) >= policy.MaxActions {
			break
		}
	}
	return plan
}

func planRequestTrim(workload model.Workload, finding analyzer.Finding, target classifier.Target, policy Policy) (PlannedAction, bool) {
	action := PlannedAction{
		Kind:          PatchRequest,
		Namespace:     workload.Namespace,
		WorkloadKind:  workload.Kind,
		WorkloadName:  workload.Name,
		Container:     target.Container,
		CurrentCPUm:   -1,
		NewCPUm:       -1,
		CurrentMemMiB: -1,
		NewMemMiB:     -1,
		FindingRuleID: finding.RuleID,
		Confidence:    finding.Confidence,
		Reason:        finding.Evidence,
	}
	replicas := int64(workload.Replicas)
	if replicas < 1 {
		return action, false
	}
	switch finding.RuleID {
	case "memory-request-over-provisioned":
		if workload.UsageMemoryMiB == nil || workload.RequestsMemoryMiB <= 0 {
			return action, false
		}
		usagePer := *workload.UsageMemoryMiB / replicas
		currentPer := workload.RequestsMemoryMiB / replicas
		proposed := proposeRequest(usagePer, currentPer, policy.HeadroomNumerator, policy.HeadroomDenominator,
			policy.MemoryFloorMiB, policy.MaxTrimNumerator, policy.MaxTrimDenominator)
		if proposed <= 0 || proposed >= currentPer {
			return action, false
		}
		action.CurrentMemMiB = currentPer
		action.NewMemMiB = proposed
		return action, true
	case "cpu-request-over-provisioned":
		if workload.UsageCPUm == nil || workload.RequestsCPUm <= 0 {
			return action, false
		}
		usagePer := *workload.UsageCPUm / replicas
		currentPer := workload.RequestsCPUm / replicas
		proposed := proposeRequest(usagePer, currentPer, policy.HeadroomNumerator, policy.HeadroomDenominator,
			policy.CPUFloorMilli, policy.MaxTrimNumerator, policy.MaxTrimDenominator)
		if proposed <= 0 || proposed >= currentPer {
			return action, false
		}
		action.CurrentCPUm = currentPer
		action.NewCPUm = proposed
		return action, true
	}
	return action, false
}

// proposeRequest computes a safe new request value: max(floor,
// usage*headroom), then bounded so the trim does not exceed maxTrim of the
// current value. Returns 0 if no safe trim is possible.
func proposeRequest(usage, current, headroomNum, headroomDen, floor, maxTrimNum, maxTrimDen int64) int64 {
	if usage < 0 || current <= 0 {
		return 0
	}
	target := usage * headroomNum / headroomDen
	if target < floor {
		target = floor
	}
	if target >= current {
		return 0
	}
	maxReduction := current * maxTrimNum / maxTrimDen
	minAllowed := current - maxReduction
	if target < minAllowed {
		target = minAllowed
	}
	if target < floor {
		target = floor
	}
	if target >= current {
		return 0
	}
	return target
}

func applyDefaults(p Policy) Policy {
	if p.MinConfidence == "" {
		p.MinConfidence = "high"
	}
	if p.CPUFloorMilli <= 0 {
		p.CPUFloorMilli = 10
	}
	if p.MemoryFloorMiB <= 0 {
		p.MemoryFloorMiB = 32
	}
	if p.HeadroomNumerator <= 0 {
		p.HeadroomNumerator = 3
	}
	if p.HeadroomDenominator <= 0 {
		p.HeadroomDenominator = 2
	}
	if p.MaxTrimNumerator <= 0 {
		p.MaxTrimNumerator = 1
	}
	if p.MaxTrimDenominator <= 0 {
		p.MaxTrimDenominator = 2
	}
	if p.MaxActions <= 0 {
		p.MaxActions = 1
	}
	return p
}

func indexWorkloads(workloads []model.Workload) map[string]model.Workload {
	out := make(map[string]model.Workload, len(workloads))
	for _, w := range workloads {
		out[w.Namespace+"/"+w.Kind+"/"+w.Name] = w
	}
	return out
}

// occurrenceKey matches the format the persistence layer uses for
// recommendation rows.
func occurrenceKey(f analyzer.Finding) string {
	return f.RuleID + "\x00" + f.Namespace + "\x00" + f.Workload
}

func skipped(f analyzer.Finding, reason string) SkippedReason {
	return SkippedReason{RuleID: f.RuleID, Namespace: f.Namespace, Workload: f.Workload, Reason: reason}
}
