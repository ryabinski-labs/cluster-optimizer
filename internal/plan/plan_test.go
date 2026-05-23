package plan

import (
	"testing"
	"time"

	"github.com/GipsyChef/cluster-optimizer/internal/analyzer"
	"github.com/GipsyChef/cluster-optimizer/internal/classifier"
	"github.com/GipsyChef/cluster-optimizer/internal/model"
)

func buildSnapshot(usageMem int64) model.Snapshot {
	mem := usageMem
	return model.Snapshot{
		ClusterID:  "default",
		CapturedAt: time.Now(),
		Nodes: []model.Node{
			{Name: "n1", AllocatableCPUm: 1900, AllocatableMemoryMiB: 3000},
		},
		Workloads: []model.Workload{{
			Namespace: "default", Name: "api", Kind: "Deployment", Replicas: 1,
			Labels: map[string]string{"app": "api"}, Selector: map[string]string{"app": "api"},
			RequestsCPUm: 200, RequestsMemoryMiB: 512, UsageMemoryMiB: &mem,
		}},
	}
}

func buildClassifier(rules ...string) *classifier.Classifier {
	return classifier.New("default", []classifier.Target{{
		ClusterID:      "default",
		Namespace:      "default",
		Workload:       "Deployment/api",
		Container:      "api",
		SupportedRules: rules,
	}})
}

func TestProposeRequestRespectsFloorWhenMaxTrimAllows(t *testing.T) {
	// usage 5, current 100, max trim 50%, floor 32 → target 7, hits floor 32,
	// but max trim cap is current-50 = 50; floor 32 < 50, so we land at 50.
	got := proposeRequest(5, 100, 3, 2, 32, 1, 2)
	if got != 50 {
		t.Fatalf("expected max-trim 50, got %d", got)
	}
	// usage 5, current 100, max trim 90% → target 7, hits floor 32; max trim
	// cap is current-90 = 10, target raised to 32 since floor wins.
	got = proposeRequest(5, 100, 3, 2, 32, 9, 10)
	if got != 32 {
		t.Fatalf("expected floor 32, got %d", got)
	}
}

func TestProposeRequestRespectsMaxTrim(t *testing.T) {
	// observed 1, current 1000 → target = 1*3/2 = 1, floor 10 → 10
	// max trim = 1000*1/2 = 500, min allowed = 500
	// so result should be 500 (capped at the maxTrim, not the floor)
	got := proposeRequest(1, 1000, 3, 2, 10, 1, 2)
	if got != 500 {
		t.Fatalf("expected max-trim cap 500, got %d", got)
	}
}

func TestProposeRequestReturnsZeroWhenNoTrim(t *testing.T) {
	got := proposeRequest(400, 500, 3, 2, 32, 1, 2)
	// target = 400*3/2 = 600 > 500 current → no trim
	if got != 0 {
		t.Fatalf("expected 0 when no trim possible, got %d", got)
	}
}

func TestBuildProducesActionWhenAllGatesPass(t *testing.T) {
	snapshot := buildSnapshot(50)
	c := buildClassifier("memory-request-over-provisioned")
	findings := []analyzer.Finding{{
		RuleID: "memory-request-over-provisioned", Severity: "medium",
		Namespace: "default", Workload: "Deployment/api",
		Confidence: "high", Remediable: true,
	}}
	report := analyzer.Report{ClusterID: "default", Findings: findings}
	occurrences := map[string]int64{occurrenceKey(findings[0]): 5}
	plan := Build(report, snapshot, c, DefaultPolicy(), occurrences)
	if len(plan.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d: %#v (skipped=%#v)", len(plan.Actions), plan.Actions, plan.Skipped)
	}
	action := plan.Actions[0]
	if action.NewMemMiB <= 0 || action.NewMemMiB >= 512 {
		t.Fatalf("expected trimmed memory < 512, got %d", action.NewMemMiB)
	}
	if action.NewCPUm != -1 {
		t.Fatalf("memory rule must not touch CPU, got %d", action.NewCPUm)
	}
	if action.OccurrenceCount != 5 {
		t.Fatalf("expected occurrence count 5, got %d", action.OccurrenceCount)
	}
}

func TestBuildSkipsProviderManaged(t *testing.T) {
	snapshot := model.Snapshot{
		ClusterID: "default",
		Workloads: []model.Workload{{
			Namespace: "kube-system", Name: "kube-proxy", Kind: "DaemonSet", Replicas: 3,
			RequestsMemoryMiB: 375,
		}},
	}
	c := classifier.New("default", nil)
	findings := []analyzer.Finding{{
		RuleID: "memory-request-over-provisioned", Severity: "medium",
		Namespace: "kube-system", Workload: "DaemonSet/kube-proxy",
		Confidence: "high",
	}}
	plan := Build(analyzer.Report{Findings: findings}, snapshot, c, DefaultPolicy(), map[string]int64{})
	if len(plan.Actions) != 0 {
		t.Fatalf("must not plan against provider-managed workload, got %#v", plan.Actions)
	}
	if len(plan.Skipped) != 1 || plan.Skipped[0].Reason != "workload is provider-managed" {
		t.Fatalf("expected provider-managed skip, got %#v", plan.Skipped)
	}
}

