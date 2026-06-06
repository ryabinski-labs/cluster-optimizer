// Package podgc cleans up completed pods (phase Succeeded or Failed) that
// linger on the cluster after Jobs/CronJobs finish. Stranded completed pods
// don't consume CPU/memory, but they clutter `kubectl get pods`, count against
// the per-node pod cap, and hold onto IP allocations — a real source of waste
// on busy clusters running many CronJobs (e.g. ecr-pull-secret-refresh).
//
// Like the nudger, deletion is a live-mutation path: it is dry-run by default,
// honours the shared halt switch, and is gated behind an explicit Live flag.
// Deleting a completed pod is low-risk and effectively reversible — owning
// controllers recreate pods on their next schedule — but we still cap each run
// and refuse to touch anything unless an operator has opted in.
package podgc

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/GipsyChef/cluster-optimizer/internal/applier"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Options gates and configures the pod GC. The zero value plus NewOptions is a
// safe dry-run that considers both Succeeded and Failed pods in every
// namespace.
type Options struct {
	// Live, when true, actually deletes completed pods. Default false:
	// dry-run only logs what it would delete.
	Live bool

	// Namespace restricts the GC to a single namespace. Empty means all
	// namespaces.
	Namespace string

	// IncludeSucceeded / IncludeFailed select which terminal phases are
	// eligible. NewOptions sets both true (match the clean_pods.sh default).
	IncludeSucceeded bool
	IncludeFailed    bool

	// MinAge, when > 0, only deletes pods whose completion finished at least
	// this long ago. Guards against racing a controller that is still
	// reading a just-finished pod's logs/status.
	MinAge time.Duration

	// MaxDeletions caps how many pods a single run will delete (oldest
	// first). 0 means no cap. A blast-radius limit so one tick can't wipe
	// thousands of pods unexpectedly.
	MaxDeletions int

	// HaltNamespace / HaltConfigMap / HaltKey identify the kill switch (the
	// same one the applier and nudger use). Writing halt=true there stops
	// every mutation path.
	HaltNamespace string
	HaltConfigMap string
	HaltKey       string
}

// NewOptions returns Options with the same safe defaults as the other
// mutation paths: dry-run, all namespaces, both terminal phases, shared halt
// switch at cluster-optimizer/cluster-optimizer-halt.
func NewOptions() Options {
	return Options{
		IncludeSucceeded: true,
		IncludeFailed:    true,
		HaltNamespace:    applier.DefaultHaltNamespace,
		HaltConfigMap:    applier.DefaultHaltConfigMap,
		HaltKey:          applier.DefaultHaltKey,
	}
}

// Result summarises one CleanCompletedPods run for the remediation audit log.
// Mode is "live" or "dry-run"; Candidates is how many completed pods matched
// the filters; Deleted/DeletionErrors describe the live outcome (both zero in
// dry-run).
type Result struct {
	Mode       string
	Halted     bool
	HaltReason string
	// Namespace is the raw namespace the run was scoped to, empty for all
	// namespaces. Scope is the human-readable label used in logs.
	Namespace      string
	Scope          string
	Candidates     int
	Deleted        int
	DeletionErrors int
}

// CleanCompletedPods is a thin wrapper that discards the Result.
func CleanCompletedPods(ctx context.Context, clientset kubernetes.Interface, opts Options) error {
	_, err := CleanCompletedPodsWithResult(ctx, clientset, opts)
	return err
}

