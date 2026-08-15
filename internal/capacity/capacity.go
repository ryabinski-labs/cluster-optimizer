// Package capacity answers "how few nodes can this cluster safely run on?"
//
// It replaces a fixed two-node target that was, in the end, a constant: the
// old estimate compared requests against twice the cluster-average node,
// which reported the same number for every cluster, described an averaged
// node that exists in no heterogeneous cluster, and could not see a single
// scheduling constraint. A pool of three memory-optimised nodes pinned by
// anti-affinity got the same "2 nodes" verdict as an idle pool of ten.
//
// Here the number is derived per node pool, by searching for the smallest N
// whose placement actually simulates, and the answer is allowed to be "I do
// not know".
package capacity

import (
	"fmt"
	"sort"
	"strings"

	"github.com/GipsyChef/cluster-optimizer/internal/binpack"
	"github.com/GipsyChef/cluster-optimizer/internal/model"
	"github.com/GipsyChef/cluster-optimizer/internal/usage"
)

// DefaultFloor is the smallest node count the engine will recommend unless an
// operator lowers it. Two nodes is the smallest count that survives losing one.
const DefaultFloor = 2

// DefaultHeadroomPercent is the share of each retained node's allocatable
// capacity held back from the packing simulation. Packing a node to exactly
// 100% of requests leaves nothing for the next rolling update, so a plan that
// only works at full density is not a plan.
const DefaultHeadroomPercent = 10

// DefaultUsageHeadroomPercent is added on top of observed p95 usage before it
// is compared against a pod's request.
const DefaultUsageHeadroomPercent = 20

// Status is a pool's verdict.
type Status string

const (
	// StatusFits means the pool can run on fewer nodes than it has.
	StatusFits Status = "fits"
	// StatusAtMinimum means the workload genuinely needs every node.
	StatusAtMinimum Status = "at_minimum"
	// StatusAtFloor means the workload would fit on fewer nodes but the
	// configured floor stops there.
	StatusAtFloor Status = "at_floor"
	// StatusIndeterminate means a constraint could not be evaluated, so no
	// claim is made. Enforcement must treat this as a refusal, not a maybe.
	StatusIndeterminate Status = "indeterminate"
)

// Config tunes the search.
type Config struct {
	// Floor is the smallest node count the search may recommend. It can only
	// make the answer safer: the derived minimum is never lowered to meet it.
	Floor int
	// SurviveNodeLoss requires that the recommended N still places the
	// workload after losing one node. This is the difference between the
	// minimum and the minimum *safe* node count.
	SurviveNodeLoss bool
	// HeadroomPercent is withheld from each node's allocatable capacity.
	HeadroomPercent int
	// UsageHeadroomPercent is added to observed usage before comparing it
	// against requests.
	UsageHeadroomPercent int
}

func (c Config) floor() int {
	if c.Floor > 0 {
		return c.Floor
	}
	return DefaultFloor
}

func (c Config) headroom() int {
	if c.HeadroomPercent > 0 {
		return c.HeadroomPercent
	}
	return DefaultHeadroomPercent
}

func (c Config) usageHeadroom() int {
	if c.UsageHeadroomPercent > 0 {
		return c.UsageHeadroomPercent
	}
	return DefaultUsageHeadroomPercent
}

// Blocker is one reason a pool cannot shrink further, aggregated across the
// pods it affects so the UI can rank causes rather than list symptoms.
type Blocker struct {
	// Reason is a binpack predicate name.
	Reason string `json:"reason"`
	// Pods is how many pods this predicate blocked.
	Pods int `json:"pods"`
	// Examples names a few affected pods.
	Examples []string `json:"examples,omitempty"`
}

