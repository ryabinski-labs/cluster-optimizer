// Package binpack answers one question: if these pods had to be placed on
// these nodes, could they be?
//
// It is a placement *simulation*, not a reimplementation of kube-scheduler.
// It evaluates the hard predicates that can make a placement impossible —
// resources, taints, node selectors, required node affinity, required pod
// anti-affinity, DoNotSchedule topology spread — and ignores everything that
// only influences which of several viable nodes the scheduler would prefer.
//
// The asymmetry is deliberate and load-bearing. Missing a soft preference
// makes the simulator disagree with the scheduler about *where* a pod lands,
// which costs nothing. Missing a hard constraint would make it disagree about
// *whether* a pod lands, which is how a tool like this drains a node that
// cannot be drained. So every constraint it cannot evaluate is escalated to
// Indeterminate rather than assumed satisfiable: the caller is told "I do not
// know", and a caller that does not know must not act.
package binpack

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/GipsyChef/cluster-optimizer/internal/model"
)

// Node is one candidate placement target.
type Node struct {
	Name   string
	Pool   string
	Zone   string
	Labels map[string]string
	Taints []model.Taint

	AllocatableCPUm      int64
	AllocatableMemoryMiB int64
	AllocatableExtended  map[string]int64
	// MaxPods is the kubelet's pod cap. Zero means unknown, in which case
	// the cap is not enforced.
	MaxPods int64

	// Reserved is capacity already committed on this node before placement
	// begins — in practice the DaemonSet pods that will run here regardless.
	ReservedCPUm      int64
	ReservedMemoryMiB int64
	ReservedExtended  map[string]int64
	ReservedPods      int64
}

// Pod is one unit to place.
type Pod struct {
	Namespace string
	Name      string
	Labels    map[string]string

	CPUm      int64
	MemoryMiB int64
	Extended  map[string]int64

	NodeSelector   map[string]string
	Tolerations    []model.Toleration
	Affinity       *model.Affinity
	TopologySpread []model.TopologySpreadConstraint
	// Unmodelled carries the constraints the collector could not express.
	// Any entry makes this pod unplaceable-unknown.
	Unmodelled []string
}

// Key is the pod's namespace/name identifier.
func (p Pod) Key() string { return p.Namespace + "/" + p.Name }

// Placement is the outcome of one simulation.
type Placement struct {
	// Assignments maps pod key to the node it was placed on.
	Assignments map[string]string
	// Unplaced lists pods that could not be placed, each with the predicate
	// that ruled out the last node tried.
	Unplaced []Rejection
	// Indeterminate is set when at least one pod carried a constraint the
	// simulator cannot evaluate. The result is then not a "no" — it is an
	// "unknown", and callers must not treat it as permission.
	Indeterminate bool
	// IndeterminateReasons lists the distinct unmodelled constraint kinds
	// encountered, for reporting.
	IndeterminateReasons []string
}

// Feasible reports whether every pod was placed AND nothing was unknown.
// A caller deciding whether to remove capacity should use only this.
func (p Placement) Feasible() bool {
	return len(p.Unplaced) == 0 && !p.Indeterminate
}

// Rejection records why one pod could not be placed.
type Rejection struct {
	PodKey string
	// Reason is the predicate that blocked the most nodes, which is the
	// closest thing to "the thing standing in the way".
	Reason string
	// Detail is a human-readable elaboration.
	Detail string
}

// Predicate names, used as Rejection.Reason so callers can group and rank
// what is blocking a scale-down rather than reading prose.
const (
	ReasonInsufficientCPU      = "insufficient_cpu"
	ReasonInsufficientMemory   = "insufficient_memory"
	ReasonInsufficientExtended = "insufficient_extended_resource"
	ReasonPodCapacity          = "node_pod_capacity"
	ReasonTaint                = "untolerated_taint"
	ReasonNodeSelector         = "node_selector"
	ReasonNodeAffinity         = "node_affinity"
	ReasonPodAntiAffinity      = "pod_anti_affinity"
	ReasonTopologySpread       = "topology_spread"
	ReasonNoNodes              = "no_candidate_nodes"
)

// nodeState is a node plus what the simulation has placed on it so far.
type nodeState struct {
	Node
	usedCPUm     int64
	usedMemMiB   int64
	usedExtended map[string]int64
	podCount     int64
	placed       []*Pod
}

func (n *nodeState) freeCPU() int64 { return n.AllocatableCPUm - n.ReservedCPUm - n.usedCPUm }
func (n *nodeState) freeMem() int64 {
	return n.AllocatableMemoryMiB - n.ReservedMemoryMiB - n.usedMemMiB
}

