package nudger

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

var reapNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

// cordonedNode builds a node that looks like this tool cordoned it `age` ago.
func cordonedNode(name string, age time.Duration, runID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				AnnotationCordonedAt:         reapNow.Add(-age).Format(time.RFC3339),
				AnnotationCordonedByRun:      runID,
				AnnotationPriorUnschedulable: "false",
			},
		},
		Spec: corev1.NodeSpec{Unschedulable: true},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi"),
		}},
	}
}

func TestReapStaleCordons_ReversesAbandonedCordon(t *testing.T) {
	node := cordonedNode("node-1", 2*time.Hour, "dead-run")
	clientset := fake.NewSimpleClientset(node)

	result := ReapStaleCordons(context.Background(), clientset, true, "this-run", DefaultCordonTTL, reapNow)

	if len(result.Uncordoned) != 1 || result.Uncordoned[0] != "node-1" {
		t.Fatalf("expected node-1 to be uncordoned, got %+v", result)
	}
	updated, err := clientset.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node-1: %v", err)
	}
	if updated.Spec.Unschedulable {
		t.Error("expected node-1 to be schedulable again after reaping")
	}
	for _, key := range []string{AnnotationCordonedAt, AnnotationCordonedByRun, AnnotationPriorUnschedulable} {
		if _, present := updated.Annotations[key]; present {
			t.Errorf("expected %s to be stripped after reaping", key)
		}
	}
	if updated.Annotations[AnnotationCordonReapedAt] == "" {
		t.Error("expected the reap timestamp to be stamped so the cooldown applies")
	}
}

func TestReapStaleCordons_HoldsCordonWithinTTL(t *testing.T) {
	node := cordonedNode("node-1", 5*time.Minute, "dead-run")
	clientset := fake.NewSimpleClientset(node)

	result := ReapStaleCordons(context.Background(), clientset, true, "this-run", DefaultCordonTTL, reapNow)

	if len(result.Uncordoned) != 0 {
		t.Fatalf("a 5-minute-old cordon is not stale; got %+v", result.Uncordoned)
	}
	if len(result.Held) != 1 {
		t.Fatalf("expected the fresh cordon to be held, got %+v", result)
	}
	updated, _ := clientset.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if !updated.Spec.Unschedulable {
		t.Error("a cordon within its TTL must stay in place")
	}
}

func TestReapStaleCordons_IgnoresCordonsWeDidNotPlace(t *testing.T) {
	// An operator's own cordon: unschedulable, but no ownership annotations.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "operator-cordoned"},
		Spec:       corev1.NodeSpec{Unschedulable: true},
	}
	clientset := fake.NewSimpleClientset(node)

	result := ReapStaleCordons(context.Background(), clientset, true, "this-run", DefaultCordonTTL, reapNow)

	if result.Reaped() {
		t.Fatalf("reaper must never touch a cordon it did not place; got %+v", result)
	}
	updated, _ := clientset.CoreV1().Nodes().Get(context.Background(), "operator-cordoned", metav1.GetOptions{})
	if !updated.Spec.Unschedulable {
		t.Error("an operator's deliberate cordon must survive the reaper")
	}
}

func TestReapStaleCordons_RestoresPriorUnschedulableState(t *testing.T) {
	// The node was already cordoned before we claimed it, so reversing our
	// claim must not make it schedulable.
	node := cordonedNode("node-1", 2*time.Hour, "dead-run")
	node.Annotations[AnnotationPriorUnschedulable] = "true"
	clientset := fake.NewSimpleClientset(node)

	ReapStaleCordons(context.Background(), clientset, true, "this-run", DefaultCordonTTL, reapNow)

	updated, _ := clientset.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if !updated.Spec.Unschedulable {
		t.Error("reaping must not uncordon a node that was already cordoned before we claimed it")
	}
	if _, present := updated.Annotations[AnnotationCordonedAt]; present {
		t.Error("our markers should still be cleared even when schedulability is left alone")
	}
}

func TestReapStaleCordons_ClearsMarkersOnManuallyUncordonedNode(t *testing.T) {
	node := cordonedNode("node-1", 2*time.Hour, "dead-run")
	node.Spec.Unschedulable = false // operator already uncordoned it
	clientset := fake.NewSimpleClientset(node)

	result := ReapStaleCordons(context.Background(), clientset, true, "this-run", DefaultCordonTTL, reapNow)

	if len(result.Cleared) != 1 {
		t.Fatalf("expected the orphan markers to be cleared, got %+v", result)
	}
	if len(result.Uncordoned) != 0 {
		t.Error("nothing was cordoned, so nothing should be reported as uncordoned")
	}
	updated, _ := clientset.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if _, present := updated.Annotations[AnnotationCordonedAt]; present {
		t.Error("stale markers should be removed from an already-uncordoned node")
	}
}

func TestReapStaleCordons_DryRunMutatesNothing(t *testing.T) {
	node := cordonedNode("node-1", 2*time.Hour, "dead-run")
	clientset := fake.NewSimpleClientset(node)

	result := ReapStaleCordons(context.Background(), clientset, false, "this-run", DefaultCordonTTL, reapNow)

	if len(result.Uncordoned) != 1 {
		t.Fatalf("dry-run should still report what it would reverse, got %+v", result)
	}
	updated, _ := clientset.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if !updated.Spec.Unschedulable {
		t.Error("dry-run reap must not actually uncordon")
	}
	for _, action := range clientset.Actions() {
		if action.GetVerb() == "update" {
			t.Fatal("dry-run reap must not issue any update")
		}
	}
}

