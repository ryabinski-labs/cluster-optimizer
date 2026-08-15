package binpack

import (
	"testing"

	"github.com/GipsyChef/cluster-optimizer/internal/model"
)

func minDomains(v int32) *int32 { return &v }

// spreadPod builds a pod carrying one DoNotSchedule spread constraint.
func spreadPod(name string, maxSkew int32, min *int32) Pod {
	return Pod{
		Namespace: "app", Name: name, Labels: map[string]string{"app": "web"},
		CPUm: 100, MemoryMiB: 128,
		TopologySpread: []model.TopologySpreadConstraint{{
			MaxSkew: maxSkew, TopologyKey: "zone", WhenUnsatisfiable: "DoNotSchedule",
			MinDomains:    min,
			LabelSelector: &model.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
		}},
	}
}

func zonedNodes() []Node {
	return []Node{
		{Name: "a", Labels: map[string]string{"zone": "z1"}, AllocatableCPUm: 8000, AllocatableMemoryMiB: 16384},
		{Name: "b", Labels: map[string]string{"zone": "z2"}, AllocatableCPUm: 8000, AllocatableMemoryMiB: 16384},
	}
}

// minDomains tightens a spread constraint: when fewer eligible domains exist
// than it demands, the scheduler treats the global minimum as zero, so the
// candidate domain's own count becomes the skew. Ignoring the field
// understates skew and admits placements the scheduler refuses.
func TestFit_MinDomainsTightensTheSpreadConstraint(t *testing.T) {
	nodes := zonedNodes()
	pods := []Pod{
		spreadPod("w1", 1, minDomains(3)),
		spreadPod("w2", 1, minDomains(3)),
		spreadPod("w3", 1, minDomains(3)),
	}

	got := Fit(nodes, pods)
	if len(got.Unplaced) == 0 {
		t.Fatalf("expected the third pod to be refused, placed all: %v", got.Assignments)
	}
	if got.Unplaced[0].Reason != ReasonTopologySpread {
		t.Errorf("reason = %q, want %q", got.Unplaced[0].Reason, ReasonTopologySpread)
	}
}

// Without minDomains the same three pods fit: two domains at 2 and 1 is a
// skew of 1, which maxSkew=1 permits. This is the control for the test above.
func TestFit_SpreadWithoutMinDomainsStillPermitsAnAllowedSkew(t *testing.T) {
	pods := []Pod{
		spreadPod("w1", 1, nil),
		spreadPod("w2", 1, nil),
		spreadPod("w3", 1, nil),
	}
	if got := Fit(zonedNodes(), pods); len(got.Unplaced) != 0 {
		t.Errorf("expected all three placed, unplaced: %+v", got.Unplaced)
	}
}

// A minDomains the cluster satisfies must not tighten anything.
func TestFit_SatisfiedMinDomainsDoesNotTighten(t *testing.T) {
	pods := []Pod{
		spreadPod("w1", 1, minDomains(2)),
		spreadPod("w2", 1, minDomains(2)),
		spreadPod("w3", 1, minDomains(2)),
	}
	if got := Fit(zonedNodes(), pods); len(got.Unplaced) != 0 {
		t.Errorf("two domains satisfy minDomains=2; expected all placed, unplaced: %+v", got.Unplaced)
	}
}

// The API contract: a null or empty node selector term matches no objects.
// Reading it as "no constraint" would be the optimistic interpretation of a
// pod the scheduler can in fact place nowhere.
func TestFit_EmptyNodeSelectorTermMatchesNoNode(t *testing.T) {
	nodes := []Node{node("n1", 8000, 16384)}
	p := pod("app", "w", 100, 128)
	p.Affinity = &model.Affinity{RequiredNodeAffinity: []model.NodeSelectorTerm{{}}}

	got := Fit(nodes, []Pod{p})
	if len(got.Unplaced) != 1 {
		t.Fatalf("expected the pod to be unplaceable, got %v", got.Assignments)
	}
	if got.Unplaced[0].Reason != ReasonNodeAffinity {
		t.Errorf("reason = %q, want %q", got.Unplaced[0].Reason, ReasonNodeAffinity)
	}
}