func (n *nodeState) freeExtended(name string) int64 {
	return n.AllocatableExtended[name] - n.ReservedExtended[name] - n.usedExtended[name]
}

func (n *nodeState) topologyValue(key string) (string, bool) {
	switch key {
	case "kubernetes.io/hostname":
		// Always available, whether or not the node carries the label.
		if v, ok := n.Labels[key]; ok {
			return v, true
		}
		return n.Name, true
	default:
		v, ok := n.Labels[key]
		return v, ok
	}
}

// Fit runs a first-fit-decreasing pass: pods are sorted largest-first and each
// is placed on the first node that satisfies every hard predicate.
//
// FFD is not optimal — no polynomial algorithm is, bin-packing being NP-hard —
// and it can fail on an arrangement a perfect packer would solve. That error
// direction is the safe one: a false "does not fit" costs a missed saving,
// while a false "fits" costs an outage. The scheduler itself is also greedy
// and online, so a packing FFD cannot find is one the real cluster would
// likely not achieve either.
func Fit(nodes []Node, pods []Pod) Placement {
	result := Placement{Assignments: map[string]string{}}

	// Unknown constraints are decided before any packing: if we cannot model
	// a pod, no amount of successful placement elsewhere makes the answer
	// trustworthy.
	seenReason := map[string]bool{}
	for i := range pods {
		for _, reason := range pods[i].Unmodelled {
			if !seenReason[reason] {
				seenReason[reason] = true
				result.IndeterminateReasons = append(result.IndeterminateReasons, reason)
			}
		}
	}
	if len(result.IndeterminateReasons) > 0 {
		result.Indeterminate = true
		sort.Strings(result.IndeterminateReasons)
	}

	if len(nodes) == 0 {
		for i := range pods {
			result.Unplaced = append(result.Unplaced, Rejection{
				PodKey: pods[i].Key(), Reason: ReasonNoNodes,
				Detail: "no nodes remain in the pool",
			})
		}
		return result
	}

	states := make([]*nodeState, 0, len(nodes))
	for _, n := range nodes {
		states = append(states, &nodeState{Node: n, usedExtended: map[string]int64{}})
	}

	ordered := sortedForPacking(pods)
	for _, pod := range ordered {
		placed := false
		var votes map[string]int
		for _, node := range states {
			reason, ok := admits(node, pod, states)
			if ok {
				assign(node, pod)
				result.Assignments[pod.Key()] = node.Name
				placed = true
				break
			}
			if votes == nil {
				votes = map[string]int{}
			}
			votes[reason]++
		}
		if !placed {
			result.Unplaced = append(result.Unplaced, Rejection{
				PodKey: pod.Key(),
				Reason: dominant(votes),
				Detail: describe(votes, len(states)),
			})
		}
	}
	return result
}

// sortedForPacking orders pods largest-first. Memory leads because it is the
// binding resource on most clusters — it cannot be compressed the way CPU can,
// so a memory-tight pod placed late is the one that fails.
func sortedForPacking(pods []Pod) []*Pod {
	out := make([]*Pod, 0, len(pods))
	for i := range pods {
		out = append(out, &pods[i])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].MemoryMiB != out[j].MemoryMiB {
			return out[i].MemoryMiB > out[j].MemoryMiB
		}
		if out[i].CPUm != out[j].CPUm {
			return out[i].CPUm > out[j].CPUm
		}
		// Deterministic tiebreak: the same input must always produce the
		// same answer, or the UI shows a different verdict every run.
		return out[i].Key() < out[j].Key()
	})
	return out
}

func assign(node *nodeState, pod *Pod) {
	node.usedCPUm += pod.CPUm
	node.usedMemMiB += pod.MemoryMiB
	node.podCount++
	for name, qty := range pod.Extended {
		node.usedExtended[name] += qty
	}
	node.placed = append(node.placed, pod)
}

