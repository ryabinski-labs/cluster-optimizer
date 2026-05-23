package analyzer

import (
	"strings"
	"testing"
	"time"

	"github.com/GipsyChef/cluster-optimizer/internal/classifier"
	"github.com/GipsyChef/cluster-optimizer/internal/model"
)

func TestDetectsSingleReplicaPDBBlocker(t *testing.T) {
	snapshot := model.Snapshot{
		ClusterID:  "test",
		CapturedAt: time.Now(),
		Nodes: []model.Node{
			{Name: "n1", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
			{Name: "n2", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
		},
		Workloads: []model.Workload{{
			Namespace: "default", Name: "api", Kind: "Deployment", Replicas: 1,
			Labels: map[string]string{"app": "api"}, Selector: map[string]string{"app": "api"},
			RequestsCPUm: 100, RequestsMemoryMiB: 128,
		}},
		PDBs: []model.PDB{{Namespace: "default", Name: "api-pdb", Selector: map[string]string{"app": "api"}, MinAvailable: "1"}},
	}

	report := Analyze(snapshot)
	if len(report.Findings) != 1 || report.Findings[0].RuleID != "single-replica-pdb-blocks-drain" {
		t.Fatalf("unexpected findings: %#v", report.Findings)
	}
}

func TestDetectsMemoryRequestBelowUsage(t *testing.T) {
	mem := int64(320)
	cpu := int64(10)
	snapshot := model.Snapshot{
		ClusterID:  "test",
		CapturedAt: time.Now(),
		Nodes: []model.Node{
			{Name: "n1", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
			{Name: "n2", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
		},
		Pods: []model.Pod{{
			Namespace: "default", Name: "api-1", Phase: "Running", OwnerKind: "ReplicaSet", OwnerName: "api-abc123",
			RequestsCPUm: 100, RequestsMemoryMiB: 128, UsageCPUm: &cpu, UsageMemoryMiB: &mem,
		}},
		Workloads: []model.Workload{{
			Namespace: "default", Name: "api", Kind: "Deployment", Replicas: 1,
			Labels: map[string]string{"app": "api"}, Selector: map[string]string{"app": "api"},
			RequestsCPUm: 100, RequestsMemoryMiB: 128, UsageCPUm: &cpu, UsageMemoryMiB: &mem,
		}},
	}

	report := Analyze(snapshot)
	for _, finding := range report.Findings {
		if finding.RuleID == "memory-request-below-usage" {
			return
		}
	}
	t.Fatalf("memory finding missing: %#v", report.Findings)
}

func TestSkipsCPURecommendationWhenCPUMetricsMissing(t *testing.T) {
	mem := int64(128)
	snapshot := model.Snapshot{
		ClusterID:  "test",
		CapturedAt: time.Now(),
		Nodes: []model.Node{
			{Name: "n1", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
			{Name: "n2", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
		},
		Workloads: []model.Workload{{
			Namespace: "default", Name: "api", Kind: "Deployment", Replicas: 1,
			Labels: map[string]string{"app": "api"}, Selector: map[string]string{"app": "api"},
			RequestsCPUm: 500, RequestsMemoryMiB: 128, UsageMemoryMiB: &mem,
		}},
	}

	report := Analyze(snapshot)
	for _, finding := range report.Findings {
		if finding.RuleID == "cpu-request-over-provisioned" {
			t.Fatalf("unexpected cpu finding: %#v", finding)
		}
	}
}

func TestDetectsLowRequestCPUHPASensitivity(t *testing.T) {
	target := int32(70)
	snapshot := model.Snapshot{
		ClusterID:  "test",
		CapturedAt: time.Now(),
		Nodes: []model.Node{
			{Name: "n1", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
			{Name: "n2", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
		},
		Workloads: []model.Workload{{
			Namespace: "default", Name: "api", Kind: "Deployment", Replicas: 1,
			Labels: map[string]string{"app": "api"}, Selector: map[string]string{"app": "api"},
			RequestsCPUm: 50, RequestsMemoryMiB: 256,
		}},
		HPAs: []model.HPA{{
			Namespace: "default", Name: "api-hpa", TargetKind: "Deployment", TargetName: "api",
			MinReplicas: 1, MaxReplicas: 3, Metrics: []string{"cpu"}, CPUUtilizationTarget: &target,
		}},
	}

	report := Analyze(snapshot)
	for _, finding := range report.Findings {
		if finding.RuleID == "cpu-hpa-low-request-sensitive" {
			return
		}
	}
	t.Fatalf("hpa sensitivity finding missing: %#v", report.Findings)
}

func TestSkipsLowRequestCPUHPASensitivityForAverageValueTarget(t *testing.T) {
	averageValue := int64(100)
	snapshot := model.Snapshot{
		ClusterID:  "test",
		CapturedAt: time.Now(),
		Nodes: []model.Node{
			{Name: "n1", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
			{Name: "n2", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
		},
		Workloads: []model.Workload{{
			Namespace: "default", Name: "api", Kind: "Deployment", Replicas: 1,
			Labels: map[string]string{"app": "api"}, Selector: map[string]string{"app": "api"},
			RequestsCPUm: 50, RequestsMemoryMiB: 256,
		}},
		HPAs: []model.HPA{{
			Namespace: "default", Name: "api-hpa", TargetKind: "Deployment", TargetName: "api",
			MinReplicas: 1, MaxReplicas: 3, Metrics: []string{"cpu"}, CPUAverageValueTargetm: &averageValue,
		}},
	}

	report := Analyze(snapshot)
	for _, finding := range report.Findings {
		if finding.RuleID == "cpu-hpa-low-request-sensitive" {
			t.Fatalf("unexpected hpa sensitivity finding: %#v", finding)
		}
	}
}

func TestCPURequestRecommendationMentionsHPARetuning(t *testing.T) {
	cpu := int64(20)
	target := int32(70)
	snapshot := model.Snapshot{
		ClusterID:  "test",
		CapturedAt: time.Now(),
		Nodes: []model.Node{
			{Name: "n1", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
			{Name: "n2", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
		},
		Workloads: []model.Workload{{
			Namespace: "default", Name: "api", Kind: "Deployment", Replicas: 1,
			Labels: map[string]string{"app": "api"}, Selector: map[string]string{"app": "api"},
			RequestsCPUm: 250, RequestsMemoryMiB: 256, UsageCPUm: &cpu,
		}},
		HPAs: []model.HPA{{
			Namespace: "default", Name: "api-hpa", TargetKind: "Deployment", TargetName: "api",
			MinReplicas: 1, MaxReplicas: 3, Metrics: []string{"cpu"}, CPUUtilizationTarget: &target,
		}},
	}

	report := Analyze(snapshot)
	for _, finding := range report.Findings {
		if finding.RuleID == "cpu-request-over-provisioned" && strings.Contains(finding.Recommendation, "HPA retuning") {
			return
		}
	}
	t.Fatalf("hpa-aware cpu recommendation missing: %#v", report.Findings)
}

func TestDetectsRuntimeModernizationCandidate(t *testing.T) {
	mem := int64(460)
	snapshot := model.Snapshot{
		ClusterID:  "test",
		CapturedAt: time.Now(),
		Nodes: []model.Node{
			{Name: "n1", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
			{Name: "n2", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
		},
		Workloads: []model.Workload{{
			Namespace: "default", Name: "api", Kind: "Deployment", Replicas: 1,
			Labels: map[string]string{"app": "api"}, Selector: map[string]string{"app": "api"},
			RuntimeHints: []string{"nodejs"}, RequestsCPUm: 100, RequestsMemoryMiB: 512, UsageMemoryMiB: &mem,
		}},
	}

	report := Analyze(snapshot)
	for _, finding := range report.Findings {
		if finding.RuleID == "runtime-modernization-candidate" {
			return
		}
	}
	t.Fatalf("runtime modernization finding missing: %#v", report.Findings)
}

func TestDetectsUnknownRuntimeModernizationCandidate(t *testing.T) {
	mem := int64(333)
	snapshot := model.Snapshot{
		ClusterID:  "test",
		CapturedAt: time.Now(),
		Nodes: []model.Node{
			{Name: "n1", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
			{Name: "n2", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
		},
		Workloads: []model.Workload{{
			Namespace: "default", Name: "api", Kind: "Deployment", Replicas: 1,
			Labels: map[string]string{"app": "api"}, Selector: map[string]string{"app": "api"},
			RequestsCPUm: 100, RequestsMemoryMiB: 128, UsageMemoryMiB: &mem,
		}},
	}

	report := Analyze(snapshot)
	for _, finding := range report.Findings {
		if finding.RuleID == "runtime-modernization-candidate" {
			return
		}
	}
	t.Fatalf("unknown runtime modernization finding missing: %#v", report.Findings)
}

func TestAnalyzeWithTagsProviderManagedAndRemediable(t *testing.T) {
	mem := int64(50)
	snapshot := model.Snapshot{
		ClusterID:  "default",
		CapturedAt: time.Now(),
		Nodes: []model.Node{
			{Name: "n1", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
			{Name: "n2", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
		},
		Workloads: []model.Workload{
			{
				Namespace: "default", Name: "agentdraft-api", Kind: "Deployment", Replicas: 1,
				Labels: map[string]string{"app": "agentdraft-api"}, Selector: map[string]string{"app": "agentdraft-api"},
				RequestsCPUm: 100, RequestsMemoryMiB: 512, UsageMemoryMiB: &mem,
			},
			{
				Namespace: "kube-system", Name: "kube-proxy", Kind: "DaemonSet", Replicas: 3,
				Labels:       map[string]string{"k8s-app": "kube-proxy"},
				Selector:     map[string]string{"k8s-app": "kube-proxy"},
				RequestsCPUm: 100, RequestsMemoryMiB: 375, UsageMemoryMiB: &mem,
			},
		},
	}
	c := classifier.New("default", []classifier.Target{{
		ClusterID:      "default",
		Namespace:      "default",
		Workload:       "Deployment/agentdraft-api",
		Container:      "agentdraft-api",
		SupportedRules: []string{"memory-request-over-provisioned"},
	}})
	report := AnalyzeWith(snapshot, c)
	var sawUserRemediable, sawProviderManaged bool
	for _, f := range report.Findings {
		if f.RuleID == "memory-request-over-provisioned" && f.Workload == "Deployment/agentdraft-api" {
			if f.ProviderManaged {
				t.Fatalf("agentdraft-api must not be provider managed")
			}
			if !f.Remediable {
				t.Fatalf("agentdraft-api memory finding should be remediable, got %#v", f)
			}
			sawUserRemediable = true
		}
		if f.Workload == "DaemonSet/kube-proxy" {
			if !f.ProviderManaged {
				t.Fatalf("kube-proxy finding must be provider managed")
			}
			if f.Remediable {
				t.Fatalf("kube-proxy finding must not be remediable, got %#v", f)
			}
			sawProviderManaged = true
		}
	}
	if !sawUserRemediable {
		t.Fatalf("expected a remediable agentdraft-api finding; got %#v", report.Findings)
	}
	if !sawProviderManaged {
		t.Fatalf("expected a provider-managed kube-proxy finding; got %#v", report.Findings)
	}
}

func TestDetectsFixedReplicaWithoutAutoscaler(t *testing.T) {
	snapshot := model.Snapshot{
		ClusterID:  "test",
		CapturedAt: time.Now(),
		Nodes: []model.Node{
			{Name: "n1", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
			{Name: "n2", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
		},
		Workloads: []model.Workload{{
			Namespace: "default", Name: "api", Kind: "Deployment", Replicas: 2,
			Labels: map[string]string{"app": "api"}, Selector: map[string]string{"app": "api"},
			RequestsCPUm: 100, RequestsMemoryMiB: 128,
		}},
	}

	report := Analyze(snapshot)
	var sawScaling, sawPDB bool
	for _, finding := range report.Findings {
		if finding.RuleID == "fixed-replica-capacity-without-autoscaler" {
			sawScaling = true
		}
		if finding.RuleID == "missing-pdb-for-multi-replica-workload" {
			sawPDB = true
		}
	}
	if !sawScaling || !sawPDB {
		t.Fatalf("expected scaling and pdb findings, got: %#v", report.Findings)
	}
}
