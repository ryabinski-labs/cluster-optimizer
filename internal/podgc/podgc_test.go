package podgc

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func completedPod(name, ns string, phase corev1.PodPhase, finished time.Time) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Status:     corev1.PodStatus{Phase: phase},
	}
	if !finished.IsZero() {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				FinishedAt: metav1.NewTime(finished),
			}},
		}}
	}
	return pod
}

func deleteVerbCount(client *fake.Clientset) int {
	n := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "delete" && action.GetResource().Resource == "pods" {
			n++
		}
	}
	return n
}

func TestDryRunDoesNotDelete(t *testing.T) {
	client := fake.NewSimpleClientset(
		completedPod("done", "default", corev1.PodSucceeded, time.Time{}),
		completedPod("boom", "default", corev1.PodFailed, time.Time{}),
	)
	result, err := CleanCompletedPodsWithResult(context.Background(), client, NewOptions())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Mode != "dry-run" {
		t.Fatalf("expected dry-run mode, got %q", result.Mode)
	}
	if result.Candidates != 2 {
		t.Fatalf("expected 2 candidates, got %d", result.Candidates)
	}
	if result.Deleted != 0 {
		t.Fatalf("dry-run must not delete, deleted=%d", result.Deleted)
	}
	if got := deleteVerbCount(client); got != 0 {
		t.Fatalf("dry-run must not issue delete verbs, got %d", got)
	}
}

func TestLiveDeletesCompletedAndKeepsRunning(t *testing.T) {
	client := fake.NewSimpleClientset(
		completedPod("done", "default", corev1.PodSucceeded, time.Time{}),
		completedPod("boom", "default", corev1.PodFailed, time.Time{}),
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "alive", Namespace: "default"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		},
	)
	opts := NewOptions()
	opts.Live = true
	result, err := CleanCompletedPodsWithResult(context.Background(), client, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Deleted != 2 {
		t.Fatalf("expected 2 deletions, got %d", result.Deleted)
	}
	if result.DeletionErrors != 0 {
		t.Fatalf("expected 0 deletion errors, got %d", result.DeletionErrors)
	}
	if got := deleteVerbCount(client); got != 2 {
		t.Fatalf("expected 2 delete verbs, got %d", got)
	}
	remaining, _ := client.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	if len(remaining.Items) != 1 || remaining.Items[0].Name != "alive" {
		t.Fatalf("expected only running pod to remain, got %+v", remaining.Items)
	}
}

func TestHaltSwitchBlocksDeletion(t *testing.T) {
	opts := NewOptions()
	opts.Live = true
	haltCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: opts.HaltConfigMap, Namespace: opts.HaltNamespace},
		Data:       map[string]string{opts.HaltKey: "true"},
	}
	client := fake.NewSimpleClientset(
		completedPod("done", "default", corev1.PodSucceeded, time.Time{}),
		haltCM,
	)
	result, err := CleanCompletedPodsWithResult(context.Background(), client, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Halted || result.HaltReason != "halt=true" {
		t.Fatalf("expected halt, got halted=%v reason=%q", result.Halted, result.HaltReason)
	}
	if got := deleteVerbCount(client); got != 0 {
		t.Fatalf("halt must prevent deletes, got %d", got)
	}
}

