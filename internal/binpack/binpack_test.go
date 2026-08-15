package binpack

import (
	"testing"

	"github.com/GipsyChef/cluster-optimizer/internal/model"
)

func node(name string, cpu, mem int64) Node {
	return Node{
		Name: name, Pool: "pool-a",
		AllocatableCPUm: cpu, AllocatableMemoryMiB: mem,
		Labels: map[string]string{"kubernetes.io/hostname": name},
	}
}

func pod(ns, name string, cpu, mem int64) Pod {
	return Pod{Namespace: ns, Name: name, CPUm: cpu, MemoryMiB: mem}
}

func TestFit_PlacesEverythingWhenCapacityAllows(t *testing.T) {
	nodes := []Node{node("n1", 2000, 4096), node("n2", 2000, 4096)}
	pods := []Pod{pod("default", "a", 500, 512), pod("default", "b", 500, 512)}

	got := Fit(nodes, pods)
	if !got.Feasible() {
		t.Fatalf("expected a feasible placement, got %+v", got)
	}
	if len(got.Assignments) != 2 {
		t.Errorf("expected both pods placed, got %v", got.Assignments)
	}
}

func TestFit_ReportsInsufficientMemoryAsTheBindingConstraint(t *testing.T) {
	nodes := []Node{node("n1", 8000, 1024)}
	pods := []Pod{pod("default", "big", 100, 4096)}

	got := Fit(nodes, pods)
	if got.Feasible() {
		t.Fatal("a 4Gi pod cannot fit a 1Gi node")
	}
	if len(got.Unplaced) != 1 {
		t.Fatalf("expected one rejection, got %+v", got.Unplaced)
	}
	if got.Unplaced[0].Reason != ReasonInsufficientMemory {
		t.Errorf("expected memory to be named as the constraint, got %q", got.Unplaced[0].Reason)
	}
}

func TestFit_ReservedCapacityIsNotAvailable(t *testing.T) {
	// DaemonSet overhead is pre-charged to every node; a pod must fit in
	// what is left, not in raw allocatable.
	n := node("n1", 1000, 1024)
	n.ReservedCPUm, n.ReservedMemoryMiB = 800, 800
	got := Fit([]Node{n}, []Pod{pod("default", "a", 500, 500)})
	if got.Feasible() {
		t.Fatal("the pod should not fit in the capacity left after DaemonSet reservation")
	}
}

func TestFit_HonoursTaintsAndTolerations(t *testing.T) {
	tainted := node("gpu-1", 4000, 8192)
	tainted.Taints = []model.Taint{{Key: "nvidia.com/gpu", Value: "true", Effect: "NoSchedule"}}

	untolerating := pod("default", "web", 100, 128)
	if got := Fit([]Node{tainted}, []Pod{untolerating}); got.Feasible() {
		t.Fatal("a pod with no toleration must not be placed on a NoSchedule-tainted node")
	} else if got.Unplaced[0].Reason != ReasonTaint {
		t.Errorf("expected the taint to be named, got %q", got.Unplaced[0].Reason)
	}

	tolerating := pod("default", "ml", 100, 128)
	tolerating.Tolerations = []model.Toleration{{Key: "nvidia.com/gpu", Operator: "Equal", Value: "true", Effect: "NoSchedule"}}
	if got := Fit([]Node{tainted}, []Pod{tolerating}); !got.Feasible() {
		t.Fatalf("a tolerating pod should be placed, got %+v", got.Unplaced)
	}
}

func TestFit_PreferNoScheduleDoesNotBlock(t *testing.T) {
	n := node("n1", 4000, 8192)
	n.Taints = []model.Taint{{Key: "soft", Effect: "PreferNoSchedule"}}
	if got := Fit([]Node{n}, []Pod{pod("default", "web", 100, 128)}); !got.Feasible() {
		t.Fatal("PreferNoSchedule is advisory and must not block placement")
	}
}

