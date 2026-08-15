package collector

import (
	"testing"

	"github.com/GipsyChef/cluster-optimizer/internal/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodePool_PrefersProviderLabels(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"doks", map[string]string{"doks.digitalocean.com/node-pool": "pool-general"}, "pool-general"},
		{"eks", map[string]string{"eks.amazonaws.com/nodegroup": "ng-1"}, "ng-1"},
		{"gke", map[string]string{"cloud.google.com/gke-nodepool": "default-pool"}, "default-pool"},
		{"aks", map[string]string{"agentpool": "nodepool1"}, "nodepool1"},
		{"karpenter", map[string]string{"karpenter.sh/nodepool": "spot"}, "spot"},
		{
			"provider label wins over instance type",
			map[string]string{"doks.digitalocean.com/node-pool": "pool-a", "node.kubernetes.io/instance-type": "s-4vcpu-8gb"},
			"pool-a",
		},
		{
			"falls back to instance type",
			map[string]string{"node.kubernetes.io/instance-type": "s-4vcpu-8gb"},
			"instance-type/s-4vcpu-8gb",
		},
		{"unlabelled node still belongs to a pool", nil, "default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", Labels: tc.labels}}
			if got := nodePool(node); got != tc.want {
				t.Errorf("nodePool() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNodeReady_MissingConditionIsNotReady(t *testing.T) {
	// A node with no Ready condition must not be counted as capacity.
	if nodeReady(corev1.Node{}) {
		t.Error("a node with no Ready condition must not report ready")
	}
	notReady := corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
	}}}
	if nodeReady(notReady) {
		t.Error("Ready=False must not report ready")
	}
	ready := corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
	}}}
	if !nodeReady(ready) {
		t.Error("Ready=True must report ready")
	}
}

func TestExtendedAllocatable_SkipsCPUAndMemory(t *testing.T) {
	list := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
		corev1.ResourcePods:   resource.MustParse("110"),
		"nvidia.com/gpu":      resource.MustParse("2"),
	}
	got := extendedAllocatable(list)
	if got["nvidia.com/gpu"] != 2 {
		t.Errorf("expected the GPU count to be captured, got %v", got)
	}
	for _, skipped := range []string{"cpu", "memory", "pods"} {
		if _, present := got[skipped]; present {
			t.Errorf("%s is tracked natively and must not appear in extended allocatable", skipped)
		}
	}
}

func TestPodAffinity_DropsPreferredKeepsRequired(t *testing.T) {
	spec := corev1.PodSpec{Affinity: &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				TopologyKey:   "kubernetes.io/hostname",
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "payments"}},
			}},
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
				Weight:          100,
				PodAffinityTerm: corev1.PodAffinityTerm{TopologyKey: "topology.kubernetes.io/zone"},
			}},
		},
	}}
	got := podAffinity(spec)
	if got == nil {
		t.Fatal("expected required anti-affinity to be captured")
	}
	if len(got.RequiredPodAntiAffinity) != 1 {
		t.Fatalf("expected exactly the required term, got %d", len(got.RequiredPodAntiAffinity))
	}
	if got.RequiredPodAntiAffinity[0].TopologyKey != "kubernetes.io/hostname" {
		t.Errorf("wrong term captured: %+v", got.RequiredPodAntiAffinity[0])
	}
	if got.RequiredPodAntiAffinity[0].LabelSelector.MatchLabels["app"] != "payments" {
		t.Error("expected the term's label selector to survive conversion")
	}
}

func TestPodAffinity_NilWhenOnlyPreferredTermsExist(t *testing.T) {
	// Soft terms cannot make a placement impossible, so a pod carrying only
	// preferred rules must not look constrained to the simulator.
	spec := corev1.PodSpec{Affinity: &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{Weight: 1}},
		},
	}}
	if got := podAffinity(spec); got != nil {
		t.Errorf("expected nil affinity when only preferred terms exist, got %+v", got)
	}
}

