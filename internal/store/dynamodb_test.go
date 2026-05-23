package store

import (
	"encoding/json"
	"testing"
	"time"
)

// The audit feed and engine sentinel are read back into Go structs verbatim,
// so any wire-format drift would silently break the UI. Lock the JSON keys
// down so a careless rename to RemediationEvent or EngineStatus surfaces here
// instead of as an empty card in the dashboard.
func TestRemediationEventJSONShape(t *testing.T) {
	event := RemediationEvent{
		Timestamp:    time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC),
		Mode:         "live",
		Kind:         "patch_request",
		Namespace:    "default",
		Workload:     "echothread-api",
		WorkloadKind: "Deployment",
		Container:    "echothread-api",
		RuleID:       "memory-request-over-provisioned",
		BeforeCPUm:   500,
		AfterCPUm:    250,
		BeforeMemMiB: 512,
		AfterMemMiB:  384,
		Applied:      true,
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := map[string]any{}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"timestamp", "mode", "kind", "namespace", "workload", "workload_kind",
		"container", "rule_id", "before_cpu_m", "after_cpu_m",
		"before_memory_mib", "after_memory_mib", "applied",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("expected key %q in marshaled event, got %v", key, got)
		}
	}
}

func TestEngineStatusJSONShape(t *testing.T) {
	status := EngineStatus{
		AutoApplyEnabled: true,
		AutoApplyLive:    true,
		NudgeEnabled:     false,
		HaltActive:       true,
		HaltReason:       "halt=true",
		LastRunAt:        time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC),
		LastRunActions:   3,
		LastRunApplied:   2,
		LastRunErrors:    1,
		LastClusterID:    "default",
	}
	payload, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := map[string]any{}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"auto_apply_enabled", "auto_apply_live", "nudge_enabled", "halt_active",
		"halt_reason", "last_run_at", "last_run_actions", "last_run_applied",
		"last_run_errors",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("expected key %q in marshaled status, got %v", key, got)
		}
	}
}

// RemediationEvent for the nudger path omits the resource fields — make sure
// the omitempty tags actually drop them so the UI doesn't render misleading
// "cpu 0m → 0m" rows.
func TestRemediationEventOmitsEmptyResourceFields(t *testing.T) {
	event := RemediationEvent{
		Timestamp:  time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC),
		Mode:       "live",
		Kind:       "cordon_evict",
		TargetNode: "node-1",
		Evicted:    2,
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := map[string]any{}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"before_cpu_m", "after_cpu_m", "before_memory_mib", "after_memory_mib", "container", "rule_id"} {
		if _, ok := got[key]; ok {
			t.Errorf("expected %q to be omitted from cordon_evict event, got %v", key, got)
		}
	}
}