// PoolVerdict is the result for one node pool.
type PoolVerdict struct {
	Pool         string `json:"pool"`
	CurrentNodes int    `json:"current_nodes"`
	// MinimumSafeNodes is the recommendation. Zero when indeterminate.
	MinimumSafeNodes int    `json:"minimum_safe_nodes,omitempty"`
	RemovableNodes   int    `json:"removable_nodes"`
	Status           Status `json:"status"`
	// DerivedMinimum is what the simulation found before the configured
	// floor was applied. Exposing both is what lets the UI say "the floor is
	// what is stopping this" rather than implying the workload needs it.
	DerivedMinimum  int `json:"derived_minimum,omitempty"`
	ConfiguredFloor int `json:"configured_floor"`
	// BindingConstraint names what stops the pool going one node lower.
	BindingConstraint string    `json:"binding_constraint,omitempty"`
	Blockers          []Blocker `json:"blockers,omitempty"`
	// IndeterminateReasons lists unmodelled constraints found in the pool.
	IndeterminateReasons []string `json:"indeterminate_reasons,omitempty"`
	// ImmovableNodes is how many nodes host a pod that cannot be evicted at
	// all (bare or mirror pods); those nodes can never be drained.
	ImmovableNodes int `json:"immovable_nodes,omitempty"`
	// UsageFidelity records the evidence class behind the pod footprints.
	UsageFidelity usage.Fidelity `json:"usage_fidelity"`
	// Actionable is true only when this verdict is safe to enforce against.
	Actionable bool `json:"actionable"`
}

// Result is the whole-cluster answer.
type Result struct {
	Pools            []PoolVerdict  `json:"pools"`
	CurrentNodes     int            `json:"current_nodes"`
	MinimumSafeNodes int            `json:"minimum_safe_nodes"`
	RemovableNodes   int            `json:"removable_nodes"`
	UsageFidelity    usage.Fidelity `json:"usage_fidelity"`
	UsageSource      string         `json:"usage_source,omitempty"`
	UsageNote        string         `json:"usage_note,omitempty"`
	// SurviveNodeLoss records whether the recommendation was required to still
	// place the workload after losing a node. It travels with the result
	// because a consumer showing the number must not claim a safety property
	// the search was never asked to verify.
	SurviveNodeLoss bool `json:"survive_node_loss"`
	// Actionable is true only when every pool is actionable. One
	// indeterminate pool makes the cluster figure a report, not a plan.
	Actionable bool `json:"actionable"`
}

// Analyze runs the per-pool minimum-N search over a snapshot.
func Analyze(snapshot model.Snapshot, usageSet usage.Set, cfg Config) Result {
	result := Result{
		UsageFidelity:   usageSet.Fidelity,
		UsageSource:     usageSet.Source,
		UsageNote:       usageSet.Err,
		SurviveNodeLoss: cfg.SurviveNodeLoss,
		Actionable:      true,
	}
	if result.UsageFidelity == "" {
		result.UsageFidelity = usage.FidelityNone
	}

	byPool := map[string][]model.Node{}
	for _, node := range snapshot.Nodes {
		byPool[node.Pool] = append(byPool[node.Pool], node)
	}
	podsByNode := map[string][]model.Pod{}
	for _, pod := range snapshot.Pods {
		if !pod.Active() || pod.NodeName == "" {
			continue
		}
		podsByNode[pod.NodeName] = append(podsByNode[pod.NodeName], pod)
	}

	poolNames := make([]string, 0, len(byPool))
	for name := range byPool {
		poolNames = append(poolNames, name)
	}
	sort.Strings(poolNames)

	for _, name := range poolNames {
		verdict := analyzePool(name, byPool[name], podsByNode, usageSet, cfg)
		result.Pools = append(result.Pools, verdict)
		result.CurrentNodes += verdict.CurrentNodes
		result.MinimumSafeNodes += verdict.MinimumSafeNodes
		result.RemovableNodes += verdict.RemovableNodes
		if !verdict.Actionable {
			result.Actionable = false
		}
	}
	// An indeterminate pool contributes its current count to the cluster
	// minimum: we cannot claim it can shrink, so we must assume it cannot.
	for _, verdict := range result.Pools {
		if verdict.Status == StatusIndeterminate {
			result.MinimumSafeNodes += verdict.CurrentNodes
		}
	}
	return result
}

