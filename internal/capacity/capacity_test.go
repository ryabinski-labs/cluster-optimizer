package capacity

import (
	"testing"

	"github.com/GipsyChef/cluster-optimizer/internal/binpack"
	"github.com/GipsyChef/cluster-optimizer/internal/model"
	"github.com/GipsyChef/cluster-optimizer/internal/usage"
)

func node(name, pool string, cpu, mem int64) model.Node {
	return model.Node{
		Name: name, Pool: pool, Ready: true,
		AllocatableCPUm: cpu, AllocatableMemoryMiB: mem,
		Labels: map[string]string{"kubernetes.io/hostname": name},
	}
}

func pod(ns, name, nodeName string, cpu, mem int64) model.Pod {
	return model.Pod{
		Namespace: ns, Name: name, NodeName: nodeName, Phase: "Running",
		OwnerKind: "ReplicaSet", OwnerName: name + "-rs",
		RequestsCPUm: cpu, RequestsMemoryMiB: mem,
	}
}

// p95 is a usage set strong enough to be actionable, so tests exercise the
// real decision path rather than the "weak evidence" short-circuit.
func p95(readings map[string]usage.Reading) usage.Set {
	return usage.Set{Pods: readings, Fidelity: usage.FidelityHistoricalP95, Source: "test"}
}

func poolOf(t *testing.T, r Result, name string) PoolVerdict {
	t.Helper()
	for _, p := range r.Pools {
		if p.Pool == name {
			return p
		}
	}
	t.Fatalf("pool %q not found in %+v", name, r.Pools)
	return PoolVerdict{}
}

func TestAnalyze_IdlePoolShrinksToTheFloor(t *testing.T) {
	// Six nodes carrying almost nothing. The old two-node estimate would say
	// "2" here — and would also say "2" for a pool that genuinely needed six.
	snapshot := model.Snapshot{}
	for _, name := range []string{"n1", "n2", "n3", "n4", "n5", "n6"} {
		snapshot.Nodes = append(snapshot.Nodes, node(name, "pool-a", 4000, 8192))
		snapshot.Pods = append(snapshot.Pods, pod("default", "app-"+name, name, 100, 128))
	}

	got := Analyze(snapshot, p95(nil), Config{Floor: 2, SurviveNodeLoss: true})
	pool := poolOf(t, got, "pool-a")

	if pool.Status != StatusFits {
		t.Errorf("a pool that can give back nodes reads as fits, got %s", pool.Status)
	}
	if pool.MinimumSafeNodes != 2 {
		t.Errorf("expected a minimum of 2, got %d", pool.MinimumSafeNodes)
	}
	if pool.RemovableNodes != 4 {
		t.Errorf("expected 4 removable nodes, got %d", pool.RemovableNodes)
	}
	// The survival gate, not the configured floor, is what holds this at 2:
	// one node would hold the workload but not survive losing itself.
	if pool.DerivedMinimum != 2 {
		t.Errorf("expected the survival gate to derive 2, got %d", pool.DerivedMinimum)
	}
}

func TestAnalyze_AtFloorIsDistinctFromAtMinimum(t *testing.T) {
	// Three nearly-empty nodes with a configured floor of 3. Nothing is
	// removable, but the reason is the operator's floor, not the workload —
	// and the verdict has to say which, or an operator cannot tell a
	// correctly-sized cluster from an over-constrained one.
	snapshot := model.Snapshot{}
	for _, name := range []string{"n1", "n2", "n3"} {
		snapshot.Nodes = append(snapshot.Nodes, node(name, "pool-a", 8000, 16384))
		snapshot.Pods = append(snapshot.Pods, pod("default", "app-"+name, name, 50, 64))
	}

	got := Analyze(snapshot, p95(nil), Config{Floor: 3, SurviveNodeLoss: true})
	pool := poolOf(t, got, "pool-a")

	if pool.Status != StatusAtFloor {
		t.Fatalf("expected at_floor, got %s (derived=%d)", pool.Status, pool.DerivedMinimum)
	}
	if pool.RemovableNodes != 0 {
		t.Errorf("expected nothing removable at the floor, got %d", pool.RemovableNodes)
	}
	if pool.DerivedMinimum >= pool.CurrentNodes {
		t.Errorf("the workload should fit below the floor; derived=%d current=%d",
			pool.DerivedMinimum, pool.CurrentNodes)
	}
	if pool.BindingConstraint == "" {
		t.Error("at_floor must name the floor as the reason")
	}
}

