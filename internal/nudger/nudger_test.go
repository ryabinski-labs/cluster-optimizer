package nudger

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GipsyChef/cluster-optimizer/internal/capacity"
	"github.com/GipsyChef/cluster-optimizer/internal/usage"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// readyNode builds an ordinary, usable node. Tests that expect a drain
// candidate must use it: the collector's Ready rule treats a node with no
// Ready condition as not-ready, and the nudger applies that same rule before
// it will consider a node for emptying.
func readyNode(name, cpu, memory string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(memory),
			},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

// poolNode is readyNode with a provider pool label, for tests about per-pool
// gating.
func poolNode(name, pool, cpu, memory string) *corev1.Node {
	node := readyNode(name, cpu, memory)
	node.Labels = map[string]string{"doks.digitalocean.com/node-pool": pool}
	return node
}

// controlledPod builds a controller-owned, running pod scheduled on a node.
func controlledPod(name, namespace, nodeName string, labels map[string]string, cpu, memory string) *corev1.Pod {
	isController := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: name + "-rs", Controller: &isController}},
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(memory),
			}}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// drainableCapacity is a same-run verdict that authorises draining one node
// from the named pool: actionable evidence, two usable nodes, a floor of one.
// Tests asserting that a drain happens attach it through liveOpts; tests
// asserting a refusal mutate it to model the engine's refusal instead.
func drainableCapacity(pool string) *capacity.Result {
	return &capacity.Result{
		Actionable: true,
		Pools: []capacity.PoolVerdict{{
			Pool:             pool,
			CurrentNodes:     2,
			MinimumSafeNodes: 1,
			RemovableNodes:   1,
			DerivedMinimum:   1,
			Status:           capacity.StatusFits,
			Actionable:       true,
			UsageFidelity:    usage.FidelityHistoricalP95,
		}},
	}
}

// liveOpts returns an Options that flips the dry-run gate off and supplies
// the capacity verdict the mutating path now requires, so existing
// cordon+evict assertions keep working.
func liveOpts() Options {
	opts := NewOptions()
	opts.Live = true
	opts.Capacity = drainableCapacity("default")
	return opts
}

// cordonedOrEvicted reports whether a nudge pass mutated anything on the
// given fake clientset.
func cordonedOrEvicted(t *testing.T, clientset *fake.Clientset, nodeNames ...string) bool {
	t.Helper()
	mutated := false
	for _, name := range nodeNames {
		node, err := clientset.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get node %s: %v", name, err)
		}
		if node.Spec.Unschedulable {
			mutated = true
		}
	}
	for _, action := range clientset.Actions() {
		if action.GetVerb() == "create" && action.GetSubresource() == "eviction" {
			mutated = true
		}
	}
	return mutated
}

func TestNudgePodsWithResult_FeasibleReportsEvictedAndTarget(t *testing.T) {
	node1 := readyNode("node-1", "2", "4Gi")
	node2 := readyNode("node-2", "2", "4Gi")
	pod := controlledPod("web-pod", "default", "node-1", nil, "500m", "512Mi")
	clientset := fake.NewSimpleClientset(node1, node2, pod)
	result, err := NudgePodsWithResult(context.Background(), clientset, liveOpts())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Mode != "live" {
		t.Fatalf("expected live mode, got %q", result.Mode)
	}
	if result.TargetNode == "" {
		t.Fatal("expected a target node to be captured in result")
	}
	if result.Evicted != 1 {
		t.Fatalf("expected 1 evicted, got %d", result.Evicted)
	}
	if result.Halted {
		t.Fatal("result should not be halted")
	}
}

func TestNudgePodsWithResult_HaltedSetsHaltedFlag(t *testing.T) {
	node1 := readyNode("node-1", "2", "4Gi")
	node2 := readyNode("node-2", "2", "4Gi")
	halt := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-optimizer-halt", Namespace: "cluster-optimizer"},
		Data:       map[string]string{"halt": "true"},
	}
	clientset := fake.NewSimpleClientset(node1, node2, halt)
	result, err := NudgePodsWithResult(context.Background(), clientset, liveOpts())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Halted {
		t.Fatal("expected halted=true when halt configmap is set")
	}
	if result.HaltReason == "" {
		t.Fatal("expected HaltReason to be populated")
	}
}