func TestHaltFailsClosedOnReadError(t *testing.T) {
	opts := NewOptions()
	opts.Live = true
	client := fake.NewSimpleClientset(completedPod("done", "default", corev1.PodSucceeded, time.Time{}))
	client.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	result, err := CleanCompletedPodsWithResult(context.Background(), client, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Halted {
		t.Fatalf("expected fail-closed halt on configmap read error")
	}
	if got := deleteVerbCount(client); got != 0 {
		t.Fatalf("fail-closed halt must prevent deletes, got %d", got)
	}
}

func TestNamespaceScope(t *testing.T) {
	client := fake.NewSimpleClientset(
		completedPod("a", "ns1", corev1.PodSucceeded, time.Time{}),
		completedPod("b", "ns2", corev1.PodSucceeded, time.Time{}),
	)
	opts := NewOptions()
	opts.Live = true
	opts.Namespace = "ns1"
	result, err := CleanCompletedPodsWithResult(context.Background(), client, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Candidates != 1 || result.Deleted != 1 {
		t.Fatalf("expected 1 candidate/deletion scoped to ns1, got candidates=%d deleted=%d", result.Candidates, result.Deleted)
	}
	if result.Namespace != "ns1" {
		t.Fatalf("expected result.Namespace=ns1, got %q", result.Namespace)
	}
	if _, err := client.CoreV1().Pods("ns2").Get(context.Background(), "b", metav1.GetOptions{}); err != nil {
		t.Fatalf("pod in ns2 should be untouched: %v", err)
	}
}

func TestPhaseSelection(t *testing.T) {
	client := fake.NewSimpleClientset(
		completedPod("done", "default", corev1.PodSucceeded, time.Time{}),
		completedPod("boom", "default", corev1.PodFailed, time.Time{}),
	)
	opts := NewOptions()
	opts.Live = true
	opts.IncludeFailed = false // succeeded-only
	result, err := CleanCompletedPodsWithResult(context.Background(), client, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Deleted != 1 {
		t.Fatalf("expected only Succeeded pod deleted, got %d", result.Deleted)
	}
	if _, err := client.CoreV1().Pods("default").Get(context.Background(), "boom", metav1.GetOptions{}); err != nil {
		t.Fatalf("Failed pod should be untouched when IncludeFailed=false: %v", err)
	}
}

func TestMinAgeSkipsRecent(t *testing.T) {
	now := time.Now()
	client := fake.NewSimpleClientset(
		completedPod("old", "default", corev1.PodSucceeded, now.Add(-2*time.Hour)),
		completedPod("fresh", "default", corev1.PodSucceeded, now.Add(-1*time.Minute)),
	)
	opts := NewOptions()
	opts.Live = true
	opts.MinAge = time.Hour
	result, err := CleanCompletedPodsWithResult(context.Background(), client, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Deleted != 1 {
		t.Fatalf("expected only the old pod deleted, got %d", result.Deleted)
	}
	if _, err := client.CoreV1().Pods("default").Get(context.Background(), "fresh", metav1.GetOptions{}); err != nil {
		t.Fatalf("fresh pod should survive MinAge guard: %v", err)
	}
}

func TestMaxDeletionsCapsOldestFirst(t *testing.T) {
	now := time.Now()
	client := fake.NewSimpleClientset(
		completedPod("oldest", "default", corev1.PodSucceeded, now.Add(-3*time.Hour)),
		completedPod("middle", "default", corev1.PodSucceeded, now.Add(-2*time.Hour)),
		completedPod("newest", "default", corev1.PodSucceeded, now.Add(-1*time.Hour)),
	)
	opts := NewOptions()
	opts.Live = true
	opts.MaxDeletions = 2
	result, err := CleanCompletedPodsWithResult(context.Background(), client, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Candidates != 3 {
		t.Fatalf("expected 3 candidates counted, got %d", result.Candidates)
	}
	if result.Deleted != 2 {
		t.Fatalf("expected cap of 2 deletions, got %d", result.Deleted)
	}
	// The newest pod must be the survivor.
	if _, err := client.CoreV1().Pods("default").Get(context.Background(), "newest", metav1.GetOptions{}); err != nil {
		t.Fatalf("newest pod should survive the cap: %v", err)
	}
}

func TestNoCompletedPodsIsNoop(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "alive", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	})
	opts := NewOptions()
	opts.Live = true
	result, err := CleanCompletedPodsWithResult(context.Background(), client, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Candidates != 0 || result.Deleted != 0 {
		t.Fatalf("expected no-op, got candidates=%d deleted=%d", result.Candidates, result.Deleted)
	}
}

func TestSkipsTerminatingPods(t *testing.T) {
	terminating := completedPod("terminating", "default", corev1.PodSucceeded, time.Time{})
	ts := metav1.NewTime(time.Now())
	terminating.DeletionTimestamp = &ts
	client := fake.NewSimpleClientset(terminating)
	opts := NewOptions()
	opts.Live = true
	result, err := CleanCompletedPodsWithResult(context.Background(), client, opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Candidates != 0 {
		t.Fatalf("pods already terminating must be skipped, got %d candidates", result.Candidates)
	}
}