func TestFit_HonoursNodeSelector(t *testing.T) {
	general := node("n1", 4000, 8192)
	general.Labels["tier"] = "general"
	memory := node("n2", 4000, 8192)
	memory.Labels["tier"] = "memory"

	p := pod("default", "cache", 100, 128)
	p.NodeSelector = map[string]string{"tier": "memory"}

	got := Fit([]Node{general, memory}, []Pod{p})
	if !got.Feasible() {
		t.Fatalf("expected placement on the matching node, got %+v", got.Unplaced)
	}
	if got.Assignments[p.Key()] != "n2" {
		t.Errorf("expected placement on n2, got %q", got.Assignments[p.Key()])
	}
}

func TestFit_RequiredNodeAffinityIsAnOrOfTerms(t *testing.T) {
	n := node("n1", 4000, 8192)
	n.Labels["zone"] = "fra1"

	p := pod("default", "app", 100, 128)
	p.Affinity = &model.Affinity{RequiredNodeAffinity: []model.NodeSelectorTerm{
		{MatchExpressions: []model.SelectorRequirement{{Key: "zone", Operator: "In", Values: []string{"ams3"}}}},
		{MatchExpressions: []model.SelectorRequirement{{Key: "zone", Operator: "In", Values: []string{"fra1"}}}},
	}}
	if got := Fit([]Node{n}, []Pod{p}); !got.Feasible() {
		t.Fatalf("matching any single term should admit the node, got %+v", got.Unplaced)
	}

	p.Affinity.RequiredNodeAffinity = []model.NodeSelectorTerm{
		{MatchExpressions: []model.SelectorRequirement{{Key: "zone", Operator: "In", Values: []string{"ams3"}}}},
	}
	if got := Fit([]Node{n}, []Pod{p}); got.Feasible() {
		t.Fatal("a node matching no term must be rejected")
	}
}

func TestFit_HostnameAntiAffinitySetsANodeFloor(t *testing.T) {
	// Three replicas with required anti-affinity per hostname need three
	// nodes, no matter how much spare capacity two nodes have.
	makeReplica := func(name string) Pod {
		p := pod("default", name, 10, 16)
		p.Labels = map[string]string{"app": "payments"}
		p.Affinity = &model.Affinity{RequiredPodAntiAffinity: []model.PodAffinityTerm{{
			TopologyKey:   "kubernetes.io/hostname",
			LabelSelector: &model.LabelSelector{MatchLabels: map[string]string{"app": "payments"}},
		}}}
		return p
	}
	replicas := []Pod{makeReplica("p1"), makeReplica("p2"), makeReplica("p3")}

	// Two huge nodes: capacity is abundant, topology is not.
	twoNodes := []Node{node("n1", 64000, 262144), node("n2", 64000, 262144)}
	got := Fit(twoNodes, replicas)
	if got.Feasible() {
		t.Fatal("three hostname-anti-affine replicas cannot fit two nodes")
	}
	if got.Unplaced[0].Reason != ReasonPodAntiAffinity {
		t.Errorf("expected anti-affinity to be named as the constraint, got %q", got.Unplaced[0].Reason)
	}

	threeNodes := append(twoNodes, node("n3", 64000, 262144))
	if got := Fit(threeNodes, replicas); !got.Feasible() {
		t.Fatalf("three nodes should hold three replicas, got %+v", got.Unplaced)
	}
}

func TestFit_AntiAffinityIgnoresOtherNamespaces(t *testing.T) {
	a := pod("team-a", "web", 10, 16)
	a.Labels = map[string]string{"app": "web"}
	a.Affinity = &model.Affinity{RequiredPodAntiAffinity: []model.PodAffinityTerm{{
		TopologyKey:   "kubernetes.io/hostname",
		LabelSelector: &model.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
	}}}
	b := pod("team-b", "web", 10, 16)
	b.Labels = map[string]string{"app": "web"}

	// b is in a different namespace, so it does not conflict with a's term.
	if got := Fit([]Node{node("n1", 4000, 8192)}, []Pod{a, b}); !got.Feasible() {
		t.Fatalf("anti-affinity defaults to the pod's own namespace, got %+v", got.Unplaced)
	}
}