// admits evaluates every hard predicate for one (node, pod) pair. Returns the
// first failing predicate's name, or ok=true.
//
// Order matters only for the quality of the reported reason: the cheap
// resource checks run first, so a pod that simply does not fit is reported as
// not fitting rather than as some incidental topology mismatch.
func admits(node *nodeState, pod *Pod, all []*nodeState) (string, bool) {
	if node.freeCPU() < pod.CPUm {
		return ReasonInsufficientCPU, false
	}
	if node.freeMem() < pod.MemoryMiB {
		return ReasonInsufficientMemory, false
	}
	for name, qty := range pod.Extended {
		if node.freeExtended(name) < qty {
			return ReasonInsufficientExtended, false
		}
	}
	if node.MaxPods > 0 && node.ReservedPods+node.podCount+1 > node.MaxPods {
		return ReasonPodCapacity, false
	}
	if !tolerates(pod.Tolerations, node.Taints) {
		return ReasonTaint, false
	}
	if !matchesNodeSelector(pod.NodeSelector, node.Labels) {
		return ReasonNodeSelector, false
	}
	if pod.Affinity != nil && len(pod.Affinity.RequiredNodeAffinity) > 0 {
		if !matchesNodeAffinity(pod.Affinity.RequiredNodeAffinity, node) {
			return ReasonNodeAffinity, false
		}
	}
	if pod.Affinity != nil && len(pod.Affinity.RequiredPodAntiAffinity) > 0 {
		if !satisfiesAntiAffinity(pod, node, all) {
			return ReasonPodAntiAffinity, false
		}
	}
	if len(pod.TopologySpread) > 0 {
		if !satisfiesSpread(pod, node, all) {
			return ReasonTopologySpread, false
		}
	}
	return "", true
}

// tolerates reports whether the pod tolerates every NoSchedule/NoExecute taint
// on the node. PreferNoSchedule is advisory and cannot block placement.
func tolerates(tolerations []model.Toleration, taints []model.Taint) bool {
	for _, taint := range taints {
		if taint.Effect == "PreferNoSchedule" {
			continue
		}
		if !toleratedBy(taint, tolerations) {
			return false
		}
	}
	return true
}

func toleratedBy(taint model.Taint, tolerations []model.Toleration) bool {
	for _, tol := range tolerations {
		// An empty effect tolerates all effects.
		if tol.Effect != "" && tol.Effect != taint.Effect {
			continue
		}
		switch tol.Operator {
		case "Exists":
			// An empty key with Exists tolerates everything.
			if tol.Key == "" || tol.Key == taint.Key {
				return true
			}
		case "", "Equal":
			if tol.Key == taint.Key && tol.Value == taint.Value {
				return true
			}
		}
	}
	return false
}

func matchesNodeSelector(selector, labels map[string]string) bool {
	for key, want := range selector {
		if labels[key] != want {
			return false
		}
	}
	return true
}

// matchesNodeAffinity evaluates the OR-list of required node selector terms.
// A node satisfies the affinity if it matches any single term; within a term,
// every expression must match.
func matchesNodeAffinity(terms []model.NodeSelectorTerm, node *nodeState) bool {
	for _, term := range terms {
		if termMatches(term, node) {
			return true
		}
	}
	return false
}

func termMatches(term model.NodeSelectorTerm, node *nodeState) bool {
	// The API is explicit that a null or empty node selector term matches no
	// objects. Treating it as "no constraint" would be the optimistic reading
	// of a pod the scheduler can in fact place nowhere.
	if len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
		return false
	}
	for _, expr := range term.MatchExpressions {
		value, present := node.Labels[expr.Key]
		if !requirementMatches(expr, value, present) {
			return false
		}
	}
	for _, field := range term.MatchFields {
		// metadata.name is the only field selector in practice.
		var value string
		var present bool
		if field.Key == "metadata.name" {
			value, present = node.Name, true
		}
		if !requirementMatches(field, value, present) {
			return false
		}
	}
	return true
}

func requirementMatches(req model.SelectorRequirement, value string, present bool) bool {
	switch req.Operator {
	case "In":
		return present && containsString(req.Values, value)
	case "NotIn":
		return !present || !containsString(req.Values, value)
	case "Exists":
		return present
	case "DoesNotExist":
		return !present
	case "Gt", "Lt":
		if !present || len(req.Values) == 0 {
			return false
		}
		have, err1 := strconv.ParseInt(value, 10, 64)
		want, err2 := strconv.ParseInt(req.Values[0], 10, 64)
		if err1 != nil || err2 != nil {
			return false
		}
		if req.Operator == "Gt" {
			return have > want
		}
		return have < want
	case "", "Equals", "Equal":
		return present && len(req.Values) > 0 && value == req.Values[0]
	}
	// An operator we do not recognise must not silently pass.
	return false
}

