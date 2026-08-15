package model

// Scheduling constraints captured from the cluster so a placement simulation
// can answer "would this pod actually land on that node?" rather than only
// "does it fit numerically".
//
// These mirror the Kubernetes API shapes closely but deliberately do not
// reproduce all of them. Two rules govern what is here:
//
//  1. Only *required* constraints are modelled. Preferred/soft terms
//     (`preferredDuringSchedulingIgnoredDuringExecution`, `ScheduleAnyway`
//     spread) influence which node the scheduler picks but can never make a
//     placement impossible, so ignoring them cannot make the simulator
//     optimistic about feasibility.
//
//  2. Anything required that is *not* modelled must be recorded in
//     Pod.Unmodelled. The simulator treats a pod carrying unmodelled
//     constraints as unplaceable-unknown, which forces the enclosing node pool
//     to `indeterminate` and blocks enforcement. Being wrong in the pessimistic
//     direction costs a missed saving; being wrong in the optimistic direction
//     costs an outage.

// Taint is a node taint. A pod without a matching toleration cannot be
// scheduled onto a node carrying a NoSchedule or NoExecute taint.
type Taint struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Effect string `json:"effect"`
}

// Toleration is a pod's tolerance for a taint.
type Toleration struct {
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
	// TolerationSeconds is recorded but not used for placement: a
	// time-limited toleration still admits the pod at scheduling time.
	TolerationSeconds *int64 `json:"toleration_seconds,omitempty"`
}

// SelectorRequirement is one matchExpressions entry. Used for both node
// selector terms and label selectors, which share this shape.
type SelectorRequirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values,omitempty"`
}

// NodeSelectorTerm is one OR-branch of required node affinity. A node
// satisfies the affinity if it matches any single term; within a term every
// expression must match.
type NodeSelectorTerm struct {
	MatchExpressions []SelectorRequirement `json:"match_expressions,omitempty"`
	// MatchFields selects on node fields (in practice `metadata.name`)
	// rather than labels.
	MatchFields []SelectorRequirement `json:"match_fields,omitempty"`
}

// LabelSelector is the standard matchLabels + matchExpressions selector.
type LabelSelector struct {
	MatchLabels      map[string]string     `json:"match_labels,omitempty"`
	MatchExpressions []SelectorRequirement `json:"match_expressions,omitempty"`
}

// Empty reports whether the selector constrains nothing. Note the API's
// asymmetry, which callers must respect: an empty selector on a pod affinity
// term matches *every* pod, whereas a nil selector matches none.
func (s *LabelSelector) Empty() bool {
	return s == nil || (len(s.MatchLabels) == 0 && len(s.MatchExpressions) == 0)
}

// PodAffinityTerm is a required pod affinity or anti-affinity rule: pods
// matching LabelSelector must (affinity) or must not (anti-affinity) already
// occupy the topology domain identified by TopologyKey.
//
// The overwhelmingly common case in cost work is anti-affinity on
// `kubernetes.io/hostname`, which pins one replica per node and is frequently
// the real reason a cluster cannot shrink.
type PodAffinityTerm struct {
	LabelSelector *LabelSelector `json:"label_selector,omitempty"`
	// Namespaces empty means "the pod's own namespace".
	Namespaces        []string       `json:"namespaces,omitempty"`
	NamespaceSelector *LabelSelector `json:"namespace_selector,omitempty"`
	TopologyKey       string         `json:"topology_key"`
}

// TopologySpreadConstraint limits how unevenly matching pods may be spread
// across a topology domain. Only DoNotSchedule constraints can block a
// placement; ScheduleAnyway ones are captured for reporting but never enforced
// by the simulator.
type TopologySpreadConstraint struct {
	MaxSkew           int32          `json:"max_skew"`
	TopologyKey       string         `json:"topology_key"`
	WhenUnsatisfiable string         `json:"when_unsatisfiable"`
	LabelSelector     *LabelSelector `json:"label_selector,omitempty"`
	MinDomains        *int32         `json:"min_domains,omitempty"`
}

// Affinity groups the required affinity rules attached to a pod.
type Affinity struct {
	// RequiredNodeAffinity is the OR-list of node selector terms from
	// nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.
	RequiredNodeAffinity []NodeSelectorTerm `json:"required_node_affinity,omitempty"`
	RequiredPodAffinity  []PodAffinityTerm  `json:"required_pod_affinity,omitempty"`
	// RequiredPodAntiAffinity is the constraint that most often sets a hard
	// floor on node count.
	RequiredPodAntiAffinity []PodAffinityTerm `json:"required_pod_anti_affinity,omitempty"`
}

// Empty reports whether there is nothing here for the simulator to enforce.
func (a *Affinity) Empty() bool {
	return a == nil ||
		(len(a.RequiredNodeAffinity) == 0 &&
			len(a.RequiredPodAffinity) == 0 &&
			len(a.RequiredPodAntiAffinity) == 0)
}

// Reasons a pod cannot be placed by simulation alone. Recorded on
// Pod.Unmodelled; any value present forces the pod's pool to `indeterminate`
// rather than allowing an optimistic guess.
const (
	// UnmodelledExtendedResource marks a pod requesting a resource the
	// simulator does not track allocatable capacity for (device plugins,
	// hugepages, vendor resources).
	UnmodelledExtendedResource = "extended_resource"
	// UnmodelledVolumeAffinity marks a pod bound to a PersistentVolume whose
	// node affinity pins it to specific nodes or zones. Local and
	// zone-bound volumes make a pod effectively immovable.
	UnmodelledVolumeAffinity = "volume_node_affinity"
	// UnmodelledMatchLabelKeys marks use of the newer spread-constraint
	// fields (matchLabelKeys, nodeAffinityPolicy, nodeTaintsPolicy) whose
	// semantics the simulator does not reproduce.
	UnmodelledMatchLabelKeys = "spread_match_label_keys"
	// UnmodelledNamespaceSelector marks a pod affinity term scoped by
	// namespaceSelector, which requires namespace label resolution the
	// simulator does not perform.
	UnmodelledNamespaceSelector = "affinity_namespace_selector"
	// UnmodelledRuntimeOverhead marks a pod with RuntimeClass overhead, whose
	// effective request exceeds the sum of its container requests.
	UnmodelledRuntimeOverhead = "runtime_class_overhead"
	// UnmodelledRequiredPodAffinity marks required pod *affinity* (not
	// anti-affinity). Affinity is order-dependent — a pod must land in a
	// domain that already holds its partner — so a first-fit pass can reach
	// a different answer than the scheduler depending purely on the order it
	// happens to try pods in. Rather than simulate it approximately and risk
	// an optimistic result, the pod is declared unplaceable-unknown.
	// Anti-affinity has no such ordering problem and is fully modelled.
	UnmodelledRequiredPodAffinity = "required_pod_affinity"
)
