package main

import (
	"testing"
	"time"

	"github.com/GipsyChef/cluster-optimizer/internal/applier"
	"github.com/GipsyChef/cluster-optimizer/internal/nudger"
	"github.com/GipsyChef/cluster-optimizer/internal/plan"
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
