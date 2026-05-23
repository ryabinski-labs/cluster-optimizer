package applier

import (
	"context"
	"errors"
	"testing"

	"github.com/GipsyChef/cluster-optimizer/internal/plan"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func makeDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "api",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("512Mi"),
								corev1.ResourceCPU:    resource.MustParse("200m"),
							},
						},
					}},
				},
			},
		},
	}
}

func sampleAction() plan.PlannedAction {
	return plan.PlannedAction{
		Kind:          plan.PatchRequest,
		Namespace:     "default",
		WorkloadKind:  "Deployment",
		WorkloadName:  "api",
		Container:     "api",
		CurrentMemMiB: 512,
		NewMemMiB:     128,
		CurrentCPUm:   -1,
		NewCPUm:       -1,
		FindingRuleID: "memory-request-over-provisioned",
		Confidence:    "high",
		Reason:        "observed memory 50Mi",
	}
}

func TestApplyDryRunByDefault(t *testing.T) {
	dep := makeDeployment()
	client := fake.NewSimpleClientset(dep)
	p := plan.Plan{Actions: []plan.PlannedAction{sampleAction()}}
	result := Apply(context.Background(), client, p, NewOptions())
	if !result.DryRun {
		t.Fatal("expected dry-run when AutoApply is false")
	}
	if len(result.Outcomes) != 1 || result.Outcomes[0].Applied {
		t.Fatalf("expected no application in dry-run, got %#v", result.Outcomes)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" {
			t.Fatalf("dry-run must not issue patch verbs, got %v", action)
		}
	}
}

func TestApplyRequiresBothGates(t *testing.T) {
	dep := makeDeployment()
	client := fake.NewSimpleClientset(dep)
	p := plan.Plan{Actions: []plan.PlannedAction{sampleAction()}}
	// Only flag set, env not set → still dry-run.
	opts := NewOptions()
	opts.AutoApply = true
	result := Apply(context.Background(), client, p, opts)
	if !result.DryRun {
		t.Fatal("flag alone must not enable live apply")
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" {
			t.Fatalf("expected no patch when env gate missing, got %v", action)
		}
	}
}

// Mirror of TestApplyRequiresBothGates: env without flag must also stay dry-run.
// The dual gate is the headline safety property; both directions need to fail
// closed or a regression could silently turn live apply on.
func TestApplyRequiresBothGatesMirror(t *testing.T) {
	dep := makeDeployment()
	client := fake.NewSimpleClientset(dep)
	p := plan.Plan{Actions: []plan.PlannedAction{sampleAction()}}
	opts := NewOptions()
	opts.AutoApplyEnvSet = true
	result := Apply(context.Background(), client, p, opts)
	if !result.DryRun {
		t.Fatal("env alone must not enable live apply")
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" {
			t.Fatalf("expected no patch when flag gate missing, got %v", action)
		}
	}
}

func TestApplyLiveAppliesPatch(t *testing.T) {
	dep := makeDeployment()
	client := fake.NewSimpleClientset(dep)
	p := plan.Plan{Actions: []plan.PlannedAction{sampleAction()}}
	opts := NewOptions()
	opts.AutoApply = true
	opts.AutoApplyEnvSet = true
	result := Apply(context.Background(), client, p, opts)
	if result.DryRun {
		t.Fatal("expected live mode when both gates true")
	}
	if len(result.Outcomes) != 1 || !result.Outcomes[0].Applied {
		t.Fatalf("expected outcome to be applied, got %#v", result.Outcomes)
	}
	// Confirm a patch verb was issued for the right resource.
	sawPatch := false
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" && action.GetResource().Resource == "deployments" {
			sawPatch = true
		}
	}
	if !sawPatch {
		t.Fatal("expected deployment patch verb")
	}
}

func TestApplyHaltSwitchAborts(t *testing.T) {
	dep := makeDeployment()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultHaltConfigMap, Namespace: DefaultHaltNamespace},
		Data:       map[string]string{DefaultHaltKey: "true"},
	}
	client := fake.NewSimpleClientset(dep, cm)
	p := plan.Plan{Actions: []plan.PlannedAction{sampleAction()}}
	opts := NewOptions()
	opts.AutoApply = true
	opts.AutoApplyEnvSet = true
	result := Apply(context.Background(), client, p, opts)
	if !result.Halted {
		t.Fatal("expected halt switch to abort")
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" {
			t.Fatalf("halt must prevent patches, got %v", action)
		}
	}
}

func TestApplyHaltUnreadableFailsClosed(t *testing.T) {
	dep := makeDeployment()
	client := fake.NewSimpleClientset(dep)
	// Make ConfigMap GET return a non-NotFound error (forbidden).
	client.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	p := plan.Plan{Actions: []plan.PlannedAction{sampleAction()}}
	opts := NewOptions()
	opts.AutoApply = true
	opts.AutoApplyEnvSet = true
	result := Apply(context.Background(), client, p, opts)
	if !result.Halted {
		t.Fatal("expected fail-closed halt when ConfigMap unreadable")
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" {
			t.Fatalf("fail-closed halt must prevent patches, got %v", action)
		}
	}
}
