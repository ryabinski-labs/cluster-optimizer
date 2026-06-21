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

// TestSkipsMemoryOverProvisionedWhenPerReplicaRequestReasonable is a regression
// test for the recurring "lower memory request to 128Mi" recommendation that
// never resolved. Requests and usage in the snapshot are fleet totals summed
// across replicas, so a workload whose per-pod request is already a reasonable
// 128Mi must not be flagged just because it runs several replicas
// (2x128=256Mi, 3x128=384Mi fleet totals). Applying the per-pod 128Mi target
// the UI derives from such a finding is a no-op, so the finding would otherwise
// recur on every analyzer run forever.
func TestSkipsMemoryOverProvisionedWhenPerReplicaRequestReasonable(t *testing.T) {
	cases := []struct {
		name     string
		replicas int32
		request  int64 // fleet total across all replicas
		usage    int64 // fleet total across all replicas
	}{
		{"echothread-api two replicas at 128Mi", 2, 256, 47},
		{"tovinio-backend three replicas at 128Mi", 3, 384, 66},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage := tc.usage
			snapshot := model.Snapshot{
				ClusterID:  "default",
				CapturedAt: time.Now(),
				Workloads: []model.Workload{{
					Namespace: "default", Name: "api", Kind: "Deployment", Replicas: tc.replicas,
					Labels: map[string]string{"app": "api"}, Selector: map[string]string{"app": "api"},
					RequestsMemoryMiB: tc.request, UsageMemoryMiB: &usage,
				}},
			}
			for _, finding := range Analyze(snapshot).Findings {
				if finding.RuleID == "memory-request-over-provisioned" {
					t.Fatalf("per-replica request is already reasonable; rule must not fire: %#v", finding)
				}
			}
		})
	}
}

// TestDetectsMemoryOverProvisionedUsesPerReplicaEvidence verifies a genuinely
// over-provisioned workload (512Mi per pod, ~50Mi used) still fires and that the
// evidence reports per-replica values, so the remediation target derived from
// the evidence is a per-container value rather than a fleet total.
func TestDetectsMemoryOverProvisionedUsesPerReplicaEvidence(t *testing.T) {
	usage := int64(150) // fleet total across 3 replicas => 50Mi per pod
	snapshot := model.Snapshot{
		ClusterID:  "default",
		CapturedAt: time.Now(),
		Workloads: []model.Workload{{
			Namespace: "default", Name: "api", Kind: "Deployment", Replicas: 3,
			Labels: map[string]string{"app": "api"}, Selector: map[string]string{"app": "api"},
			RequestsMemoryMiB: 1536, UsageMemoryMiB: &usage, // 512Mi per pod
		}},
	}
	var found bool
	for _, finding := range Analyze(snapshot).Findings {
		if finding.RuleID != "memory-request-over-provisioned" {
			continue
		}
		found = true
		if !strings.Contains(finding.Evidence, "request 512Mi") || !strings.Contains(finding.Evidence, "Observed memory 50Mi") {
			t.Fatalf("evidence should report per-replica values (50Mi used, 512Mi request), got: %q", finding.Evidence)
		}
		if strings.Contains(finding.Evidence, "1536Mi") {
			t.Fatalf("evidence must not report the fleet-total request (1536Mi), got: %q", finding.Evidence)
		}
	}
	if !found {
		t.Fatalf("expected a memory-request-over-provisioned finding for a per-pod over-provisioned workload")
	}
}