func TestUnmodelledConstraints_FlagsWhatTheSimulatorCannotSee(t *testing.T) {
	cases := []struct {
		name string
		pod  corev1.Pod
		want string
	}{
		{
			name: "gpu request",
			pod: corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}},
			}}}},
			want: model.UnmodelledExtendedResource,
		},
		{
			name: "persistent volume claim",
			pod: corev1.Pod{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
				Name:         "data",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}},
			}}}},
			want: model.UnmodelledVolumeAffinity,
		},
		{
			name: "runtime class overhead",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Overhead: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			}},
			want: model.UnmodelledRuntimeOverhead,
		},
		{
			name: "spread matchLabelKeys",
			pod: corev1.Pod{Spec: corev1.PodSpec{TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
				MaxSkew: 1, TopologyKey: "kubernetes.io/hostname", MatchLabelKeys: []string{"pod-template-hash"},
			}}}},
			want: model.UnmodelledMatchLabelKeys,
		},
		{
			name: "affinity namespaceSelector",
			pod: corev1.Pod{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{
				PodAntiAffinity: &corev1.PodAntiAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
						TopologyKey:       "kubernetes.io/hostname",
						NamespaceSelector: &metav1.LabelSelector{},
					}},
				},
			}}},
			want: model.UnmodelledNamespaceSelector,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unmodelledConstraints(tc.pod, extendedRequests(tc.pod.Spec))
			if !contains(got, tc.want) {
				t.Errorf("expected %q in unmodelled constraints, got %v", tc.want, got)
			}
		})
	}
}

func TestUnmodelledConstraints_OrdinaryPodIsFullyModelled(t *testing.T) {
	// The common case must stay clean, or every pool goes indeterminate and
	// the engine never recommends anything.
	pod := corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi"),
		}}}},
		NodeSelector: map[string]string{"pool": "general"},
		Volumes:      []corev1.Volume{{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
	}}
	if got := unmodelledConstraints(pod, extendedRequests(pod.Spec)); len(got) != 0 {
		t.Errorf("an ordinary pod must be fully modelled, got %v", got)
	}
}

func TestUnmodelledConstraints_EphemeralStorageIsNotFlagged(t *testing.T) {
	// Nearly every pod carries an ephemeral-storage request; flagging it
	// would make every pool indeterminate and the engine useless.
	pod := corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceEphemeralStorage: resource.MustParse("1Gi"),
		}},
	}}}}
	if got := unmodelledConstraints(pod, extendedRequests(pod.Spec)); len(got) != 0 {
		t.Errorf("ephemeral storage must not force indeterminate, got %v", got)
	}
}

func TestPodModel_MirrorPodIsNotRelocatable(t *testing.T) {
	isController := true
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kube-apiserver-node1", Namespace: "kube-system",
			Annotations:     map[string]string{"kubernetes.io/config.mirror": "abc123"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "Node", Name: "node1", Controller: &isController}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	got := podModel(pod, resourceUsage{})
	if got.Relocatable() {
		t.Error("a mirror pod is the kubelet's and cannot be relocated")
	}
}

func TestPodRelocatableAndActive(t *testing.T) {
	if (model.Pod{OwnerKind: "ReplicaSet"}).Relocatable() != true {
		t.Error("a ReplicaSet-owned pod is relocatable")
	}
	if (model.Pod{OwnerKind: "DaemonSet"}).Relocatable() {
		t.Error("a DaemonSet pod follows its node and is not relocatable")
	}
	if (model.Pod{OwnerKind: ""}).Relocatable() {
		t.Error("a bare pod has no controller to recreate it")
	}
	if (model.Pod{Phase: "Succeeded"}).Active() {
		t.Error("a completed pod occupies no capacity")
	}
	if !(model.Pod{Phase: "Running"}).Active() {
		t.Error("a running pod occupies capacity")
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
