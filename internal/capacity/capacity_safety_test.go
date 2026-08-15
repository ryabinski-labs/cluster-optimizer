package capacity

import (
	"testing"

	"github.com/GipsyChef/cluster-optimizer/internal/model"
	"github.com/GipsyChef/cluster-optimizer/internal/usage"
)

// Every test here guards the same property from a different angle: the engine
// must never report more removable nodes than reality allows. A pessimistic
// answer costs a saving; an optimistic one costs an outage.

// A bare pod cannot be evicted, so it is not part of the placement problem —
// but it still occupies its host. If its footprint is not charged there, the
// simulator sees an empty node and packs relocatable pods into space that is
// already taken.
func TestAnalyze_BarePodOccupiesItsHost(t *testing.T) {
	snapshot := model.Snapshot{
		Nodes: []model.Node{
			node("node-a", "workers", 4000, 8192),
			node("node-b", "workers", 4000, 8192),
		},
		Pods: []model.Pod{
			// OwnerKind "" is a bare pod: unevictable, and it holds most of node-a.
			{Namespace: "ops", Name: "bare", NodeName: "node-a", Phase: "Running",
				RequestsCPUm: 500, RequestsMemoryMiB: 7000},
			pod("app", "web", "node-b", 500, 6000),
		},
	}

	result := Analyze(snapshot, p95(nil), Config{Floor: 1})
	verdict := poolOf(t, result, "workers")

	// node-a offers 8192*0.9 = 7372 MiB, of which the bare pod holds 7000.
	// The 6000 MiB web pod cannot join it, so both nodes are required.
	if verdict.RemovableNodes != 0 {
		t.Errorf("removable = %d, want 0: the bare pod leaves no room on node-a", verdict.RemovableNodes)
	}
	if verdict.MinimumSafeNodes != 2 {
		t.Errorf("minimum = %d, want 2", verdict.MinimumSafeNodes)
	}
}

// A mirror pod (a static pod, owned by the Node) is unevictable for the same
// reason and must be charged the same way.
func TestAnalyze_MirrorPodOccupiesItsHost(t *testing.T) {
	snapshot := model.Snapshot{
		Nodes: []model.Node{
			node("node-a", "cp", 4000, 8192),
			node("node-b", "cp", 4000, 8192),
		},
		Pods: []model.Pod{
			{Namespace: "kube-system", Name: "apiserver-a", NodeName: "node-a", Phase: "Running",
				OwnerKind: "Node", RequestsCPUm: 3500, RequestsMemoryMiB: 7000},
			pod("app", "web", "node-b", 500, 6000),
		},
	}

	verdict := poolOf(t, Analyze(snapshot, p95(nil), Config{Floor: 1}), "cp")
	if verdict.RemovableNodes != 0 {
		t.Errorf("removable = %d, want 0: the static pod pins node-a", verdict.RemovableNodes)
	}
}

// The scheduler will not place on a NotReady node, so its allocatable figure
// is not capacity. A pod that only fits there does not fit anywhere.
func TestAnalyze_NotReadyNodeOffersNoCapacity(t *testing.T) {
	dead := node("node-dead", "workers", 16000, 32768)
	dead.Ready = false

	snapshot := model.Snapshot{
		Nodes: []model.Node{dead, node("node-ok", "workers", 2000, 4096)},
		Pods: []model.Pod{
			// Only the dead node is large enough to hold this.
			pod("app", "heavy", "node-ok", 500, 20000),
		},
	}

	verdict := poolOf(t, Analyze(snapshot, p95(nil), Config{Floor: 1}), "workers")
	if verdict.RemovableNodes != 0 {
		t.Errorf("removable = %d, want 0: the only node large enough is NotReady", verdict.RemovableNodes)
	}
	if len(verdict.Blockers) == 0 {
		t.Error("expected a blocker naming why the workload does not fit")
	}
}

// A cordoned node is likewise not a placement target — including one this tool
// cordoned itself on a previous pass.
func TestAnalyze_CordonedNodeOffersNoCapacity(t *testing.T) {
	cordoned := node("node-cordoned", "workers", 16000, 32768)
	cordoned.Unschedulable = true

	snapshot := model.Snapshot{
		Nodes: []model.Node{cordoned, node("node-ok", "workers", 2000, 4096)},
		Pods:  []model.Pod{pod("app", "heavy", "node-ok", 500, 20000)},
	}

	verdict := poolOf(t, Analyze(snapshot, p95(nil), Config{Floor: 1}), "workers")
	if verdict.RemovableNodes != 0 {
		t.Errorf("removable = %d, want 0: the only node large enough is cordoned", verdict.RemovableNodes)
	}
}

// An unusable node that hosts nothing immovable is simply dead weight and
// should be reported as releasable.
func TestAnalyze_UnusableNodeWithNoPinnedWorkIsReleasable(t *testing.T) {
	dead := node("node-dead", "workers", 4000, 8192)
	dead.Ready = false

	snapshot := model.Snapshot{
		Nodes: []model.Node{dead, node("node-ok", "workers", 4000, 8192)},
		Pods:  []model.Pod{pod("app", "web", "node-ok", 100, 500)},
	}

	verdict := poolOf(t, Analyze(snapshot, p95(nil), Config{Floor: 1}), "workers")
	if verdict.RemovableNodes != 1 {
		t.Errorf("removable = %d, want 1: the NotReady node carries nothing", verdict.RemovableNodes)
	}
}