func TestNudgePods_Feasible(t *testing.T) {
	node1 := readyNode("node-1", "2", "4Gi")
	node2 := readyNode("node-2", "2", "4Gi")
	pod1 := controlledPod("web-pod", "default", "node-1", nil, "500m", "512Mi")

	clientset := fake.NewSimpleClientset(node1, node2, pod1)

	// Run NudgePods
	err := NudgePods(context.Background(), clientset, liveOpts())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// Check if node-1 (least loaded node) was cordoned
	updatedNode1, err := clientset.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get node-1: %v", err)
	}
	if !updatedNode1.Spec.Unschedulable {
		t.Error("expected node-1 to be cordoned (Unschedulable = true)")
	}

	// Check if node-2 was NOT cordoned
	updatedNode2, err := clientset.CoreV1().Nodes().Get(context.Background(), "node-2", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get node-2: %v", err)
	}
	if updatedNode2.Spec.Unschedulable {
		t.Error("expected node-2 to NOT be cordoned")
	}

	// Check that we attempted eviction on pod1
	actions := clientset.Actions()
	evictionTriggered := false
	for _, action := range actions {
		if action.GetVerb() == "create" && action.GetResource().Resource == "pods" && action.GetSubresource() == "eviction" {
			evictionTriggered = true
			break
		}
	}
	if !evictionTriggered {
		t.Error("expected an eviction action to be triggered for pod1")
	}
}

func TestNudgePods_NotFeasible(t *testing.T) {
	// Create fake nodes where node-2 has no available capacity
	node1 := readyNode("node-1", "1", "1Gi")
	node2 := readyNode("node-2", "1", "1Gi")

	// Pod 1 on node-1 has 800m CPU. Pod 2 on node-2 takes up almost all of
	// node-2, so neither can move.
	pod1 := controlledPod("web-pod", "default", "node-1", nil, "800m", "512Mi")
	pod2 := controlledPod("other-pod", "default", "node-2", nil, "800m", "512Mi")

	clientset := fake.NewSimpleClientset(node1, node2, pod1, pod2)

	err := NudgePods(context.Background(), clientset, liveOpts())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// Since pod1 (800m CPU) won't fit on node-2 (only has 200m CPU remaining: 1000m - 800m),
	// node-1 should NOT be cordoned or evicted.
	updatedNode1, err := clientset.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get node-1: %v", err)
	}
	if updatedNode1.Spec.Unschedulable {
		t.Error("expected node-1 to NOT be cordoned because consolidation is not feasible")
	}
}

func TestNudgePods_SkipBareAndDaemonSetPods(t *testing.T) {
	node1 := readyNode("node-1", "2", "4Gi")
	node2 := readyNode("node-2", "2", "4Gi")

	// Pod with DaemonSet owner (not relocatable)
	isController := true
	dsPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ds-pod",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "DaemonSet",
					Name:       "some-ds",
					Controller: &isController,
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	// Bare pod with no owner references (not relocatable)
	barePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bare-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("128Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	clientset := fake.NewSimpleClientset(node1, node2, dsPod, barePod)

	err := NudgePods(context.Background(), clientset, liveOpts())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// Neither pod was relocatable, so node-1 should NOT be cordoned
	updatedNode1, err := clientset.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get node-1: %v", err)
	}
	if updatedNode1.Spec.Unschedulable {
		t.Error("expected node-1 to NOT be cordoned because no relocatable pods were found")
	}
}

func TestNudgePods_DryRunDoesNotCordon(t *testing.T) {
	node1 := readyNode("node-1", "2", "4Gi")
	node2 := readyNode("node-2", "2", "4Gi")
	pod := controlledPod("web-pod", "default", "node-1", nil, "500m", "512Mi")
	clientset := fake.NewSimpleClientset(node1, node2, pod)
	// Default opts: dry-run (Live=false), plus the capacity verdict the
	// feasibility gate reads.
	opts := NewOptions()
	opts.Capacity = drainableCapacity("default")
	if err := NudgePods(context.Background(), clientset, opts); err != nil {
		t.Fatalf("dry-run nudge errored: %v", err)
	}
	updated, _ := clientset.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if updated.Spec.Unschedulable {
		t.Fatal("dry-run nudge must not cordon node-1")
	}
	for _, action := range clientset.Actions() {
		if action.GetVerb() == "create" && action.GetSubresource() == "eviction" {
			t.Fatal("dry-run nudge must not issue eviction")
		}
		if action.GetVerb() == "update" && action.GetResource().Resource == "nodes" {
			t.Fatal("dry-run nudge must not update nodes")
		}
	}
}

