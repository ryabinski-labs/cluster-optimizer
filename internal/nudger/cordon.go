package nudger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Cordon ownership annotations. The nudger stamps these onto a node in the
// same API update that sets Unschedulable, so a cordon this tool placed is
// always attributable and always reversible — there is no window in which a
// node is cordoned without a marker saying who did it and what to restore.
const (
	// AnnotationCordonedAt is the RFC3339 timestamp of the cordon.
	AnnotationCordonedAt = "cluster-optimizer.io/cordoned-at"
	// AnnotationCordonedByRun identifies the process that placed the cordon.
	// A marker carrying a run ID other than the current one was written by a
	// process that has since exited.
	AnnotationCordonedByRun = "cluster-optimizer.io/cordoned-by-run"
	// AnnotationPriorUnschedulable records node.Spec.Unschedulable as it was
	// *before* the cordon, so the reaper restores the operator's state rather
	// than blindly uncordoning a node that was already cordoned by a human.
	AnnotationPriorUnschedulable = "cluster-optimizer.io/prior-unschedulable"
	// AnnotationCordonReapedAt is stamped when the reaper reverses a stale
	// cordon. It drives the re-cordon cooldown so reaping cannot turn into a
	// cordon/evict/uncordon loop against the same node.
	AnnotationCordonReapedAt = "cluster-optimizer.io/cordon-reaped-at"
)

// DefaultCordonTTL is how long a cordon this tool placed may stand before the
// reaper treats it as abandoned and gives the capacity back.
//
// A cordon is only ever a means to an end: the node is drained so that an
// autoscaler or an operator can remove it. If nothing removes the node, the
// cordon is not "in progress", it is lost capacity — and the cluster has no
// other mechanism that will ever notice. 30 minutes is comfortably longer
// than any single consolidation pass and short enough that a crashed run
// costs at most one CronJob interval of scheduling headroom.
const DefaultCordonTTL = 30 * time.Minute

// DefaultRecordonCooldown is how long a node is excluded from consolidation
// candidacy after the reaper uncordons it. Without this the next run would
// immediately re-target the same node, re-evict the same pods, and the pair
// would oscillate forever — churning workloads while saving nothing.
const DefaultRecordonCooldown = 2 * time.Hour

// ReapResult summarises one pass of the stale-cordon reaper.
type ReapResult struct {
	// Uncordoned lists nodes whose cordon this run reversed.
	Uncordoned []string
	// Cleared lists nodes that carried our markers but were no longer
	// cordoned (an operator uncordoned them by hand); we only stripped the
	// stale annotations.
	Cleared []string
	// Held lists cordons that are ours but still within the TTL.
	Held []string
	// Errors holds per-node failures. A reap failure never aborts the run:
	// leaving a stale cordon in place is the status quo, not a regression.
	Errors []string
}

// Reaped reports whether the pass changed anything worth an audit row.
func (r ReapResult) Reaped() bool {
	return len(r.Uncordoned) > 0 || len(r.Cleared) > 0 || len(r.Errors) > 0
}

// newRunID returns an identifier unique to this process invocation. It is
// written onto every cordon so a later run can tell "I placed this" from
// "some earlier process placed this and never came back".
func newRunID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// A weaker ID still distinguishes runs across CronJob ticks, which
		// is all this is for; it is not a security boundary.
		return fmt.Sprintf("t%d", time.Now().UTC().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// claimCordon cordons a node and stamps ownership in a single API update.
//
// Doing both in one write is what makes the cordon recoverable: the API
// server applies the whole object or none of it, so there is no interleaving
// in which the process dies after cordoning but before recording that it did.
// Every cordon this tool places is therefore reapable by the next run.
func claimCordon(ctx context.Context, clientset kubernetes.Interface, node *corev1.Node, runID string, now time.Time) error {
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[AnnotationCordonedAt] = now.UTC().Format(time.RFC3339)
	node.Annotations[AnnotationCordonedByRun] = runID
	node.Annotations[AnnotationPriorUnschedulable] = fmt.Sprintf("%t", node.Spec.Unschedulable)
	// A fresh cordon supersedes any earlier reap; drop the cooldown stamp so
	// the annotation set describes only the current state.
	delete(node.Annotations, AnnotationCordonReapedAt)
	node.Spec.Unschedulable = true

	if _, err := clientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("failed to cordon node %q: %w", node.Name, err)
	}
	return nil
}