// spreadReplica builds a replica with a hard maxSkew=1 hostname spread.
func spreadReplica(name string) Pod {
	p := pod("default", name, 10, 16)
	p.Labels = map[string]string{"app": "api"}
	p.TopologySpread = []model.TopologySpreadConstraint{{
		MaxSkew:           1,
		TopologyKey:       "kubernetes.io/hostname",
		WhenUnsatisfiable: "DoNotSchedule",
		LabelSelector:     &model.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
	}}
	return p
}

func TestFit_TopologySpreadAllowsAPermittedImbalance(t *testing.T) {
	// Three replicas over two hostnames land 2/1, a skew of exactly 1, which
	// maxSkew=1 permits. Spread only blocks when no domain can absorb the
	// pod — it is not a cap on replicas per node.
	got := Fit([]Node{node("n1", 64000, 262144), node("n2", 64000, 262144)},
		[]Pod{spreadReplica("p1"), spreadReplica("p2"), spreadReplica("p3")})
	if !got.Feasible() {
		t.Fatalf("a 2/1 split is within maxSkew=1, got %+v", got.Unplaced)
	}
}

func TestFit_TopologySpreadBlocksWhenTheOnlyBalancedDomainIsUnusable(t *testing.T) {
	// n3 is the domain that would keep the spread balanced, but it has no
	// room. n1 and n2 have room but taking a third replica there would push
	// the skew to 2, so the pod has nowhere to go.
	tiny := node("n3", 1, 1)
	got := Fit([]Node{node("n1", 64000, 262144), node("n2", 64000, 262144), tiny},
		[]Pod{spreadReplica("p1"), spreadReplica("p2"), spreadReplica("p3")})

	if got.Feasible() {
		t.Fatal("the third replica has no admissible domain")
	}
	if got.Unplaced[0].Reason != ReasonTopologySpread {
		t.Errorf("expected topology spread to be named as the dominant blocker, got %q (%s)",
			got.Unplaced[0].Reason, got.Unplaced[0].Detail)
	}
}

func TestFit_TopologySpreadScheduleAnywayNeverBlocks(t *testing.T) {
	makeReplica := func(name string) Pod {
		p := pod("default", name, 10, 16)
		p.Labels = map[string]string{"app": "api"}
		p.TopologySpread = []model.TopologySpreadConstraint{{
			MaxSkew:           1,
			TopologyKey:       "kubernetes.io/hostname",
			WhenUnsatisfiable: "ScheduleAnyway",
			LabelSelector:     &model.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
		}}
		return p
	}
	got := Fit([]Node{node("n1", 64000, 262144)},
		[]Pod{makeReplica("p1"), makeReplica("p2"), makeReplica("p3")})
	if !got.Feasible() {
		t.Fatalf("a soft spread constraint must never make a placement infeasible, got %+v", got.Unplaced)
	}
}

func TestFit_ExtendedResourcesAreEnforced(t *testing.T) {
	// Without extended-resource accounting a GPU pod appears to fit any node.
	plain := node("cpu-1", 8000, 16384)
	gpu := node("gpu-1", 8000, 16384)
	gpu.AllocatableExtended = map[string]int64{"nvidia.com/gpu": 1}

	p := pod("default", "trainer", 100, 128)
	p.Extended = map[string]int64{"nvidia.com/gpu": 1}

	got := Fit([]Node{plain, gpu}, []Pod{p})
	if !got.Feasible() {
		t.Fatalf("the GPU node should accept the pod, got %+v", got.Unplaced)
	}
	if got.Assignments[p.Key()] != "gpu-1" {
		t.Errorf("expected placement on the GPU node, got %q", got.Assignments[p.Key()])
	}
	if got := Fit([]Node{plain}, []Pod{p}); got.Feasible() {
		t.Fatal("a GPU pod must not be placed on a node with no GPU")
	}
}