func TestAnalyze_BusyPoolCannotShrink(t *testing.T) {
	// Three nodes each ~85% committed. Nothing can be removed.
	snapshot := model.Snapshot{}
	for _, name := range []string{"n1", "n2", "n3"} {
		snapshot.Nodes = append(snapshot.Nodes, node(name, "pool-a", 4000, 8192))
		snapshot.Pods = append(snapshot.Pods, pod("default", "app-"+name, name, 3400, 6900))
	}

	got := Analyze(snapshot, p95(nil), Config{Floor: 2, SurviveNodeLoss: true})
	pool := poolOf(t, got, "pool-a")

	if pool.Status != StatusAtMinimum {
		t.Errorf("expected at_minimum, got %s (min=%d)", pool.Status, pool.MinimumSafeNodes)
	}
	if pool.RemovableNodes != 0 {
		t.Errorf("expected nothing removable, got %d", pool.RemovableNodes)
	}
	if pool.BindingConstraint == "" {
		t.Error("a pool that cannot shrink must say what is stopping it")
	}
}

func TestAnalyze_PerPoolNotClusterAverage(t *testing.T) {
	// Two pools with very different shapes. A cluster average would describe
	// neither of them; the verdicts must be independent.
	snapshot := model.Snapshot{Nodes: []model.Node{
		node("small-1", "pool-small", 1000, 2048),
		node("small-2", "pool-small", 1000, 2048),
		node("small-3", "pool-small", 1000, 2048),
		node("big-1", "pool-big", 16000, 65536),
		node("big-2", "pool-big", 16000, 65536),
		node("big-3", "pool-big", 16000, 65536),
	}}
	// The small pool is full; the big pool is nearly empty.
	for _, n := range []string{"small-1", "small-2", "small-3"} {
		snapshot.Pods = append(snapshot.Pods, pod("default", "tight-"+n, n, 850, 1700))
	}
	for _, n := range []string{"big-1", "big-2", "big-3"} {
		snapshot.Pods = append(snapshot.Pods, pod("default", "idle-"+n, n, 50, 64))
	}

	got := Analyze(snapshot, p95(nil), Config{Floor: 2, SurviveNodeLoss: true})

	small := poolOf(t, got, "pool-small")
	big := poolOf(t, got, "pool-big")
	if small.RemovableNodes != 0 {
		t.Errorf("the full pool must not be shrinkable, got %d removable", small.RemovableNodes)
	}
	if big.RemovableNodes != 1 {
		t.Errorf("the idle pool should give back a node, got %d removable", big.RemovableNodes)
	}
	if got.CurrentNodes != 6 {
		t.Errorf("expected 6 current nodes, got %d", got.CurrentNodes)
	}
}

func TestAnalyze_AntiAffinitySetsTheFloorAboveTheConfiguredOne(t *testing.T) {
	// Four huge, nearly-empty nodes, but three replicas pinned one-per-host.
	// Capacity says 2; topology says 3. The safer number must win.
	snapshot := model.Snapshot{}
	for _, name := range []string{"n1", "n2", "n3", "n4"} {
		snapshot.Nodes = append(snapshot.Nodes, node(name, "pool-a", 32000, 131072))
	}
	for i, host := range []string{"n1", "n2", "n3"} {
		p := pod("default", "payments-"+string(rune('a'+i)), host, 10, 16)
		p.Labels = map[string]string{"app": "payments"}
		p.Affinity = &model.Affinity{RequiredPodAntiAffinity: []model.PodAffinityTerm{{
			TopologyKey:   "kubernetes.io/hostname",
			LabelSelector: &model.LabelSelector{MatchLabels: map[string]string{"app": "payments"}},
		}}}
		snapshot.Pods = append(snapshot.Pods, p)
	}

	got := Analyze(snapshot, p95(nil), Config{Floor: 2, SurviveNodeLoss: false})
	pool := poolOf(t, got, "pool-a")

	if pool.MinimumSafeNodes != 3 {
		t.Errorf("three host-anti-affine replicas need three nodes, got %d", pool.MinimumSafeNodes)
	}
	if pool.RemovableNodes != 1 {
		t.Errorf("expected exactly one removable node, got %d", pool.RemovableNodes)
	}
}