func analyzePool(name string, nodes []model.Node, podsByNode map[string][]model.Pod, usageSet usage.Set, cfg Config) PoolVerdict {
	verdict := PoolVerdict{
		Pool:            name,
		CurrentNodes:    len(nodes),
		ConfiguredFloor: cfg.floor(),
		UsageFidelity:   usageSet.Fidelity,
	}
	if verdict.UsageFidelity == "" {
		verdict.UsageFidelity = usage.FidelityNone
	}

	// Sort nodes so the retained subset is deterministic and represents the
	// best case for a given N: keep the largest nodes, and always keep nodes
	// that host something immovable.
	sorted := append([]model.Node(nil), nodes...)
	immovableHosts := map[string]bool{}
	for _, node := range nodes {
		for _, pod := range podsByNode[node.Name] {
			if !pod.Relocatable() && pod.OwnerKind != "DaemonSet" {
				immovableHosts[node.Name] = true
				break
			}
		}
	}
	verdict.ImmovableNodes = len(immovableHosts)
	sort.SliceStable(sorted, func(i, j int) bool {
		if immovableHosts[sorted[i].Name] != immovableHosts[sorted[j].Name] {
			return immovableHosts[sorted[i].Name]
		}
		if sorted[i].AllocatableMemoryMiB != sorted[j].AllocatableMemoryMiB {
			return sorted[i].AllocatableMemoryMiB > sorted[j].AllocatableMemoryMiB
		}
		if sorted[i].AllocatableCPUm != sorted[j].AllocatableCPUm {
			return sorted[i].AllocatableCPUm > sorted[j].AllocatableCPUm
		}
		return sorted[i].Name < sorted[j].Name
	})

	// Pods to re-place: everything relocatable currently running in the pool.
	// DaemonSet pods are not placed — they follow nodes — but their footprint
	// is charged to every retained node as reserved capacity.
	var toPlace []binpack.Pod
	var dsCPU, dsMem, dsPods int64
	// Bare and mirror pods cannot move, so they are not part of the placement
	// problem — but they do occupy their host. Charging them as reserved
	// capacity on that specific node is what stops the simulator packing
	// relocatable pods into space that is already taken; leaving them out
	// entirely makes a pinned node look empty, which is the one error
	// direction this engine must never make.
	pinnedCPU := map[string]int64{}
	pinnedMem := map[string]int64{}
	pinnedPods := map[string]int64{}
	for _, node := range nodes {
		var nodeDSCPU, nodeDSMem, nodeDSPods int64
		for _, pod := range podsByNode[node.Name] {
			if pod.OwnerKind == "DaemonSet" {
				nodeDSCPU += pod.RequestsCPUm
				nodeDSMem += pod.RequestsMemoryMiB
				nodeDSPods++
				continue
			}
			if !pod.Relocatable() {
				cpu, mem := usage.Effective(pod.RequestsCPUm, pod.RequestsMemoryMiB,
					readingFor(usageSet, pod), usageSet.Fidelity, cfg.usageHeadroom())
				pinnedCPU[node.Name] += cpu
				pinnedMem[node.Name] += mem
				pinnedPods[node.Name]++
				continue
			}
			toPlace = append(toPlace, simPod(pod, usageSet, cfg))
		}
		// Take the worst per-node DaemonSet footprint in the pool: a node
		// currently missing a DaemonSet will get it eventually.
		if nodeDSCPU > dsCPU {
			dsCPU = nodeDSCPU
		}
		if nodeDSMem > dsMem {
			dsMem = nodeDSMem
		}
		if nodeDSPods > dsPods {
			dsPods = nodeDSPods
		}
	}

	// Only a schedulable node can receive pods: the scheduler will not place
	// on a cordoned or NotReady node, so its allocatable figure is not
	// capacity. Such a node is still counted in the pool, and if something
	// immovable is stranded on it the pool cannot give it back either.
	var placeable []model.Node
	var strandedNodes int
	for _, n := range sorted {
		if n.Schedulable() {
			placeable = append(placeable, n)
			continue
		}
		if immovableHosts[n.Name] {
			strandedNodes++
		}
	}

	buildNodes := func(retained []model.Node) []binpack.Node {
		out := make([]binpack.Node, 0, len(retained))
		for _, n := range retained {
			out = append(out, binpack.Node{
				Name:   n.Name,
				Pool:   n.Pool,
				Zone:   n.Zone,
				Labels: n.Labels,
				Taints: n.Taints,
				// Withhold headroom from allocatable so the plan does not
				// depend on packing every node to the last megabyte.
				AllocatableCPUm:      withhold(n.AllocatableCPUm, cfg.headroom()),
				AllocatableMemoryMiB: withhold(n.AllocatableMemoryMiB, cfg.headroom()),
				AllocatableExtended:  n.AllocatableExtended,
				MaxPods:              n.MaxPods,
				ReservedCPUm:         dsCPU + pinnedCPU[n.Name],
				ReservedMemoryMiB:    dsMem + pinnedMem[n.Name],
				ReservedPods:         dsPods + pinnedPods[n.Name],
			})
		}
		return out
	}

	fitNodes := func(retained []model.Node) binpack.Placement {
		if len(retained) == 0 {
			if len(toPlace) == 0 {
				return binpack.Placement{Assignments: map[string]string{}}
			}
			return binpack.Fit(nil, toPlace)
		}
		return binpack.Fit(buildNodes(retained), toPlace)
	}

	// Search upward for the smallest N that works. Starting at 1 rather than
	// at the configured floor is what lets the verdict distinguish "the
	// workload needs this many" from "the floor stops us here".
	derived := 0
	var indeterminate []string
	for n := 1; n <= len(placeable); n++ {
		retained := placeable[:n]
		placement := fitNodes(retained)
		if placement.Indeterminate {
			indeterminate = placement.IndeterminateReasons
			break
		}
		if len(placement.Unplaced) > 0 {
			continue
		}
		if cfg.SurviveNodeLoss {
			// The pool must still place everything after losing one node.
			// This is the whole difference between a minimum and a safe one.
			survivor := fitNodes(withoutWorstNode(retained))
			if survivor.Indeterminate {
				indeterminate = survivor.IndeterminateReasons
				break
			}
			if len(survivor.Unplaced) > 0 {
				continue
			}
		}
		derived = n
		break
	}

	if len(indeterminate) > 0 {
		verdict.Status = StatusIndeterminate
		verdict.IndeterminateReasons = indeterminate
		verdict.BindingConstraint = "unmodelled constraint: " + strings.Join(indeterminate, ", ")
		verdict.Actionable = false
		return verdict
	}

	if derived == 0 {
		// Nothing below the current count worked; the pool keeps what it has.
		derived = len(sorted)
	} else {
		// The search only ever shrinks the schedulable set. Nodes stranded by
		// an immovable pod on unusable capacity cannot be handed back.
		derived += strandedNodes
	}
	verdict.DerivedMinimum = derived
	verdict.MinimumSafeNodes = derived
	if cfg.floor() > verdict.MinimumSafeNodes {
		verdict.MinimumSafeNodes = cfg.floor()
	}
	if verdict.MinimumSafeNodes > verdict.CurrentNodes {
		// The pool is already below its own floor; do not invent nodes.
		verdict.MinimumSafeNodes = verdict.CurrentNodes
	}
	verdict.RemovableNodes = verdict.CurrentNodes - verdict.MinimumSafeNodes

	switch {
	case verdict.RemovableNodes > 0:
		verdict.Status = StatusFits
	case verdict.DerivedMinimum < verdict.CurrentNodes:
		// The workload would fit smaller; the floor is what stops it.
		verdict.Status = StatusAtFloor
		verdict.BindingConstraint = fmt.Sprintf("configured floor of %d nodes", cfg.floor())
	default:
		verdict.Status = StatusAtMinimum
	}

	// Describe what stops the pool going one node lower than recommended.
	//
	// The probe is re-run rather than reusing whatever the search last
	// rejected: the search also probes N-1 for the survival gate, and a
	// failure there says "zero nodes cannot host pods", which is an artifact
	// of the question, not a constraint anyone can act on.
	if verdict.Status != StatusAtFloor {
		// Express the probe in terms of the schedulable set the search worked
		// over, since that is the only part of the pool a smaller N can come
		// out of.
		probeCount := verdict.MinimumSafeNodes - 1 - strandedNodes
		if probeCount > len(placeable) {
			probeCount = len(placeable)
		}
		if probeCount <= 0 {
			verdict.BindingConstraint = lowerBoundReason(cfg)
		} else {
			probe := fitNodes(placeable[:probeCount])
			switch {
			case probe.Indeterminate:
				verdict.BindingConstraint = "unmodelled constraint below this size"
			case len(probe.Unplaced) == 0:
				// The workload would pack into one fewer node. Something
				// other than capacity is holding the line — name it, rather
				// than reporting no blocker at all.
				verdict.BindingConstraint = lowerBoundReason(cfg)
			default:
				verdict.Blockers = summarizeBlockers(probe)
				if len(verdict.Blockers) > 0 {
					verdict.BindingConstraint = verdict.Blockers[0].Reason
				}
			}
		}
	}

	// A verdict is only safe to act on when the usage evidence behind the pod
	// footprints spans time. A single live sample cannot distinguish an idle
	// workload from one caught between bursts.
	verdict.Actionable = verdict.Status != StatusIndeterminate && verdict.UsageFidelity.Actionable()
	return verdict
}