func TestNudgePods_HaltSwitchAborts(t *testing.T) {
	node1 := readyNode("node-1", "2", "4Gi")
	node2 := readyNode("node-2", "2", "4Gi")
	pod := controlledPod("web-pod", "default", "node-1", nil, "500m", "512Mi")
	halt := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-optimizer-halt", Namespace: "cluster-optimizer"},
		Data:       map[string]string{"halt": "true"},
	}
	clientset := fake.NewSimpleClientset(node1, node2, pod, halt)
	if err := NudgePods(context.Background(), clientset, liveOpts()); err != nil {
		t.Fatalf("nudger should return cleanly when halted: %v", err)
	}
	updated, _ := clientset.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if updated.Spec.Unschedulable {
		t.Fatal("halt switch must prevent cordon")
	}
}

func TestNudgePods_SkipsWhenPDBWouldBlock(t *testing.T) {
	node1 := readyNode("node-1", "2", "4Gi")
	node2 := readyNode("node-2", "2", "4Gi")
	pod := controlledPod("web-pod", "default", "node-1", map[string]string{"app": "web"}, "500m", "512Mi")
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "web-pdb", Namespace: "default"},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}},
		Status:     policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0},
	}
	clientset := fake.NewSimpleClientset(node1, node2, pod, pdb)
	if err := NudgePods(context.Background(), clientset, liveOpts()); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	updated, _ := clientset.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if updated.Spec.Unschedulable {
		t.Fatal("PDB should have blocked consolidation; node should not be cordoned")
	}
}

// The mutating path must not run on its own weaker model: without the same
// run's capacity verdict it has no authority to cordon anything.
func TestNudgePods_RefusesWithoutCapacityVerdict(t *testing.T) {
	node1 := readyNode("node-1", "2", "4Gi")
	node2 := readyNode("node-2", "2", "4Gi")
	pod := controlledPod("web-pod", "default", "node-1", nil, "500m", "512Mi")
	clientset := fake.NewSimpleClientset(node1, node2, pod)

	opts := NewOptions()
	opts.Live = true // no Capacity verdict
	result, err := NudgePodsWithResult(context.Background(), clientset, opts)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.NotFeasibleReason == "" {
		t.Fatal("expected a refusal reason when no capacity verdict is supplied")
	}
	if cordonedOrEvicted(t, clientset, "node-1", "node-2") {
		t.Fatal("no drain may run without a capacity verdict")
	}
}

// A verdict the engine marked non-actionable (weak usage evidence) is a
// refusal, not a maybe: the same run's report already said "advisory only".
func TestNudgePods_RefusesWhenVerdictNotActionable(t *testing.T) {
	node1 := readyNode("node-1", "2", "4Gi")
	node2 := readyNode("node-2", "2", "4Gi")
	pod := controlledPod("web-pod", "default", "node-1", nil, "500m", "512Mi")
	clientset := fake.NewSimpleClientset(node1, node2, pod)

	verdict := drainableCapacity("default")
	verdict.Actionable = false
	verdict.Pools[0].Actionable = false
	verdict.Pools[0].UsageFidelity = usage.FidelityInstant
	opts := liveOpts()
	opts.Capacity = verdict

	result, err := NudgePodsWithResult(context.Background(), clientset, opts)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !strings.Contains(result.NotFeasibleReason, "not actionable") {
		t.Fatalf("expected a not-actionable refusal reason, got %q", result.NotFeasibleReason)
	}
	if cordonedOrEvicted(t, clientset, "node-1", "node-2") {
		t.Fatal("a non-actionable verdict must not authorise a drain")
	}
}

// An actionable verdict with no spare node is still a refusal: this is the
// exact state the reported churn ran in (removable_nodes=0).
func TestNudgePods_RefusesWhenPoolHasNoSpareNode(t *testing.T) {
	node1 := readyNode("node-1", "2", "4Gi")
	node2 := readyNode("node-2", "2", "4Gi")
	pod := controlledPod("web-pod", "default", "node-1", nil, "500m", "512Mi")
	clientset := fake.NewSimpleClientset(node1, node2, pod)

	verdict := drainableCapacity("default")
	verdict.Pools[0].MinimumSafeNodes = 2
	verdict.Pools[0].DerivedMinimum = 2
	verdict.Pools[0].RemovableNodes = 0
	opts := liveOpts()
	opts.Capacity = verdict

	result, err := NudgePodsWithResult(context.Background(), clientset, opts)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !strings.Contains(result.NotFeasibleReason, "no spare") {
		t.Fatalf("expected a no-spare refusal reason, got %q", result.NotFeasibleReason)
	}
	if cordonedOrEvicted(t, clientset, "node-1", "node-2") {
		t.Fatal("a pool with no spare node must not be drained")
	}
}

