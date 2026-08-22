package classifier

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsProviderManagedByNamespace(t *testing.T) {
	c := New("default", nil)
	if !c.IsProviderManaged("kube-system", "Deployment/whatever") {
		t.Fatal("kube-system should be provider managed")
	}
	if c.IsProviderManaged("default", "Deployment/agentdraft-api") {
		t.Fatal("user namespace deployment must not be provider managed")
	}
}

func TestIsProviderManagedByWorkloadName(t *testing.T) {
	c := New("default", nil)
	// DaemonSet deployed into default namespace by DOKS still counts as provider managed.
	if !c.IsProviderManaged("default", "DaemonSet/cilium") {
		t.Fatal("cilium DaemonSet must be provider managed regardless of namespace")
	}
	if !c.IsProviderManaged("default", "DaemonSet/kube-proxy") {
		t.Fatal("kube-proxy must be provider managed")
	}
}

func TestIsRemediableRequiresRuleMatch(t *testing.T) {
	c := New("default", []Target{{
		ClusterID:      "default",
		Namespace:      "default",
		Workload:       "Deployment/agentdraft-api",
		Container:      "agentdraft-api",
		SupportedRules: []string{"memory-request-over-provisioned"},
	}})
	if !c.IsRemediable("memory-request-over-provisioned", "default", "Deployment/agentdraft-api") {
		t.Fatal("expected memory rule to be remediable for agentdraft-api")
	}
	if c.IsRemediable("cpu-request-over-provisioned", "default", "Deployment/agentdraft-api") {
		t.Fatal("cpu rule must not be remediable when target does not list it")
	}
	if c.IsRemediable("memory-request-over-provisioned", "default", "Deployment/no-such") {
		t.Fatal("missing target must not be remediable")
	}
}

func TestNewIgnoresTargetsForOtherClusters(t *testing.T) {
	c := New("default", []Target{{
		ClusterID:      "other-cluster",
		Namespace:      "default",
		Workload:       "Deployment/agentdraft-api",
		SupportedRules: []string{"memory-request-over-provisioned"},
	}})
	if c.IsRemediable("memory-request-over-provisioned", "default", "Deployment/agentdraft-api") {
		t.Fatal("target for other cluster must not be applied to current cluster")
	}
}

func TestLoadTargetsMissingFile(t *testing.T) {
	got, err := LoadTargets(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty targets, got %d", len(got))
	}
}

func TestLoadTargetsReadsJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.json")
	body := `{"targets":[{"cluster_id":"default","namespace":"default","workload":"Deployment/api","container":"api","supported_rules":["memory-request-over-provisioned"]}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	got, err := LoadTargets(path)
	if err != nil {
		t.Fatalf("LoadTargets error: %v", err)
	}
	if len(got) != 1 || got[0].Workload != "Deployment/api" {
		t.Fatalf("unexpected targets: %#v", got)
	}
}

func TestTargetForReturnsCopy(t *testing.T) {
	c := New("default", []Target{{
		ClusterID: "default", Namespace: "default", Workload: "Deployment/api", Container: "api",
	}})
	target, ok := c.TargetFor("default", "Deployment/api")
	if !ok {
		t.Fatal("expected target to be found")
	}
	if target.Container != "api" {
		t.Fatalf("unexpected container: %q", target.Container)
	}
}

func TestTargetForUsesGlobalWildcard(t *testing.T) {
	c := NewWithNamespaceLabels("default", []Target{{
		ClusterID: "default", Namespace: "*", Workload: "*", Container: "*",
		SupportedRules: []string{"memory-request-over-provisioned"},
	}}, map[string]map[string]string{
		"sendant": {NamespaceRemediationLabel: NamespaceRemediationValue},
		"steada":  {NamespaceRemediationLabel: NamespaceRemediationValue},
	})
	target, ok := c.TargetFor("sendant", "Deployment/bootstrapd")
	if !ok || target.Container != "*" {
		t.Fatalf("expected global wildcard target, got %#v, %v", target, ok)
	}
	if !c.IsRemediable("memory-request-over-provisioned", "steada", "StatefulSet/cache") {
		t.Fatal("global wildcard should cover workloads in every application namespace")
	}
}

func TestTargetForPrefersExactTargetOverWildcard(t *testing.T) {
	c := New("default", []Target{
		{ClusterID: "default", Namespace: "*", Workload: "*", Container: "*"},
		{ClusterID: "default", Namespace: "sendant", Workload: "Deployment/bootstrapd", Container: "bootstrapd"},
	})
	target, ok := c.TargetFor("sendant", "Deployment/bootstrapd")
	if !ok || target.Container != "bootstrapd" {
		t.Fatalf("expected exact target, got %#v, %v", target, ok)
	}
}

func TestPlatformNamespacesRemainProviderManagedWithWildcard(t *testing.T) {
	c := New("default", []Target{{
		ClusterID: "default", Namespace: "*", Workload: "*", Container: "*",
		SupportedRules: []string{"memory-request-over-provisioned"},
	}})
	for _, namespace := range []string{"cert-manager", "ingress-nginx", "kube-system", "cluster-optimizer", "gatekeeper-system", "tigera-operator"} {
		if !c.IsProviderManaged(namespace, "Deployment/controller") {
			t.Errorf("namespace %q should remain provider managed", namespace)
		}
	}
}

func TestWildcardRulesActAsDefaultsForExactTargets(t *testing.T) {
	c := NewWithNamespaceLabels("default", []Target{
		{ClusterID: "default", Namespace: "*", Workload: "*", Container: "*",
			SupportedRules: []string{"memory-request-over-provisioned"}},
		{ClusterID: "default", Namespace: "sendant", Workload: "Deployment/bootstrapd", Container: "bootstrapd",
			SupportedRules: []string{"cpu-request-over-provisioned"}},
	}, map[string]map[string]string{"sendant": {NamespaceRemediationLabel: NamespaceRemediationValue}})
	if !c.IsRemediable("memory-request-over-provisioned", "sendant", "Deployment/bootstrapd") {
		t.Fatal("global wildcard rule should remain enabled when an exact target omits it")
	}
	if !c.IsRemediable("cpu-request-over-provisioned", "sendant", "Deployment/bootstrapd") {
		t.Fatal("exact target rule should remain enabled")
	}
	target, ok := c.TargetFor("sendant", "Deployment/bootstrapd")
	if !ok || target.Container != "bootstrapd" {
		t.Fatalf("exact target metadata should still win, got %#v, %v", target, ok)
	}
}

func TestWildcardRequiresNamespaceOptIn(t *testing.T) {
	targets := []Target{{
		ClusterID: "default", Namespace: "*", Workload: "*", Container: "*",
		SupportedRules: []string{"memory-request-over-provisioned"},
	}}
	c := NewWithNamespaceLabels("default", targets, map[string]map[string]string{
		"monitoring": {},
		"sendant":    {NamespaceRemediationLabel: NamespaceRemediationValue},
	})
	if _, ok := c.TargetFor("monitoring", "Deployment/prometheus"); ok {
		t.Fatal("unlabelled namespace must not match a namespace wildcard")
	}
	target, ok := c.TargetForRule("memory-request-over-provisioned", "sendant", "Deployment/bootstrapd")
	if !ok || !target.RequiresNamespaceOptIn {
		t.Fatalf("labelled namespace should resolve wildcard with recheck required, got %#v, %v", target, ok)
	}
}

func TestExactTargetDoesNotRequireNamespaceOptIn(t *testing.T) {
	c := NewWithNamespaceLabels("default", []Target{
		{ClusterID: "default", Namespace: "*", Workload: "*", Container: "*",
			SupportedRules: []string{"memory-request-over-provisioned"}},
		{ClusterID: "default", Namespace: "sendant", Workload: "Deployment/bootstrapd", Container: "bootstrapd",
			SupportedRules: []string{"memory-request-over-provisioned"}},
	}, map[string]map[string]string{"sendant": {NamespaceRemediationLabel: NamespaceRemediationValue}})
	target, ok := c.TargetForRule("memory-request-over-provisioned", "sendant", "Deployment/bootstrapd")
	if !ok || target.RequiresNamespaceOptIn {
		t.Fatalf("exact rule authorization should not require wildcard opt-in recheck, got %#v, %v", target, ok)
	}
}

func TestWildcardDefaultsFillContainerlessExactTarget(t *testing.T) {
	c := NewWithNamespaceLabels("default", []Target{
		{ClusterID: "default", Namespace: "*", Workload: "*", Container: "*",
			SupportedRules: []string{"memory-request-over-provisioned"}},
		{ClusterID: "default", Namespace: "sendant", Workload: "Deployment/bootstrapd",
			SupportedRules: []string{"runtime-modernization-candidate"}},
	}, map[string]map[string]string{"sendant": {NamespaceRemediationLabel: NamespaceRemediationValue}})
	target, ok := c.TargetForRule("memory-request-over-provisioned", "sendant", "Deployment/bootstrapd")
	if !ok || target.Container != "*" || !target.RequiresNamespaceOptIn {
		t.Fatalf("expected merged wildcard defaults, got %#v, %v", target, ok)
	}
}