// An empty term among several real ones must not rescue the pod either: terms
// are ORed, and an empty one contributes no match.
func TestFit_EmptyTermDoesNotSatisfyAnOrListOfTerms(t *testing.T) {
	nodes := []Node{node("n1", 8000, 16384)}
	p := pod("app", "w", 100, 128)
	p.Affinity = &model.Affinity{RequiredNodeAffinity: []model.NodeSelectorTerm{
		{MatchExpressions: []model.SelectorRequirement{{Key: "role", Operator: "In", Values: []string{"db"}}}},
		{},
	}}

	if got := Fit(nodes, []Pod{p}); len(got.Unplaced) != 1 {
		t.Errorf("expected the pod to be unplaceable, got %v", got.Assignments)
	}
}

// A toleration scoped to one effect must not admit a taint with another.
func TestFit_TolerationEffectMustMatchTheTaint(t *testing.T) {
	nodes := []Node{{
		Name: "tainted", AllocatableCPUm: 8000, AllocatableMemoryMiB: 16384,
		Taints: []model.Taint{{Key: "dedicated", Value: "db", Effect: "NoExecute"}},
	}}
	p := pod("app", "w", 100, 128)
	p.Tolerations = []model.Toleration{
		{Key: "dedicated", Operator: "Equal", Value: "db", Effect: "NoSchedule"},
	}

	got := Fit(nodes, []Pod{p})
	if len(got.Unplaced) != 1 {
		t.Fatalf("a NoSchedule-only toleration must not admit a NoExecute taint, got %v", got.Assignments)
	}
	if got.Unplaced[0].Reason != ReasonTaint {
		t.Errorf("reason = %q, want %q", got.Unplaced[0].Reason, ReasonTaint)
	}
}

// A toleration with an empty effect tolerates every effect of that key.
func TestFit_EmptyTolerationEffectToleratesAnyEffect(t *testing.T) {
	nodes := []Node{{
		Name: "tainted", AllocatableCPUm: 8000, AllocatableMemoryMiB: 16384,
		Taints: []model.Taint{{Key: "dedicated", Value: "db", Effect: "NoExecute"}},
	}}
	p := pod("app", "w", 100, 128)
	p.Tolerations = []model.Toleration{{Key: "dedicated", Operator: "Equal", Value: "db"}}

	if got := Fit(nodes, []Pod{p}); len(got.Unplaced) != 0 {
		t.Errorf("expected the pod placed, unplaced: %+v", got.Unplaced)
	}
}

// The same snapshot must always produce the same placement. A verdict that
// flickers between runs is one an operator cannot act on, and the search
// depends on repeated Fit calls agreeing with each other.
func TestFit_IsDeterministicAtScale(t *testing.T) {
	zones := []string{"z1", "z2", "z3"}
	nodes := make([]Node, 0, 40)
	for i := 0; i < 40; i++ {
		nodes = append(nodes, Node{
			Name:            "node-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			Labels:          map[string]string{"zone": zones[i%3]},
			AllocatableCPUm: 4000, AllocatableMemoryMiB: 8192, MaxPods: 110,
		})
	}
	pods := make([]Pod, 0, 400)
	for i := 0; i < 400; i++ {
		pods = append(pods, Pod{
			Namespace: "ns", Name: "pod-" + string(rune('a'+i%26)) + string(rune('0'+i/26)),
			CPUm: int64(50 + i%200), MemoryMiB: int64(64 + i%512),
			Labels: map[string]string{"app": "x"},
		})
	}

	first := Fit(nodes, pods)
	for run := 0; run < 25; run++ {
		got := Fit(nodes, pods)
		if len(got.Unplaced) != len(first.Unplaced) {
			t.Fatalf("run %d: unplaced %d, want %d", run, len(got.Unplaced), len(first.Unplaced))
		}
		for key, want := range first.Assignments {
			if got.Assignments[key] != want {
				t.Fatalf("run %d: pod %s moved from %s to %s", run, key, want, got.Assignments[key])
			}
		}
	}
}