// A node whose pool the capacity engine never evaluated is not drainable:
// silently treating "no verdict" as permission would defeat the gate for any
// pool-identity mismatch.
func TestNudgePods_RefusesWhenVerdictNamesAnotherPool(t *testing.T) {
	node1 := poolNode("node-1", "pool-x", "2", "4Gi")
	node2 := poolNode("node-2", "pool-x", "2", "4Gi")
	pod := controlledPod("web-pod", "default", "node-1", nil, "500m", "512Mi")
	clientset := fake.NewSimpleClientset(node1, node2, pod)

	// liveOpts authorises the "default" pool only; these nodes live in pool-x.
	result, err := NudgePodsWithResult(context.Background(), clientset, liveOpts())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !strings.Contains(result.NotFeasibleReason, "no verdict") {
		t.Fatalf("expected a missing-verdict refusal reason, got %q", result.NotFeasibleReason)
	}
	if cordonedOrEvicted(t, clientset, "node-1", "node-2") {
		t.Fatal("a pool the capacity engine never evaluated must not be drained")
	}
}

// A pool already carrying a cordoned or NotReady node must be returned to
// health before another of its nodes is emptied; draining into the gap would
// take the pool below the minimum the engine promised.
func TestNudgePods_UnusableNodeBenchesItsPool(t *testing.T) {
	notReady := readyNode("node-dead", "2", "4Gi")
	notReady.Status.Conditions = nil // no Ready condition: not capacity
	node2 := readyNode("node-2", "2", "4Gi")
	node3 := readyNode("node-3", "2", "4Gi")
	pod := controlledPod("web-pod", "default", "node-2", nil, "500m", "512Mi")
	clientset := fake.NewSimpleClientset(notReady, node2, node3, pod)

	result, err := NudgePodsWithResult(context.Background(), clientset, liveOpts())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !strings.Contains(result.NotFeasibleReason, "not Ready") {
		t.Fatalf("expected a pool-health refusal reason, got %q", result.NotFeasibleReason)
	}
	if cordonedOrEvicted(t, clientset, "node-2", "node-3") {
		t.Fatal("a pool holding an unusable node must not be drained")
	}
}

// One reaped cordon benches its whole pool: the signal is that nothing is
// removing drained nodes, not that one particular node is bad, so the next
// run must not simply drain a different node in the same pool.
func TestNudgePods_ReapCooldownBenchesWholePool(t *testing.T) {
	reaped := readyNode("node-1", "2", "4Gi")
	reaped.Annotations = map[string]string{
		AnnotationCordonReapedAt: time.Now().UTC().Format(time.RFC3339),
	}
	node2 := readyNode("node-2", "2", "4Gi")
	pod := controlledPod("web-pod", "default", "node-2", nil, "500m", "512Mi")
	clientset := fake.NewSimpleClientset(reaped, node2, pod)

	result, err := NudgePodsWithResult(context.Background(), clientset, liveOpts())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !strings.Contains(result.NotFeasibleReason, "reaped") {
		t.Fatalf("expected a reap-cooldown refusal reason, got %q", result.NotFeasibleReason)
	}
	if cordonedOrEvicted(t, clientset, "node-1", "node-2") {
		t.Fatal("a pool with a recently reaped cordon must not be drained")
	}
}