// lowerBoundReason names what holds a pool at its recommended size when the
// workload itself would fit into fewer nodes.
func lowerBoundReason(cfg Config) string {
	if cfg.SurviveNodeLoss {
		return "must survive losing one node"
	}
	return fmt.Sprintf("configured floor of %d nodes", cfg.floor())
}

// withoutWorstNode returns the retained set minus the node whose loss costs
// the most capacity.
//
// Which node is dropped is the whole substance of the survive-one-loss gate.
// Dropping the last entry of a largest-first list tests losing the *smallest*
// node, which in a heterogeneous pool proves nothing: a pool held up by one
// large node passes a gate that never asks what happens when that node dies.
//
// Testing the single most damaging loss is not a proof for every predicate —
// a topology constraint could in principle bind on a different node — but it
// is the loss that decides the capacity question, and it is strictly the
// pessimistic choice against the alternative.
func withoutWorstNode(retained []model.Node) []model.Node {
	if len(retained) <= 1 {
		return nil
	}
	worst := 0
	for i := 1; i < len(retained); i++ {
		switch {
		case retained[i].AllocatableMemoryMiB != retained[worst].AllocatableMemoryMiB:
			if retained[i].AllocatableMemoryMiB > retained[worst].AllocatableMemoryMiB {
				worst = i
			}
		case retained[i].AllocatableCPUm > retained[worst].AllocatableCPUm:
			worst = i
		}
	}
	out := make([]model.Node, 0, len(retained)-1)
	out = append(out, retained[:worst]...)
	out = append(out, retained[worst+1:]...)
	return out
}