func TestReapStaleCordons_SkipsOwnRun(t *testing.T) {
	// Defensive: a marker carrying the current run's ID was placed by us this
	// pass and must never be reaped by that same pass.
	node := cordonedNode("node-1", 2*time.Hour, "this-run")
	clientset := fake.NewSimpleClientset(node)

	result := ReapStaleCordons(context.Background(), clientset, true, "this-run", DefaultCordonTTL, reapNow)

	if result.Reaped() {
		t.Fatalf("a cordon from the current run must not be reaped; got %+v", result)
	}
}

func TestReapStaleCordons_DisabledWhenTTLZero(t *testing.T) {
	node := cordonedNode("node-1", 30*24*time.Hour, "dead-run")
	clientset := fake.NewSimpleClientset(node)

	result := ReapStaleCordons(context.Background(), clientset, true, "this-run", 0, reapNow)

	if result.Reaped() {
		t.Fatalf("TTL=0 disables reaping entirely; got %+v", result)
	}
}

func TestReapStaleCordons_UnparseableTimestampIsTreatedAsStale(t *testing.T) {
	node := cordonedNode("node-1", time.Minute, "dead-run")
	node.Annotations[AnnotationCordonedAt] = "not-a-timestamp"
	clientset := fake.NewSimpleClientset(node)

	result := ReapStaleCordons(context.Background(), clientset, true, "this-run", DefaultCordonTTL, reapNow)

	if len(result.Uncordoned) != 1 {
		t.Fatalf("an unreadable marker must not strand a node forever; got %+v", result)
	}
}

func TestNudgePods_LiveCordonRecordsOwnership(t *testing.T) {
	node1 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi"),
		}},
	}
	node2 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi"),
		}},
	}
	isController := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-pod", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web-rs", Controller: &isController}},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi"),
			}}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	clientset := fake.NewSimpleClientset(node1, node2, pod)

	opts := liveOpts()
	opts.RunID = "run-abc"
	opts.now = func() time.Time { return reapNow }
	if _, err := NudgePodsWithResult(context.Background(), clientset, opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated, _ := clientset.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if !updated.Spec.Unschedulable {
		t.Fatal("expected node-1 to be cordoned")
	}
	if updated.Annotations[AnnotationCordonedByRun] != "run-abc" {
		t.Errorf("expected the cordon to record its run ID, got %q", updated.Annotations[AnnotationCordonedByRun])
	}
	if got := updated.Annotations[AnnotationCordonedAt]; got != reapNow.Format(time.RFC3339) {
		t.Errorf("expected cordon timestamp %q, got %q", reapNow.Format(time.RFC3339), got)
	}
	if updated.Annotations[AnnotationPriorUnschedulable] != "false" {
		t.Errorf("expected prior schedulability to be recorded as false, got %q", updated.Annotations[AnnotationPriorUnschedulable])
	}
}

func TestNudgePods_SkipsNodeInRecordonCooldown(t *testing.T) {
	// node-1 was reaped moments ago. Re-cordoning it now would re-evict the
	// same pods and restart the loop the reaper exists to break.
	node1 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "node-1",
			Annotations: map[string]string{AnnotationCordonReapedAt: reapNow.Add(-10 * time.Minute).Format(time.RFC3339)},
		},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi"),
		}},
	}
	node2 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi"),
		}},
	}
	isController := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-pod", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web-rs", Controller: &isController}},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi"),
			}}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	clientset := fake.NewSimpleClientset(node1, node2, pod)

	opts := liveOpts()
	opts.now = func() time.Time { return reapNow }
	result, err := NudgePodsWithResult(context.Background(), clientset, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.TargetNode == "node-1" {
		t.Fatal("node-1 is in re-cordon cooldown and must not be re-targeted")
	}
	updated, _ := clientset.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if updated.Spec.Unschedulable {
		t.Error("a node in cooldown must not be cordoned again")
	}
}

func TestNudgePods_HaltSwitchAlsoBlocksReaping(t *testing.T) {
	node := cordonedNode("node-1", 2*time.Hour, "dead-run")
	halt := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-optimizer-halt", Namespace: "cluster-optimizer"},
		Data:       map[string]string{"halt": "true"},
	}
	clientset := fake.NewSimpleClientset(node, halt)

	opts := liveOpts()
	opts.now = func() time.Time { return reapNow }
	result, err := NudgePodsWithResult(context.Background(), clientset, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Halted {
		t.Fatal("expected the run to report halted")
	}
	updated, _ := clientset.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if !updated.Spec.Unschedulable {
		t.Error("halt means this tool touches nothing, including the reaper")
	}
}

func TestNudgePods_ReapedNodeCountsTowardCapacity(t *testing.T) {
	// node-2 is stale-cordoned. Without reaping there is only one schedulable
	// node and consolidation is impossible; after reaping, node-1's pod has
	// somewhere to go.
	node1 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi"),
		}},
	}
	node2 := cordonedNode("node-2", 2*time.Hour, "dead-run")
	isController := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-pod", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web-rs", Controller: &isController}},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi"),
			}}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	clientset := fake.NewSimpleClientset(node1, node2, pod)

	opts := liveOpts()
	opts.now = func() time.Time { return reapNow }
	result, err := NudgePodsWithResult(context.Background(), clientset, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Reap.Uncordoned) != 1 {
		t.Fatalf("expected node-2's stale cordon to be reaped, got %+v", result.Reap)
	}
	if result.TargetNode != "node-1" {
		t.Fatalf("expected node-1 to become a viable target once node-2 was returned to service, got %q", result.TargetNode)
	}
}