func TestAnalyze_SurviveNodeLossRaisesTheMinimum(t *testing.T) {
	// Two nodes, each half full. Everything fits on one node — but then
	// losing that node loses the workload.
	snapshot := model.Snapshot{Nodes: []model.Node{
		node("n1", "pool-a", 4000, 8192), node("n2", "pool-a", 4000, 8192),
	}, Pods: []model.Pod{
		pod("default", "a", "n1", 500, 1024),
		pod("default", "b", "n2", 500, 1024),
	}}

	without := Analyze(snapshot, p95(nil), Config{Floor: 1, SurviveNodeLoss: false})
	if got := poolOf(t, without, "pool-a").MinimumSafeNodes; got != 1 {
		t.Errorf("without the survival gate the workload fits one node, got %d", got)
	}

	with := Analyze(snapshot, p95(nil), Config{Floor: 1, SurviveNodeLoss: true})
	if got := poolOf(t, with, "pool-a").MinimumSafeNodes; got != 2 {
		t.Errorf("surviving one node loss requires two nodes, got %d", got)
	}
}

func TestAnalyze_UnmodelledConstraintBlocksTheWholePool(t *testing.T) {
	snapshot := model.Snapshot{Nodes: []model.Node{
		node("n1", "pool-a", 8000, 16384), node("n2", "pool-a", 8000, 16384),
		node("n3", "pool-a", 8000, 16384),
	}}
	idle := pod("default", "idle", "n1", 10, 16)
	stateful := pod("default", "db", "n2", 10, 16)
	stateful.Unmodelled = []string{model.UnmodelledVolumeAffinity}
	snapshot.Pods = append(snapshot.Pods, idle, stateful)

	got := Analyze(snapshot, p95(nil), Config{Floor: 2, SurviveNodeLoss: true})
	pool := poolOf(t, got, "pool-a")

	if pool.Status != StatusIndeterminate {
		t.Fatalf("one unmodelled pod must make the pool indeterminate, got %s", pool.Status)
	}
	if pool.Actionable {
		t.Error("an indeterminate pool must never be actionable")
	}
	if got.Actionable {
		t.Error("one indeterminate pool must make the whole cluster result non-actionable")
	}
	if pool.RemovableNodes != 0 {
		t.Errorf("an indeterminate pool must claim nothing removable, got %d", pool.RemovableNodes)
	}
	if got.MinimumSafeNodes != 3 {
		t.Errorf("an indeterminate pool must be assumed to need all its nodes, got %d", got.MinimumSafeNodes)
	}
}

func TestAnalyze_WeakUsageEvidenceIsReportedButNotActionable(t *testing.T) {
	// The numbers are still computed and shown; what changes is permission
	// to act on them.
	snapshot := model.Snapshot{Nodes: []model.Node{
		node("n1", "pool-a", 8000, 16384), node("n2", "pool-a", 8000, 16384),
		node("n3", "pool-a", 8000, 16384),
	}, Pods: []model.Pod{pod("default", "idle", "n1", 10, 16)}}

	instant := usage.Set{Pods: map[string]usage.Reading{}, Fidelity: usage.FidelityInstant, Source: "metrics-server"}
	got := Analyze(snapshot, instant, Config{Floor: 2, SurviveNodeLoss: true})
	pool := poolOf(t, got, "pool-a")

	if pool.RemovableNodes == 0 {
		t.Error("the verdict should still be computed and reported")
	}
	if pool.Actionable {
		t.Error("a single live sample must not authorise removing a node")
	}
	if got.Actionable {
		t.Error("the cluster result must inherit the non-actionable verdict")
	}
}

