package analyzer

import (
	"testing"
	"time"

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