// TestSkipsCPUOverProvisionedWhenPerReplicaRequestReasonable guards the same
// fleet-total-vs-per-replica bug on the CPU path: 2 replicas x 100m request
// (200m fleet) with 15m used per pod leaves an 85m per-pod gap, below the 100m
// threshold, so the rule must stay quiet.
func TestSkipsCPUOverProvisionedWhenPerReplicaRequestReasonable(t *testing.T) {
	cpu := int64(30) // fleet total across 2 replicas => 15m per pod
	snapshot := model.Snapshot{
		ClusterID:  "default",
		CapturedAt: time.Now(),
		Workloads: []model.Workload{{
			Namespace: "default", Name: "api", Kind: "Deployment", Replicas: 2,
			Labels: map[string]string{"app": "api"}, Selector: map[string]string{"app": "api"},
			RequestsCPUm: 200, UsageCPUm: &cpu,
		}},
	}
	for _, finding := range Analyze(snapshot).Findings {
		if finding.RuleID == "cpu-request-over-provisioned" {
			t.Fatalf("per-replica CPU request is reasonable; rule must not fire: %#v", finding)
		}
	}
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
				// Per replica this is 512Mi requested vs ~16Mi used, so the
				// over-provisioned rule fires; the assertions below verify it is
				// tagged provider-managed and non-remediable.
				Namespace: "kube-system", Name: "kube-proxy", Kind: "DaemonSet", Replicas: 3,
				Labels:       map[string]string{"k8s-app": "kube-proxy"},
				Selector:     map[string]string{"k8s-app": "kube-proxy"},
				RequestsCPUm: 300, RequestsMemoryMiB: 1536, UsageMemoryMiB: &mem,
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

func diskFinding(findings []Finding) *Finding {
	for i := range findings {
		if findings[i].RuleID == "node-disk-utilization-high" {
			return &findings[i]
		}
	}
	return nil
}

func TestNodeDiskBelowThresholdIsQuiet(t *testing.T) {
	snapshot := model.Snapshot{Nodes: []model.Node{
		{Name: "n1", DiskCapacityBytes: 100, DiskUsedBytes: 50, ImageFsUsedBytes: 30},
	}}
	if f := diskFinding(Analyze(snapshot).Findings); f != nil {
		t.Fatalf("did not expect a disk finding at 50%%, got %#v", f)
	}
}

func TestNodeDiskWarnAtSeventyPercent(t *testing.T) {
	snapshot := model.Snapshot{Nodes: []model.Node{
		{Name: "n1", DiskCapacityBytes: 100, DiskUsedBytes: 71, ImageFsUsedBytes: 40},
	}}
	f := diskFinding(Analyze(snapshot).Findings)
	if f == nil {
		t.Fatal("expected a disk finding at 71%")
	}
	if f.Severity != "medium" {
		t.Errorf("severity = %q, want medium", f.Severity)
	}
	if f.Workload != "Node/n1" {
		t.Errorf("workload = %q, want Node/n1", f.Workload)
	}
}

func TestNodeDiskCriticalAtEightyFivePercent(t *testing.T) {
	snapshot := model.Snapshot{Nodes: []model.Node{
		{Name: "n1", DiskCapacityBytes: 100, DiskUsedBytes: 90, ImageFsUsedBytes: 60},
	}}
	f := diskFinding(Analyze(snapshot).Findings)
	if f == nil || f.Severity != "high" {
		t.Fatalf("expected high-severity disk finding at 90%%, got %#v", f)
	}
}

func TestNodeDiskPressureIsCriticalEvenBelowThreshold(t *testing.T) {
	// A node under DiskPressure must be flagged high even if the reported
	// percentage is momentarily below the warn line.
	snapshot := model.Snapshot{Nodes: []model.Node{
		{Name: "n1", DiskCapacityBytes: 100, DiskUsedBytes: 60, DiskPressure: true},
	}}
	f := diskFinding(Analyze(snapshot).Findings)
	if f == nil || f.Severity != "high" {
		t.Fatalf("expected high-severity finding under DiskPressure, got %#v", f)
	}
	if !strings.Contains(f.Evidence, "DiskPressure") {
		t.Errorf("evidence should mention DiskPressure: %q", f.Evidence)
	}
}

func TestNodeDiskNoStatsIsSkipped(t *testing.T) {
	// Capacity 0 means the kubelet stats proxy was unreachable; skip silently.
	snapshot := model.Snapshot{Nodes: []model.Node{{Name: "n1"}}}
	if f := diskFinding(Analyze(snapshot).Findings); f != nil {
		t.Fatalf("expected no finding without disk stats, got %#v", f)
	}
}