func TestBuildSkipsBelowMinConfidence(t *testing.T) {
	snapshot := buildSnapshot(50)
	c := buildClassifier("memory-request-over-provisioned")
	findings := []analyzer.Finding{{
		RuleID: "memory-request-over-provisioned",
		Namespace: "default", Workload: "Deployment/api",
		Confidence: "low",
	}}
	policy := DefaultPolicy()
	plan := Build(analyzer.Report{Findings: findings}, snapshot, c, policy, map[string]int64{occurrenceKey(findings[0]): 10})
	if len(plan.Actions) != 0 {
		t.Fatalf("low confidence must be skipped under default policy, got %#v", plan.Actions)
	}
}

func TestBuildSkipsBelowMinOccurrences(t *testing.T) {
	snapshot := buildSnapshot(50)
	c := buildClassifier("memory-request-over-provisioned")
	findings := []analyzer.Finding{{
		RuleID: "memory-request-over-provisioned",
		Namespace: "default", Workload: "Deployment/api",
		Confidence: "high",
	}}
	// Only 1 occurrence, default policy requires 3.
	plan := Build(analyzer.Report{Findings: findings}, snapshot, c, DefaultPolicy(),
		map[string]int64{occurrenceKey(findings[0]): 1})
	if len(plan.Actions) != 0 {
		t.Fatalf("min occurrences must gate live action, got %#v", plan.Actions)
	}
}

func TestBuildRefusesWithoutPersistenceWhenRequired(t *testing.T) {
	snapshot := buildSnapshot(50)
	c := buildClassifier("memory-request-over-provisioned")
	findings := []analyzer.Finding{{
		RuleID: "memory-request-over-provisioned",
		Namespace: "default", Workload: "Deployment/api",
		Confidence: "high",
	}}
	plan := Build(analyzer.Report{Findings: findings}, snapshot, c, DefaultPolicy(), nil)
	if len(plan.Actions) != 0 {
		t.Fatalf("must refuse to plan when persistence required but missing, got %#v", plan.Actions)
	}
}

func TestBuildRespectsMaxActions(t *testing.T) {
	snapshot := buildSnapshot(50)
	// Two workloads, both eligible.
	snapshot.Workloads = append(snapshot.Workloads, model.Workload{
		Namespace: "default", Name: "api2", Kind: "Deployment", Replicas: 1,
		RequestsMemoryMiB: 512, UsageMemoryMiB: ptr(int64(40)),
	})
	c := classifier.New("default", []classifier.Target{
		{ClusterID: "default", Namespace: "default", Workload: "Deployment/api", Container: "api",
			SupportedRules: []string{"memory-request-over-provisioned"}},
		{ClusterID: "default", Namespace: "default", Workload: "Deployment/api2", Container: "api2",
			SupportedRules: []string{"memory-request-over-provisioned"}},
	})
	findings := []analyzer.Finding{
		{RuleID: "memory-request-over-provisioned", Namespace: "default", Workload: "Deployment/api", Confidence: "high"},
		{RuleID: "memory-request-over-provisioned", Namespace: "default", Workload: "Deployment/api2", Confidence: "high"},
	}
	occ := map[string]int64{occurrenceKey(findings[0]): 10, occurrenceKey(findings[1]): 10}
	policy := DefaultPolicy()
	policy.MaxActions = 1
	plan := Build(analyzer.Report{Findings: findings}, snapshot, c, policy, occ)
	if len(plan.Actions) != 1 {
		t.Fatalf("expected exactly 1 action due to MaxActions cap, got %d", len(plan.Actions))
	}
}

func TestBuildSkipsWhenNoTrimAvailable(t *testing.T) {
	// usage close to current; headroom would push above current.
	snapshot := buildSnapshot(400)
	c := buildClassifier("memory-request-over-provisioned")
	findings := []analyzer.Finding{{
		RuleID: "memory-request-over-provisioned",
		Namespace: "default", Workload: "Deployment/api",
		Confidence: "high",
	}}
	plan := Build(analyzer.Report{Findings: findings}, snapshot, c, DefaultPolicy(),
		map[string]int64{occurrenceKey(findings[0]): 5})
	if len(plan.Actions) != 0 {
		t.Fatalf("no trim should be available, got %#v", plan.Actions)
	}
	if len(plan.Skipped) != 1 || plan.Skipped[0].Reason != "no safe trim available" {
		t.Fatalf("expected no-safe-trim skip, got %#v", plan.Skipped)
	}
}

func ptr(v int64) *int64 { return &v }