// readingFor returns the observed usage for a pod, if any.
func readingFor(usageSet usage.Set, pod model.Pod) usage.Reading {
	reading, _ := usageSet.Get(pod.Namespace, pod.Name)
	return reading
}

// simPod converts a model pod into a placement unit, sizing it by the more
// demanding of its request and its observed usage.
func simPod(pod model.Pod, usageSet usage.Set, cfg Config) binpack.Pod {
	reading := readingFor(usageSet, pod)
	cpu, mem := usage.Effective(pod.RequestsCPUm, pod.RequestsMemoryMiB, reading, usageSet.Fidelity, cfg.usageHeadroom())
	return binpack.Pod{
		Namespace:      pod.Namespace,
		Name:           pod.Name,
		Labels:         pod.Labels,
		CPUm:           cpu,
		MemoryMiB:      mem,
		Extended:       pod.RequestsExtended,
		NodeSelector:   pod.NodeSelector,
		Tolerations:    pod.Tolerations,
		Affinity:       pod.Affinity,
		TopologySpread: pod.TopologySpread,
		Unmodelled:     pod.Unmodelled,
	}
}

// withhold reduces a capacity figure by a percentage.
func withhold(value int64, percent int) int64 {
	if percent <= 0 || percent >= 100 {
		return value
	}
	return value * int64(100-percent) / 100
}

// summarizeBlockers groups rejections by predicate, most-blocking first, so
// the UI can name the cause rather than list every affected pod.
func summarizeBlockers(placement binpack.Placement) []Blocker {
	if len(placement.Unplaced) == 0 {
		return nil
	}
	byReason := map[string]*Blocker{}
	for _, rejection := range placement.Unplaced {
		b, ok := byReason[rejection.Reason]
		if !ok {
			b = &Blocker{Reason: rejection.Reason}
			byReason[rejection.Reason] = b
		}
		b.Pods++
		if len(b.Examples) < 3 {
			b.Examples = append(b.Examples, rejection.PodKey)
		}
	}
	out := make([]Blocker, 0, len(byReason))
	for _, b := range byReason {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pods != out[j].Pods {
			return out[i].Pods > out[j].Pods
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}