func TestFit_PodCapacityCapIsEnforced(t *testing.T) {
	n := node("n1", 64000, 262144)
	n.MaxPods = 2
	pods := []Pod{pod("default", "a", 1, 1), pod("default", "b", 1, 1), pod("default", "c", 1, 1)}
	got := Fit([]Node{n}, pods)
	if got.Feasible() {
		t.Fatal("a third pod must not exceed the node's pod cap")
	}
	if got.Unplaced[0].Reason != ReasonPodCapacity {
		t.Errorf("expected the pod cap to be named, got %q", got.Unplaced[0].Reason)
	}
}

func TestFit_UnmodelledConstraintForcesIndeterminate(t *testing.T) {
	// The pod fits trivially. It must still not count as feasible, because
	// there is a required constraint the simulator cannot see.
	p := pod("default", "stateful", 10, 16)
	p.Unmodelled = []string{model.UnmodelledVolumeAffinity}

	got := Fit([]Node{node("n1", 64000, 262144)}, []Pod{p})
	if !got.Indeterminate {
		t.Fatal("an unmodelled constraint must make the result indeterminate")
	}
	if got.Feasible() {
		t.Fatal("indeterminate must never read as feasible — that is the whole safety property")
	}
	if len(got.Unplaced) != 0 {
		t.Error("the pod itself still fits; indeterminate is about confidence, not capacity")
	}
	if len(got.IndeterminateReasons) != 1 || got.IndeterminateReasons[0] != model.UnmodelledVolumeAffinity {
		t.Errorf("expected the reason to be reported, got %v", got.IndeterminateReasons)
	}
}

func TestFit_NoNodesRejectsEverything(t *testing.T) {
	got := Fit(nil, []Pod{pod("default", "a", 1, 1)})
	if got.Feasible() {
		t.Fatal("nothing can be placed on no nodes")
	}
	if got.Unplaced[0].Reason != ReasonNoNodes {
		t.Errorf("expected no_candidate_nodes, got %q", got.Unplaced[0].Reason)
	}
}

func TestFit_IsDeterministic(t *testing.T) {
	// The UI must not show a different verdict on every run for an
	// unchanged cluster.
	nodes := []Node{node("n1", 2000, 2048), node("n2", 2000, 2048), node("n3", 2000, 2048)}
	pods := []Pod{
		pod("default", "a", 500, 512), pod("default", "b", 500, 512),
		pod("default", "c", 900, 900), pod("default", "d", 100, 100),
		pod("default", "e", 500, 512), pod("default", "f", 700, 700),
	}
	first := Fit(nodes, pods)
	for i := 0; i < 20; i++ {
		again := Fit(nodes, pods)
		if len(again.Assignments) != len(first.Assignments) {
			t.Fatal("placement count varied between identical runs")
		}
		for key, node := range first.Assignments {
			if again.Assignments[key] != node {
				t.Fatalf("pod %s moved from %s to %s between identical runs", key, node, again.Assignments[key])
			}
		}
	}
}

func TestRequirementMatches_UnknownOperatorDoesNotPass(t *testing.T) {
	// A future operator we do not understand must fail closed.
	req := model.SelectorRequirement{Key: "k", Operator: "SomethingNew", Values: []string{"v"}}
	if requirementMatches(req, "v", true) {
		t.Error("an unrecognised operator must not match")
	}
}

func TestSelectorMatches_NilVersusEmpty(t *testing.T) {
	// The API's asymmetry: nil matches nothing, explicitly empty matches all.
	if selectorMatches(nil, map[string]string{"app": "web"}) {
		t.Error("a nil selector must match nothing")
	}
	if !selectorMatches(&model.LabelSelector{}, map[string]string{"app": "web"}) {
		t.Error("an empty selector must match everything")
	}
}

func TestTolerates_ExistsWithEmptyKeyToleratesEverything(t *testing.T) {
	taints := []model.Taint{{Key: "anything", Value: "x", Effect: "NoSchedule"}}
	tol := []model.Toleration{{Operator: "Exists"}}
	if !tolerates(tol, taints) {
		t.Error("Exists with an empty key tolerates all taints")
	}
}