func TestAnalyze_ObservedUsageAboveRequestExpandsTheFootprint(t *testing.T) {
	// A pod requesting 100m but really using 3 cores must be modelled at its
	// real size, or consolidation plans it onto a node it will then crush.
	snapshot := model.Snapshot{Nodes: []model.Node{
		node("n1", "pool-a", 4000, 8192), node("n2", "pool-a", 4000, 8192),
	}, Pods: []model.Pod{
		pod("default", "liar", "n1", 100, 128),
		pod("default", "honest", "n2", 3000, 6000),
	}}

	// With requests alone, both fit one node.
	byRequest := Analyze(snapshot, p95(nil), Config{Floor: 1, SurviveNodeLoss: false})
	if got := poolOf(t, byRequest, "pool-a").MinimumSafeNodes; got != 1 {
		t.Fatalf("precondition: by requests alone this fits one node, got %d", got)
	}

	// With observed p95 showing the under-requesting pod's real appetite,
	// one node is no longer enough.
	observed := p95(map[string]usage.Reading{"default/liar": {CPUm: 3000, MemoryMiB: 4000}})
	byUsage := Analyze(snapshot, observed, Config{Floor: 1, SurviveNodeLoss: false})
	if got := poolOf(t, byUsage, "pool-a").MinimumSafeNodes; got != 2 {
		t.Errorf("observed usage should have raised the minimum to 2, got %d", got)
	}
}

func TestAnalyze_DaemonSetOverheadIsChargedToEveryRetainedNode(t *testing.T) {
	// DaemonSet pods do not move, so consolidation does not eliminate them —
	// each retained node still pays for its own copy.
	snapshot := model.Snapshot{Nodes: []model.Node{
		node("n1", "pool-a", 1000, 2048), node("n2", "pool-a", 1000, 2048),
	}}
	for _, host := range []string{"n1", "n2"} {
		ds := pod("kube-system", "agent-"+host, host, 400, 800)
		ds.OwnerKind = "DaemonSet"
		snapshot.Pods = append(snapshot.Pods, ds)
		snapshot.Pods = append(snapshot.Pods, pod("default", "app-"+host, host, 300, 600))
	}

	got := Analyze(snapshot, p95(nil), Config{Floor: 1, SurviveNodeLoss: false})
	pool := poolOf(t, got, "pool-a")
	// Per node: 1000m alloc, 10% headroom -> 900m, minus 400m DaemonSet
	// reservation -> 500m free. Two 300m app pods do not both fit.
	if pool.MinimumSafeNodes != 2 {
		t.Errorf("DaemonSet overhead should prevent collapsing to one node, got %d", pool.MinimumSafeNodes)
	}
}

func TestAnalyze_ImmovablePodPinsItsNode(t *testing.T) {
	snapshot := model.Snapshot{Nodes: []model.Node{
		node("n1", "pool-a", 8000, 16384), node("n2", "pool-a", 8000, 16384),
	}}
	bare := pod("default", "bare", "n2", 10, 16)
	bare.OwnerKind = "" // no controller: nothing would recreate it
	snapshot.Pods = append(snapshot.Pods, bare)

	got := Analyze(snapshot, p95(nil), Config{Floor: 1, SurviveNodeLoss: false})
	pool := poolOf(t, got, "pool-a")
	if pool.ImmovableNodes != 1 {
		t.Errorf("expected one node pinned by an unevictable pod, got %d", pool.ImmovableNodes)
	}
}