// CleanCompletedPodsWithResult lists pods, selects those in a terminal phase
// matching the options, and (when Live) deletes them. In dry-run it logs the
// plan and returns without mutating anything.
func CleanCompletedPodsWithResult(ctx context.Context, clientset kubernetes.Interface, opts Options) (Result, error) {
	result := Result{Mode: "dry-run", Namespace: opts.Namespace, Scope: scopeLabel(opts.Namespace)}
	if opts.Live {
		result.Mode = "live"
	}
	mode := "DRY-RUN"
	if opts.Live {
		mode = "LIVE"
	}
	log.Printf("Pod GC (%s): scanning %s for completed pods...", mode, result.Scope)

	if opts.Live {
		if halted, reason := haltCheck(ctx, clientset, opts); halted {
			log.Printf("Pod GC: halt switch active (%s), refusing to delete", reason)
			result.Halted = true
			result.HaltReason = reason
			return result, nil
		}
	}

	// fake clientsets don't honour field selectors, and the rest of this
	// codebase uniformly filters pod phase in Go, so we list everything in
	// scope and filter here.
	podList, err := clientset.CoreV1().Pods(namespaceScope(opts.Namespace)).List(ctx, metav1.ListOptions{})
	if err != nil {
		return result, fmt.Errorf("failed to list pods: %w", err)
	}

	now := time.Now()
	var candidates []corev1.Pod
	for _, pod := range podList.Items {
		if !isCompleted(pod, opts) {
			continue
		}
		// Don't fight pods that are already being torn down.
		if pod.DeletionTimestamp != nil {
			continue
		}
		if opts.MinAge > 0 && now.Sub(completedAt(pod)) < opts.MinAge {
			continue
		}
		candidates = append(candidates, pod)
	}
	result.Candidates = len(candidates)

	if len(candidates) == 0 {
		log.Printf("Pod GC (%s): no completed pods to clean in %s.", mode, result.Scope)
		return result, nil
	}

	// Oldest first, so a MaxDeletions cap retires the longest-lingering pods.
	sort.Slice(candidates, func(i, j int) bool {
		return completedAt(candidates[i]).Before(completedAt(candidates[j]))
	})
	if opts.MaxDeletions > 0 && len(candidates) > opts.MaxDeletions {
		log.Printf("Pod GC (%s): %d completed pods found, capping this run at %d (oldest first).", mode, len(candidates), opts.MaxDeletions)
		candidates = candidates[:opts.MaxDeletions]
	}

	if !opts.Live {
		for _, pod := range candidates {
			log.Printf("Pod GC DRY-RUN: would delete %s/%s (phase %s)", pod.Namespace, pod.Name, pod.Status.Phase)
		}
		log.Printf("Pod GC DRY-RUN: would delete %d completed pod(s). Set CLUSTER_OPTIMIZER_GC_COMPLETED_PODS_LIVE=true to actually delete.", len(candidates))
		return result, nil
	}

	for _, pod := range candidates {
		log.Printf("Pod GC: deleting %s/%s (phase %s)...", pod.Namespace, pod.Name, pod.Status.Phase)
		err := clientset.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// Already gone (raced with the owning controller or another
				// GC pass) — not an error for our purposes.
				continue
			}
			log.Printf("Pod GC: WARNING: failed to delete %s/%s: %v", pod.Namespace, pod.Name, err)
			result.DeletionErrors++
			continue
		}
		result.Deleted++
	}

	log.Printf("Pod GC: deleted %d of %d completed pod(s) in %s (%d error(s)).", result.Deleted, len(candidates), result.Scope, result.DeletionErrors)
	return result, nil
}

// isCompleted reports whether a pod is in a terminal phase the options select.
func isCompleted(pod corev1.Pod, opts Options) bool {
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		return opts.IncludeSucceeded
	case corev1.PodFailed:
		return opts.IncludeFailed
	default:
		return false
	}
}

// completedAt returns the pod's best-known completion time: the latest
// container terminated FinishedAt, falling back to the pod start time and then
// the creation timestamp. Used for both the MinAge guard and oldest-first
// ordering.
func completedAt(pod corev1.Pod) time.Time {
	var latest time.Time
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil && t.FinishedAt.Time.After(latest) {
			latest = t.FinishedAt.Time
		}
	}
	if latest.IsZero() && pod.Status.StartTime != nil {
		latest = pod.Status.StartTime.Time
	}
	if latest.IsZero() {
		latest = pod.CreationTimestamp.Time
	}
	return latest
}

func namespaceScope(ns string) string {
	if ns == "" {
		return metav1.NamespaceAll
	}
	return ns
}

func scopeLabel(ns string) string {
	if ns == "" {
		return "all namespaces"
	}
	return fmt.Sprintf("namespace %q", ns)
}

// haltCheck consults the shared halt configmap. Fail closed: if we can't read
// it, refuse to delete.
func haltCheck(ctx context.Context, clientset kubernetes.Interface, opts Options) (bool, string) {
	cm, err := clientset.CoreV1().ConfigMaps(opts.HaltNamespace).Get(ctx, opts.HaltConfigMap, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, ""
		}
		return true, fmt.Sprintf("unreadable halt configmap: %v", err)
	}
	if cm.Data[opts.HaltKey] == "true" {
		return true, "halt=true"
	}
	return false, ""
}
