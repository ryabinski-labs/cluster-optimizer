package nudger

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// liveOpts returns an Options that flips the dry-run gate off so existing
// cordon+evict assertions keep working.
func liveOpts() Options {
	opts := NewOptions()
	opts.Live = true
	return opts
}

func TestNudgePodsWithResult_FeasibleReportsEvictedAndTarget(t *testing.T) {
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
	// Create fake nodes
	node1 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-1",
		},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
	}
	node2 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-2",
		},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
	}

	// Create a controller-owned pod on node-1 (least loaded)
	isController := true
	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-pod",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "ReplicaSet",
					Name:       "web-rs",
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
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

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
	node1 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-1",
		},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}
	node2 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-2",
		},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		},
	}

	// Pod 1 on node-1 has 800m CPU
	isController := true
	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-pod",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "ReplicaSet",
					Name:       "web-rs",
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
							corev1.ResourceCPU:    resource.MustParse("800m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	// Pod 2 on node-2 takes up almost all node-2 capacity
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-pod",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "ReplicaSet",
					Name:       "other-rs",
					Controller: &isController,
				},
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-2",
			Containers: []corev1.Container{
				{
					Name: "app",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("800m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

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
	node1 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-1",
		},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
	}
	node2 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-2",
		},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("4Gi"),
			},
		},
	}

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
	node1 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		}},
	}
	node2 := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
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
	// Default opts: dry-run (Live=false).
	if err := NudgePods(context.Background(), clientset, NewOptions()); err != nil {
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
			Name: "web-pod", Namespace: "default", Labels: map[string]string{"app": "web"},
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