func TestAnalyze_HeadroomIsWithheldFromEachNode(t *testing.T) {
	// A plan that only works by packing a node to 100% of requests is not a
	// plan; the next rolling update has nowhere to put a surge pod.
	snapshot := model.Snapshot{Nodes: []model.Node{
		node("n1", "pool-a", 1000, 2048), node("n2", "pool-a", 1000, 2048),
	}, Pods: []model.Pod{
		pod("default", "a", "n1", 500, 1024),
		pod("default", "b", "n2", 450, 900),
	}}
	// 950m of requests against 1000m allocatable fits only with zero headroom.
	tight := Analyze(snapshot, p95(nil), Config{Floor: 1, SurviveNodeLoss: false, HeadroomPercent: 1})
	if got := poolOf(t, tight, "pool-a").MinimumSafeNodes; got != 1 {
		t.Fatalf("precondition: with ~no headroom this fits one node, got %d", got)
	}
	roomy := Analyze(snapshot, p95(nil), Config{Floor: 1, SurviveNodeLoss: false, HeadroomPercent: 20})
	if got := poolOf(t, roomy, "pool-a").MinimumSafeNodes; got != 2 {
		t.Errorf("20%% headroom should rule out the single-node plan, got %d", got)
	}
}

func TestAnalyze_FloorNeverLowersTheDerivedMinimum(t *testing.T) {
	// Configuration can make the answer safer, never riskier.
	snapshot := model.Snapshot{}
	for _, name := range []string{"n1", "n2", "n3"} {
		snapshot.Nodes = append(snapshot.Nodes, node(name, "pool-a", 1000, 2048))
		snapshot.Pods = append(snapshot.Pods, pod("default", "app-"+name, name, 800, 1600))
	}
	got := Analyze(snapshot, p95(nil), Config{Floor: 1, SurviveNodeLoss: false})
	pool := poolOf(t, got, "pool-a")
	if pool.MinimumSafeNodes < 3 {
		t.Errorf("a floor of 1 must not shrink a pool the workload genuinely fills, got %d", pool.MinimumSafeNodes)
	}
}

func TestAnalyze_BlockersAreRankedByHowMuchTheyBlock(t *testing.T) {
	snapshot := model.Snapshot{}
	for _, name := range []string{"n1", "n2", "n3"} {
		snapshot.Nodes = append(snapshot.Nodes, node(name, "pool-a", 1000, 2048))
	}
	for i, host := range []string{"n1", "n2", "n3"} {
		snapshot.Pods = append(snapshot.Pods, pod("default", "fat-"+string(rune('a'+i)), host, 850, 1700))
	}
	got := Analyze(snapshot, p95(nil), Config{Floor: 1, SurviveNodeLoss: false})
	pool := poolOf(t, got, "pool-a")

	if len(pool.Blockers) == 0 {
		t.Fatal("expected the blocking predicate to be reported")
	}
	if pool.Blockers[0].Reason != binpack.ReasonInsufficientMemory &&
		pool.Blockers[0].Reason != binpack.ReasonInsufficientCPU {
		t.Errorf("expected a resource constraint to be named, got %q", pool.Blockers[0].Reason)
	}
	if len(pool.Blockers[0].Examples) == 0 {
		t.Error("a blocker should name at least one affected pod")
	}
}

func TestAnalyze_EmptyClusterIsNotAnError(t *testing.T) {
	got := Analyze(model.Snapshot{}, p95(nil), Config{})
	if len(got.Pools) != 0 || got.CurrentNodes != 0 || got.MinimumSafeNodes != 0 {
		t.Errorf("expected an empty result, got %+v", got)
	}
}

func TestAnalyze_CompletedPodsDoNotOccupyCapacity(t *testing.T) {
	snapshot := model.Snapshot{Nodes: []model.Node{
		node("n1", "pool-a", 1000, 2048), node("n2", "pool-a", 1000, 2048),
	}}
	done := pod("default", "job", "n1", 900, 1900)
	done.Phase = "Succeeded"
	snapshot.Pods = append(snapshot.Pods, done, pod("default", "live", "n2", 100, 200))

	got := Analyze(snapshot, p95(nil), Config{Floor: 1, SurviveNodeLoss: false})
	if pool := poolOf(t, got, "pool-a"); pool.MinimumSafeNodes != 1 {
		t.Errorf("a completed pod reserves nothing, so one node suffices; got %d", pool.MinimumSafeNodes)
	}
}