// An unusable node that strands an immovable pod cannot be handed back, even
// though it offers no capacity to anyone else.
func TestAnalyze_UnusableNodeStrandingAPinnedPodIsRetained(t *testing.T) {
	stranded := node("node-stranded", "workers", 4000, 8192)
	stranded.Unschedulable = true

	snapshot := model.Snapshot{
		Nodes: []model.Node{stranded, node("node-ok", "workers", 4000, 8192)},
		Pods: []model.Pod{
			{Namespace: "ops", Name: "bare", NodeName: "node-stranded", Phase: "Running",
				RequestsCPUm: 100, RequestsMemoryMiB: 100},
			pod("app", "web", "node-ok", 100, 500),
		},
	}

	verdict := poolOf(t, Analyze(snapshot, p95(nil), Config{Floor: 1}), "workers")
	if verdict.MinimumSafeNodes != 2 {
		t.Errorf("minimum = %d, want 2: the cordoned node still holds an unevictable pod", verdict.MinimumSafeNodes)
	}
	if verdict.RemovableNodes != 0 {
		t.Errorf("removable = %d, want 0", verdict.RemovableNodes)
	}
}

// The survive-one-loss gate has to test losing the node whose loss hurts most.
// In a heterogeneous pool that is the largest node, never the smallest.
func TestAnalyze_SurviveNodeLossTestsLosingTheLargestNode(t *testing.T) {
	snapshot := model.Snapshot{
		Nodes: []model.Node{
			node("big", "workers", 16000, 32768),
			node("small-1", "workers", 2000, 4096),
			node("small-2", "workers", 2000, 4096),
		},
		Pods: []model.Pod{
			// Only "big" can hold this pod at all.
			pod("app", "heavy", "big", 1000, 20000),
		},
	}

	verdict := poolOf(t, Analyze(snapshot, p95(nil), Config{Floor: 1, SurviveNodeLoss: true}), "workers")
	t.Logf("derived=%d minimum=%d removable=%d constraint=%q",
		verdict.DerivedMinimum, verdict.MinimumSafeNodes, verdict.RemovableNodes, verdict.BindingConstraint)

	// No subset of this pool survives losing "big", so nothing may be
	// released on the strength of a survive-one-loss claim.
	if verdict.RemovableNodes != 0 {
		t.Errorf("removable = %d, want 0: losing 'big' leaves nowhere for the 20 GiB pod", verdict.RemovableNodes)
	}
}

// The same gate must still permit a genuine saving in a homogeneous pool,
// where losing any node is equivalent.
func TestAnalyze_SurviveNodeLossStillAllowsRealSavings(t *testing.T) {
	snapshot := model.Snapshot{
		Nodes: []model.Node{
			node("a", "workers", 4000, 8192),
			node("b", "workers", 4000, 8192),
			node("c", "workers", 4000, 8192),
			node("d", "workers", 4000, 8192),
		},
		Pods: []model.Pod{pod("app", "web", "a", 100, 500)},
	}

	verdict := poolOf(t, Analyze(snapshot, p95(nil), Config{Floor: 1, SurviveNodeLoss: true}), "workers")
	if verdict.MinimumSafeNodes != 2 {
		t.Errorf("minimum = %d, want 2 (one node plus a spare)", verdict.MinimumSafeNodes)
	}
	if verdict.RemovableNodes != 2 {
		t.Errorf("removable = %d, want 2", verdict.RemovableNodes)
	}
}

// The result has to say whether the survival gate ran, so a consumer cannot
// present the number as carrying a guarantee that was never checked.
func TestAnalyze_ResultRecordsWhetherTheSurvivalGateRan(t *testing.T) {
	snapshot := model.Snapshot{
		Nodes: []model.Node{node("a", "w", 4000, 8192), node("b", "w", 4000, 8192)},
		Pods:  []model.Pod{pod("app", "web", "a", 100, 500)},
	}

	if got := Analyze(snapshot, p95(nil), Config{Floor: 1, SurviveNodeLoss: true}); !got.SurviveNodeLoss {
		t.Error("survive_node_loss = false after running with the gate enabled")
	}
	if got := Analyze(snapshot, p95(nil), Config{Floor: 1}); got.SurviveNodeLoss {
		t.Error("survive_node_loss = true after running with the gate disabled")
	}
}

// Observed usage must size a pinned pod the same way it sizes a movable one:
// a bare pod using far more than it requests still occupies what it uses.
func TestAnalyze_PinnedPodIsSizedByObservedUsage(t *testing.T) {
	snapshot := model.Snapshot{
		Nodes: []model.Node{
			node("node-a", "workers", 4000, 8192),
			node("node-b", "workers", 4000, 8192),
		},
		Pods: []model.Pod{
			// Requests almost nothing, actually uses 6 GiB.
			{Namespace: "ops", Name: "bare", NodeName: "node-a", Phase: "Running",
				RequestsCPUm: 10, RequestsMemoryMiB: 10},
			pod("app", "web", "node-b", 100, 1500),
		},
	}
	readings := map[string]usage.Reading{
		"ops/bare": {CPUm: 100, MemoryMiB: 6000, Samples: 2016},
	}

	verdict := poolOf(t, Analyze(snapshot, p95(readings), Config{Floor: 1}), "workers")
	// 7372 usable on node-a, minus 6000*1.2 = 7200 observed+headroom, leaves
	// 172 MiB — not enough for the 1500 MiB web pod.
	if verdict.RemovableNodes != 0 {
		t.Errorf("removable = %d, want 0: the bare pod's real footprint fills node-a", verdict.RemovableNodes)
	}
}
