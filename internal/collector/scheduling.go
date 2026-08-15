package collector

import (
	"strings"

	"github.com/GipsyChef/cluster-optimizer/internal/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Provider labels that identify a node's pool, in the order we trust them.
// Capacity has to be evaluated per pool: pools differ in instance type, taints
// and zone, so a cluster-wide average describes a node that does not exist.
var poolLabelKeys = []string{
	"doks.digitalocean.com/node-pool",
	"eks.amazonaws.com/nodegroup",
	"cloud.google.com/gke-nodepool",
	"agentpool",                        // AKS
	"karpenter.sh/nodepool",            // Karpenter v1
	"node.kubernetes.io/instancegroup", // generic
}

// zoneLabelKeys are the standard and legacy failure-domain labels.
var zoneLabelKeys = []string{
	"topology.kubernetes.io/zone",
	"failure-domain.beta.kubernetes.io/zone",
}

// resourcesTrackedNatively are the two resources the simulator accounts for
// directly. Everything else on a node or pod goes through the extended-resource
// path so a GPU or hugepage request cannot be silently ignored.
var resourcesTrackedNatively = map[corev1.ResourceName]bool{
	corev1.ResourceCPU:    true,
	corev1.ResourceMemory: true,
	// Pods are capped per node, but the cap is a node-level attribute rather
	// than something a pod requests; the simulator handles it separately.
	corev1.ResourcePods: true,
}

// nodePool returns the node's pool identity. Falls back to the instance type,
// then to a single synthetic pool, so every node belongs to exactly one group
// even on clusters that label nothing.
func nodePool(node corev1.Node) string {
	for _, key := range poolLabelKeys {
		if value := node.Labels[key]; value != "" {
			return value
		}
	}
	if t := node.Labels["node.kubernetes.io/instance-type"]; t != "" {
		return "instance-type/" + t
	}
	if t := node.Labels["beta.kubernetes.io/instance-type"]; t != "" {
		return "instance-type/" + t
	}
	return "default"
}

func nodeZone(node corev1.Node) string {
	for _, key := range zoneLabelKeys {
		if value := node.Labels[key]; value != "" {
			return value
		}
	}
	return ""
}

func nodeReady(node corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	// No Ready condition at all: treat as not ready rather than assuming
	// capacity that may not exist.
	return false
}

func nodeTaints(node corev1.Node) []model.Taint {
	if len(node.Spec.Taints) == 0 {
		return nil
	}
	out := make([]model.Taint, 0, len(node.Spec.Taints))
	for _, t := range node.Spec.Taints {
		out = append(out, model.Taint{Key: t.Key, Value: t.Value, Effect: string(t.Effect)})
	}
	return out
}

// extendedAllocatable captures every allocatable resource the simulator does
// not track natively, as an integer count. Quantities that are not whole
// numbers (or that overflow) are skipped — a pod requesting them will be
// flagged unmodelled instead, which blocks rather than permits.
func extendedAllocatable(list corev1.ResourceList) map[string]int64 {
	var out map[string]int64
	for name, qty := range list {
		if resourcesTrackedNatively[name] {
			continue
		}
		value, ok := qty.AsInt64()
		if !ok {
			continue
		}
		if out == nil {
			out = map[string]int64{}
		}
		out[string(name)] = value
	}
	return out
}

func podTolerations(spec corev1.PodSpec) []model.Toleration {
	if len(spec.Tolerations) == 0 {
		return nil
	}
	out := make([]model.Toleration, 0, len(spec.Tolerations))
	for _, t := range spec.Tolerations {
		out = append(out, model.Toleration{
			Key:               t.Key,
			Operator:          string(t.Operator),
			Value:             t.Value,
			Effect:            string(t.Effect),
			TolerationSeconds: t.TolerationSeconds,
		})
	}
	return out
}

func convertLabelSelector(sel *metav1.LabelSelector) *model.LabelSelector {
	if sel == nil {
		return nil
	}
	out := &model.LabelSelector{MatchLabels: sel.MatchLabels}
	for _, expr := range sel.MatchExpressions {
		out.MatchExpressions = append(out.MatchExpressions, model.SelectorRequirement{
			Key:      expr.Key,
			Operator: string(expr.Operator),
			Values:   expr.Values,
		})
	}
	return out
}

func convertNodeSelectorTerms(terms []corev1.NodeSelectorTerm) []model.NodeSelectorTerm {
	if len(terms) == 0 {
		return nil
	}
	out := make([]model.NodeSelectorTerm, 0, len(terms))
	for _, term := range terms {
		converted := model.NodeSelectorTerm{}
		for _, expr := range term.MatchExpressions {
			converted.MatchExpressions = append(converted.MatchExpressions, model.SelectorRequirement{
				Key: expr.Key, Operator: string(expr.Operator), Values: expr.Values,
			})
		}
		for _, field := range term.MatchFields {
			converted.MatchFields = append(converted.MatchFields, model.SelectorRequirement{
				Key: field.Key, Operator: string(field.Operator), Values: field.Values,
			})
		}
		out = append(out, converted)
	}
	return out
}

func convertPodAffinityTerms(terms []corev1.PodAffinityTerm) []model.PodAffinityTerm {
	if len(terms) == 0 {
		return nil
	}
	out := make([]model.PodAffinityTerm, 0, len(terms))
	for _, term := range terms {
		out = append(out, model.PodAffinityTerm{
			LabelSelector:     convertLabelSelector(term.LabelSelector),
			Namespaces:        term.Namespaces,
			NamespaceSelector: convertLabelSelector(term.NamespaceSelector),
			TopologyKey:       term.TopologyKey,
		})
	}
	return out
}

// podAffinity extracts only the required (hard) affinity rules. Preferred
// terms are deliberately dropped: they steer the scheduler's choice but can
// never make a placement impossible, so omitting them cannot make the
// simulator optimistic about feasibility.
func podAffinity(spec corev1.PodSpec) *model.Affinity {
	if spec.Affinity == nil {
		return nil
	}
	out := &model.Affinity{}
	if na := spec.Affinity.NodeAffinity; na != nil && na.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		out.RequiredNodeAffinity = convertNodeSelectorTerms(na.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms)
	}
	if pa := spec.Affinity.PodAffinity; pa != nil {
		out.RequiredPodAffinity = convertPodAffinityTerms(pa.RequiredDuringSchedulingIgnoredDuringExecution)
	}
	if paa := spec.Affinity.PodAntiAffinity; paa != nil {
		out.RequiredPodAntiAffinity = convertPodAffinityTerms(paa.RequiredDuringSchedulingIgnoredDuringExecution)
	}
	if out.Empty() {
		return nil
	}
	return out
}

