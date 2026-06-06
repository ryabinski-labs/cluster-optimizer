package main

import (
	"testing"
	"time"

	"github.com/GipsyChef/cluster-optimizer/internal/applier"
	"github.com/GipsyChef/cluster-optimizer/internal/nudger"
	"github.com/GipsyChef/cluster-optimizer/internal/plan"
	"github.com/GipsyChef/cluster-optimizer/internal/podgc"
)

func TestApplierEventsCaptureBeforeAfterAndMode(t *testing.T) {
	ts := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	result := applier.Result{
		DryRun: false,
		Outcomes: []applier.Outcome{
			{
				Action: plan.PlannedAction{
					Namespace:     "default",
					WorkloadKind:  "Deployment",
					WorkloadName:  "echothread-api",
					Container:     "echothread-api",
					FindingRuleID: "memory-request-over-provisioned",
					CurrentMemMiB: 512,
					NewMemMiB:     384,
				},
				Applied: true,
			},
			{
				Action: plan.PlannedAction{
					Namespace:    "default",
					WorkloadKind: "Deployment",
					WorkloadName: "echothread-api",
				},
				Error: "patch failed: nope",
			},
		},
	}
	events := applierEvents(result, ts)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Mode != "live" {
		t.Errorf("expected live mode, got %q", events[0].Mode)
	}
	if events[0].BeforeMemMiB != 512 || events[0].AfterMemMiB != 384 {
		t.Errorf("expected mem 512->384, got %d->%d", events[0].BeforeMemMiB, events[0].AfterMemMiB)
	}
	if !events[0].Applied {
		t.Error("expected first event to be Applied")
	}
	if events[1].Error == "" {
		t.Error("expected error captured on second event")
	}
}

func TestApplierEventsDryRunFlag(t *testing.T) {
	events := applierEvents(applier.Result{DryRun: true, Outcomes: []applier.Outcome{{}}}, time.Now())
	if len(events) != 1 || events[0].Mode != "dry-run" {
		t.Fatalf("expected single dry-run event, got %+v", events)
	}
}

func TestNudgerEventSkipsEmptyPass(t *testing.T) {
	if _, ok := nudgerEvent(nudger.Result{Mode: "dry-run"}, time.Now()); ok {
		t.Fatal("expected empty pass (no halt, no target, no reason) to be skipped")
	}
}

func TestNudgerEventCapturesTargetAndEvictionCounts(t *testing.T) {
	event, ok := nudgerEvent(nudger.Result{
		Mode:       "live",
		TargetNode: "node-1",
		Evicted:    3,
	}, time.Now())
	if !ok {
		t.Fatal("expected event for non-empty pass")
	}
	if event.TargetNode != "node-1" || event.Evicted != 3 {
		t.Errorf("unexpected event payload: %+v", event)
	}
	if !event.Applied {
		t.Error("expected live success to be marked applied")
	}
}

func TestNudgerEventPreservesHaltReason(t *testing.T) {
	event, ok := nudgerEvent(nudger.Result{Mode: "live", Halted: true, HaltReason: "halt=true"}, time.Now())
	if !ok {
		t.Fatal("expected event when halted")
	}
	if !event.HaltActive || event.Reason != "halt=true" {
		t.Errorf("expected halt fields set, got %+v", event)
	}
}

func TestPodGCEventSkipsEmptyPass(t *testing.T) {
	if _, ok := podGCEvent(podgc.Result{Mode: "dry-run"}, time.Now()); ok {
		t.Fatal("expected empty pass (no candidates, no halt) to be skipped")
	}
}

func TestPodGCEventCapturesDeletions(t *testing.T) {
	event, ok := podGCEvent(podgc.Result{
		Mode:       "live",
		Namespace:  "default",
		Candidates: 3,
		Deleted:    3,
	}, time.Now())
	if !ok {
		t.Fatal("expected event for non-empty pass")
	}
	if event.Kind != "delete_completed_pod" {
		t.Errorf("expected delete_completed_pod kind, got %q", event.Kind)
	}
	if event.Namespace != "default" || event.Deleted != 3 {
		t.Errorf("unexpected event payload: %+v", event)
	}
	if !event.Applied {
		t.Error("expected live deletion with no errors to be marked applied")
	}
}

// All-namespaces runs must leave Namespace empty rather than stuffing a human
// label like "all namespaces" into the structured namespace field (Finding A).
func TestPodGCEventAllNamespacesLeavesNamespaceEmpty(t *testing.T) {
	event, ok := podGCEvent(podgc.Result{Mode: "dry-run", Namespace: "", Candidates: 2}, time.Now())
	if !ok {
		t.Fatal("expected event when candidates present")
	}
	if event.Namespace != "" {
		t.Errorf("expected empty namespace for all-namespaces run, got %q", event.Namespace)
	}
}

func TestPodGCEventNotAppliedOnDeletionErrors(t *testing.T) {
	event, ok := podGCEvent(podgc.Result{Mode: "live", Candidates: 2, Deleted: 1, DeletionErrors: 1}, time.Now())
	if !ok {
		t.Fatal("expected event for non-empty pass")
	}
	if event.Applied {
		t.Error("expected Applied=false when a deletion errored")
	}
	if event.DeletionErrors != 1 {
		t.Errorf("expected deletion errors captured, got %+v", event)
	}
}

func TestPodGCEventPreservesHaltReason(t *testing.T) {
	event, ok := podGCEvent(podgc.Result{Mode: "live", Halted: true, HaltReason: "halt=true"}, time.Now())
	if !ok {
		t.Fatal("expected event when halted")
	}
	if !event.HaltActive || event.Reason != "halt=true" {
		t.Errorf("expected halt fields set, got %+v", event)
	}
}