// satisfiesAntiAffinity checks that no pod matching a required anti-affinity
// term already occupies the same topology domain as the candidate node.
//
// This is the constraint that most often sets a hard floor on node count: one
// replica per hostname means the replica count *is* the minimum node count,
// regardless of how much spare CPU and memory the cluster has.
func satisfiesAntiAffinity(pod *Pod, node *nodeState, all []*nodeState) bool {
	for _, term := range pod.Affinity.RequiredPodAntiAffinity {
		domain, ok := node.topologyValue(term.TopologyKey)
		if !ok {
			// The candidate node carries no value for the topology key, so
			// the scheduler cannot place the pod here at all.
			return false
		}
		for _, other := range all {
			otherDomain, ok := other.topologyValue(term.TopologyKey)
			if !ok || otherDomain != domain {
				continue
			}
			for _, occupant := range other.placed {
				if occupant.Key() == pod.Key() {
					continue
				}
				if !sameNamespaceScope(term, pod, occupant) {
					continue
				}
				if selectorMatches(term.LabelSelector, occupant.Labels) {
					return false
				}
			}
		}
	}
	return true
}

// satisfiesSpread enforces DoNotSchedule topology spread constraints.
// ScheduleAnyway constraints are soft and cannot block a placement.
func satisfiesSpread(pod *Pod, node *nodeState, all []*nodeState) bool {
	for _, constraint := range pod.TopologySpread {
		if constraint.WhenUnsatisfiable != "DoNotSchedule" {
			continue
		}
		candidateDomain, ok := node.topologyValue(constraint.TopologyKey)
		if !ok {
			// Nodes without the topology key are excluded from spread
			// consideration by the scheduler, so this node cannot host it.
			return false
		}

		counts := map[string]int{}
		for _, other := range all {
			domain, ok := other.topologyValue(constraint.TopologyKey)
			if !ok {
				continue
			}
			if _, seen := counts[domain]; !seen {
				counts[domain] = 0
			}
			for _, occupant := range other.placed {
				if occupant.Key() == pod.Key() {
					continue
				}
				if occupant.Namespace != pod.Namespace {
					continue
				}
				if selectorMatches(constraint.LabelSelector, occupant.Labels) {
					counts[domain]++
				}
			}
		}
		// Placing here adds one to the candidate domain.
		projected := counts[candidateDomain] + 1
		minCount := projected
		for domain, count := range counts {
			if domain == candidateDomain {
				continue
			}
			if count < minCount {
				minCount = count
			}
		}
		// minDomains tightens the constraint: when fewer eligible domains
		// exist than the constraint demands, the scheduler treats the global
		// minimum as zero, so the candidate domain's own count becomes the
		// skew. Ignoring this understates skew, which permits placements the
		// scheduler would refuse.
		if constraint.MinDomains != nil && *constraint.MinDomains > 0 &&
			int32(len(counts)) < *constraint.MinDomains {
			minCount = 0
		}
		if int32(projected-minCount) > constraint.MaxSkew {
			return false
		}
	}
	return true
}

// sameNamespaceScope reports whether the occupant falls within the namespace
// scope of the affinity term. A term with no namespaces listed means the
// pod's own namespace.
func sameNamespaceScope(term model.PodAffinityTerm, pod, occupant *Pod) bool {
	if len(term.Namespaces) == 0 {
		return occupant.Namespace == pod.Namespace
	}
	return containsString(term.Namespaces, occupant.Namespace)
}

// selectorMatches evaluates a label selector against a pod's labels.
//
// Note the API's asymmetry, reproduced here: within an affinity term a nil
// selector matches nothing, while an explicitly empty one matches everything.
func selectorMatches(selector *model.LabelSelector, labels map[string]string) bool {
	if selector == nil {
		return false
	}
	if selector.Empty() {
		return true
	}
	for key, want := range selector.MatchLabels {
		if labels[key] != want {
			return false
		}
	}
	for _, expr := range selector.MatchExpressions {
		value, present := labels[expr.Key]
		if !requirementMatches(expr, value, present) {
			return false
		}
	}
	return true
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// dominant returns the predicate that rejected the most nodes — the closest
// thing to "the reason this pod has nowhere to go".
func dominant(votes map[string]int) string {
	best, bestCount := "", -1
	for reason, count := range votes {
		if count > bestCount || (count == bestCount && reason < best) {
			best, bestCount = reason, count
		}
	}
	if best == "" {
		return ReasonNoNodes
	}
	return best
}

func describe(votes map[string]int, nodeCount int) string {
	if len(votes) == 0 {
		return "no candidate nodes"
	}
	parts := make([]string, 0, len(votes))
	for reason, count := range votes {
		parts = append(parts, fmt.Sprintf("%s on %d", reason, count))
	}
	sort.Strings(parts)
	return fmt.Sprintf("rejected by %d node(s): %s", nodeCount, strings.Join(parts, ", "))
}