// ReapStaleCordons reverses cordons this tool placed that have outlived
// DefaultCordonTTL, and clears markers left on nodes an operator has already
// uncordoned by hand.
//
// It runs at the start of every consolidation pass, before any new cordon is
// considered, so a crashed predecessor's damage is repaired before this run
// adds more. Cordons placed by anything other than this tool are never
// touched — an unannotated cordon is an operator's deliberate act.
//
// When live is false the pass is read-only: it reports what it would reverse
// and mutates nothing.
func ReapStaleCordons(ctx context.Context, clientset kubernetes.Interface, live bool, runID string, ttl time.Duration, now time.Time) ReapResult {
	var result ReapResult
	if ttl <= 0 {
		return result
	}

	nodeList, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("list nodes: %v", err))
		return result
	}

	for i := range nodeList.Items {
		node := nodeList.Items[i]
		stamp, ours := node.Annotations[AnnotationCordonedAt]
		if !ours {
			continue // not ours; leave it alone
		}
		if node.Annotations[AnnotationCordonedByRun] == runID {
			continue // placed by this very run
		}

		// An operator already uncordoned it. Nothing to reverse; just clear
		// the markers so the node stops looking like it has work outstanding.
		if !node.Spec.Unschedulable {
			if !live {
				result.Cleared = append(result.Cleared, node.Name)
				continue
			}
			if err := stripCordonMarkers(ctx, clientset, node.Name, now); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", node.Name, err))
				continue
			}
			result.Cleared = append(result.Cleared, node.Name)
			continue
		}

		cordonedAt, parseErr := time.Parse(time.RFC3339, stamp)
		if parseErr != nil {
			// Our own marker is unreadable. Returning the capacity is the
			// safe direction: the alternative is a node cordoned forever
			// because a timestamp could not be parsed.
			log.Printf("Active Nudger: node %q has an unparseable %s (%q); treating the cordon as stale", node.Name, AnnotationCordonedAt, stamp)
		} else if now.Sub(cordonedAt) < ttl {
			result.Held = append(result.Held, node.Name)
			continue
		}

		if !live {
			result.Uncordoned = append(result.Uncordoned, node.Name)
			continue
		}
		if err := releaseCordon(ctx, clientset, node.Name, now); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", node.Name, err))
			continue
		}
		log.Printf("Active Nudger: reaped stale cordon on node %q (placed %s by run %q); node is schedulable again",
			node.Name, stamp, node.Annotations[AnnotationCordonedByRun])
		result.Uncordoned = append(result.Uncordoned, node.Name)
	}

	sort.Strings(result.Uncordoned)
	sort.Strings(result.Cleared)
	sort.Strings(result.Held)
	sort.Strings(result.Errors)
	return result
}

// releaseCordon restores the node's pre-cordon schedulability and stamps the
// re-cordon cooldown. It re-reads the node so the update lands on the current
// resourceVersion rather than the copy from the List above.
func releaseCordon(ctx context.Context, clientset kubernetes.Interface, name string, now time.Time) error {
	node, err := clientset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node for uncordon: %w", err)
	}
	// Respect a cordon that predates ours: if the node was already
	// unschedulable when we claimed it, leave it unschedulable.
	priorUnschedulable := node.Annotations[AnnotationPriorUnschedulable] == "true"
	node.Spec.Unschedulable = priorUnschedulable
	applyReapMarkers(node, now)
	if _, err := clientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("uncordon: %w", err)
	}
	return nil
}

// stripCordonMarkers removes our annotations without touching schedulability.
// Used when an operator has already uncordoned the node themselves.
func stripCordonMarkers(ctx context.Context, clientset kubernetes.Interface, name string, now time.Time) error {
	node, err := clientset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node for marker cleanup: %w", err)
	}
	applyReapMarkers(node, now)
	if _, err := clientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("clear cordon markers: %w", err)
	}
	return nil
}

func applyReapMarkers(node *corev1.Node, now time.Time) {
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	delete(node.Annotations, AnnotationCordonedAt)
	delete(node.Annotations, AnnotationCordonedByRun)
	delete(node.Annotations, AnnotationPriorUnschedulable)
	node.Annotations[AnnotationCordonReapedAt] = now.UTC().Format(time.RFC3339)
}

// inRecordonCooldown reports whether a node was uncordoned by the reaper
// recently enough that re-targeting it would just restart the loop the reaper
// exists to break.
func inRecordonCooldown(node corev1.Node, cooldown time.Duration, now time.Time) bool {
	if cooldown <= 0 {
		return false
	}
	stamp, ok := node.Annotations[AnnotationCordonReapedAt]
	if !ok {
		return false
	}
	reapedAt, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		// Unreadable stamp: assume the cooldown is active. Skipping one node
		// for one run is cheap; re-entering an eviction loop is not.
		return true
	}
	return now.Sub(reapedAt) < cooldown
}