// The bench is per pool: a reap in one pool must not freeze a different,
// healthy pool.
func TestNudgePods_ReapCooldownIsScopedToThePool(t *testing.T) {
	reapedA := poolNode("a-1", "pool-a", "2", "4Gi")
	reapedA.Annotations = map[string]string{
		AnnotationCordonReapedAt: time.Now().UTC().Format(time.RFC3339),
	}
	poolA2 := poolNode("a-2", "pool-a", "2", "4Gi")
	poolB1 := poolNode("b-1", "pool-b", "2", "4Gi")
	poolB2 := poolNode("b-2", "pool-b", "2", "4Gi")
	pod := controlledPod("web-pod", "default", "b-1", nil, "500m", "512Mi")
	clientset := fake.NewSimpleClientset(reapedA, poolA2, poolB1, poolB2, pod)

	verdict := &capacity.Result{
		Actionable: true,
		Pools: []capacity.PoolVerdict{
			{Pool: "pool-a", CurrentNodes: 2, MinimumSafeNodes: 1, RemovableNodes: 1, Status: capacity.StatusFits, Actionable: true, UsageFidelity: usage.FidelityHistoricalP95},
			{Pool: "pool-b", CurrentNodes: 2, MinimumSafeNodes: 1, RemovableNodes: 1, Status: capacity.StatusFits, Actionable: true, UsageFidelity: usage.FidelityHistoricalP95},
		},
	}
	opts := liveOpts()
	opts.Capacity = verdict

	result, err := NudgePodsWithResult(context.Background(), clientset, opts)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.TargetNode != "b-1" {
		t.Fatalf("expected pool-b's node to be drained, got target %q", result.TargetNode)
	}
}

// The optimizer never drains the node its own pod is running on: evicting
// itself kills the run before any audit row is written, and the cordon it
// placed then sits until the reaper with nothing to explain it.
func TestNudgePods_NeverDrainsItsOwnNode(t *testing.T) {
	node1 := readyNode("node-1", "2", "4Gi")
	node2 := readyNode("node-2", "2", "4Gi")
	// Controller-owned and otherwise perfectly relocatable, so only the
	// self-namespace rule stops it from being evicted.
	selfPod := controlledPod("cluster-optimizer-29821080-gldv4", "cluster-optimizer", "node-1", nil, "100m", "128Mi")
	clientset := fake.NewSimpleClientset(node1, node2, selfPod)

	result, err := NudgePodsWithResult(context.Background(), clientset, liveOpts())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.TargetNode != "" {
		t.Fatalf("the optimizer's own node must not be a drain target, got %q", result.TargetNode)
	}
	if cordonedOrEvicted(t, clientset, "node-1", "node-2") {
		t.Fatal("a node hosting the optimizer's own pod must not be cordoned or evicted")
	}
	if _, err := clientset.CoreV1().Pods("cluster-optimizer").Get(context.Background(), selfPod.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("the optimizer's own pod must survive the pass: %v", err)
	}
}

// Self-exclusion is per node, not per pool: skipping the node the tool runs
// on must not stop it consolidating the rest of the pool.
func TestNudgePods_SelfNodeSkipStillConsolidatesOtherNodes(t *testing.T) {
	node1 := readyNode("node-1", "4", "8Gi")
	node2 := readyNode("node-2", "4", "8Gi")
	node3 := readyNode("node-3", "4", "8Gi")
	selfPod := controlledPod("cluster-optimizer-job", "cluster-optimizer", "node-1", nil, "100m", "128Mi")
	appPod := controlledPod("web-pod", "default", "node-2", nil, "500m", "512Mi")
	clientset := fake.NewSimpleClientset(node1, node2, node3, selfPod, appPod)

	result, err := NudgePodsWithResult(context.Background(), clientset, liveOpts())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.TargetNode != "node-2" {
		t.Fatalf("expected node-2 to be drained while node-1 (self) is skipped, got %q", result.TargetNode)
	}
	selfNode, _ := clientset.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if selfNode.Spec.Unschedulable {
		t.Fatal("the optimizer's own node must never be cordoned")
	}
}

// The self pod's footprint stays charged to its host: not being relocatable
// does not make the capacity it occupies available to other pods.
func TestNudgePods_SelfPodFootprintChargedToHost(t *testing.T) {
	node1 := readyNode("node-1", "2", "2Gi")
	node2 := readyNode("node-2", "2", "2Gi")
	selfPod := controlledPod("cluster-optimizer-job", "cluster-optimizer", "node-1", nil, "100m", "1536Mi")
	appPod := controlledPod("web-pod", "default", "node-2", nil, "100m", "1024Mi")
	clientset := fake.NewSimpleClientset(node1, node2, selfPod, appPod)

	result, err := NudgePodsWithResult(context.Background(), clientset, liveOpts())
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	// node-1 has only 512Mi free after the self pod, so the 1024Mi app pod
	// cannot land there and node-2 must stay put.
	if result.TargetNode != "" {
		t.Fatalf("expected no target when the self pod fills its host, got %q", result.TargetNode)
	}
	if cordonedOrEvicted(t, clientset, "node-2") {
		t.Fatal("node-2 must not be drained when its pod has nowhere to go")
	}
}