func podTopologySpread(spec corev1.PodSpec) []model.TopologySpreadConstraint {
	if len(spec.TopologySpreadConstraints) == 0 {
		return nil
	}
	out := make([]model.TopologySpreadConstraint, 0, len(spec.TopologySpreadConstraints))
	for _, c := range spec.TopologySpreadConstraints {
		out = append(out, model.TopologySpreadConstraint{
			MaxSkew:           c.MaxSkew,
			TopologyKey:       c.TopologyKey,
			WhenUnsatisfiable: string(c.WhenUnsatisfiable),
			LabelSelector:     convertLabelSelector(c.LabelSelector),
			MinDomains:        c.MinDomains,
		})
	}
	return out
}

// extendedRequests sums non-CPU/memory container requests.
func extendedRequests(spec corev1.PodSpec) map[string]int64 {
	var out map[string]int64
	add := func(list corev1.ResourceList) {
		for name, qty := range list {
			if resourcesTrackedNatively[name] {
				continue
			}
			value, ok := qty.AsInt64()
			if !ok {
				continue
			}
			if value == 0 {
				continue
			}
			if out == nil {
				out = map[string]int64{}
			}
			out[string(name)] += value
		}
	}
	for _, c := range spec.Containers {
		add(c.Resources.Requests)
	}
	// Init containers do not sum: the effective request is the max of any one
	// init container against the summed regular containers. Extended
	// resources on init containers are rare; take the max to stay pessimistic.
	for _, c := range spec.InitContainers {
		for name, qty := range c.Resources.Requests {
			if resourcesTrackedNatively[name] {
				continue
			}
			value, ok := qty.AsInt64()
			if !ok || value == 0 {
				continue
			}
			if out == nil {
				out = map[string]int64{}
			}
			if value > out[string(name)] {
				out[string(name)] = value
			}
		}
	}
	return out
}

// unmodelledConstraints lists the required constraints on this pod that the
// simulator cannot evaluate.
//
// This is the honesty valve for the whole placement model. Every entry it
// returns forces the pod's pool to `indeterminate`, which blocks enforcement
// for that pool rather than letting the simulator guess. Extending the
// simulator means removing entries from here — never suppressing them.
func unmodelledConstraints(pod corev1.Pod, extended map[string]int64) []string {
	var reasons []string
	seen := map[string]bool{}
	add := func(reason string) {
		if !seen[reason] {
			seen[reason] = true
			reasons = append(reasons, reason)
		}
	}

	// Extended resources are modelled only when the value is a plain count we
	// can compare against node allocatable. Ephemeral storage is excluded:
	// it is present on virtually every pod, is not a placement constraint in
	// practice, and flagging it would make every pool indeterminate.
	for name := range extended {
		if name == string(corev1.ResourceEphemeralStorage) {
			continue
		}
		add(model.UnmodelledExtendedResource)
		break
	}

	if pod.Spec.Overhead != nil {
		add(model.UnmodelledRuntimeOverhead)
	}

	for _, c := range pod.Spec.TopologySpreadConstraints {
		if len(c.MatchLabelKeys) > 0 || c.NodeAffinityPolicy != nil || c.NodeTaintsPolicy != nil {
			add(model.UnmodelledMatchLabelKeys)
			break
		}
	}

	if pod.Spec.Affinity != nil {
		terms := []corev1.PodAffinityTerm{}
		if pa := pod.Spec.Affinity.PodAffinity; pa != nil {
			if len(pa.RequiredDuringSchedulingIgnoredDuringExecution) > 0 {
				add(model.UnmodelledRequiredPodAffinity)
			}
			terms = append(terms, pa.RequiredDuringSchedulingIgnoredDuringExecution...)
		}
		if paa := pod.Spec.Affinity.PodAntiAffinity; paa != nil {
			terms = append(terms, paa.RequiredDuringSchedulingIgnoredDuringExecution...)
		}
		for _, term := range terms {
			if term.NamespaceSelector != nil {
				add(model.UnmodelledNamespaceSelector)
				break
			}
		}
	}

	// A pod bound to a PVC may be pinned to a node or zone by the volume's
	// own node affinity. We do not resolve PV topology here, so any
	// non-ephemeral claim makes the pod's mobility unknown.
	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim != nil {
			add(model.UnmodelledVolumeAffinity)
			break
		}
	}

	return reasons
}

// mirrorPod reports whether the kubelet, not a controller, owns this pod.
func mirrorPod(pod corev1.Pod) bool {
	_, ok := pod.Annotations["kubernetes.io/config.mirror"]
	return ok
}

// normalizePriorityClass trims the system- prefixes to nothing special; the
// name is carried verbatim for reporting.
func normalizePriorityClass(name string) string {
	return strings.TrimSpace(name)
}
