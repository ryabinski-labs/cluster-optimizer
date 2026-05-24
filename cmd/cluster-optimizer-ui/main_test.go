package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDaysBetweenInclusiveCountsUTCCalendarDays(t *testing.T) {
	start := time.Date(2026, 5, 19, 23, 30, 0, 0, time.UTC)
	end := time.Date(2026, 5, 20, 0, 30, 0, 0, time.UTC)

	if got := daysBetweenInclusive(start, end); got != 2 {
		t.Fatalf("daysBetweenInclusive() = %d, want 2", got)
	}
}

func TestDaysBetweenInclusiveSameUTCDate(t *testing.T) {
	start := time.Date(2026, 5, 19, 5, 30, 0, 0, time.UTC)
	end := time.Date(2026, 5, 19, 23, 30, 0, 0, time.UTC)

	if got := daysBetweenInclusive(start, end); got != 1 {
		t.Fatalf("daysBetweenInclusive() = %d, want 1", got)
	}
}

func TestDaysBetweenInclusiveSwapsReversedInputs(t *testing.T) {
	start := time.Date(2026, 5, 20, 0, 30, 0, 0, time.UTC)
	end := time.Date(2026, 5, 19, 23, 30, 0, 0, time.UTC)

	if got := daysBetweenInclusive(start, end); got != 2 {
		t.Fatalf("daysBetweenInclusive() = %d, want 2", got)
	}
}

func TestRuleCanPatchAPIYAMLCoversHPASensitivity(t *testing.T) {
	if !ruleCanPatchAPIYAML("cpu-hpa-low-request-sensitive") {
		t.Fatal("cpu-hpa-low-request-sensitive should be patchable as an api.yml change")
	}
	if ruleCanPatchAPIYAML("fixed-replica-capacity-without-autoscaler") {
		t.Fatal("fixed-replica-capacity-without-autoscaler is not an api.yml patch rule")
	}
}

func TestNeedsResourceTargetExcludesHPASensitivity(t *testing.T) {
	if needsResourceTarget("cpu-hpa-low-request-sensitive") {
		t.Fatal("cpu-hpa-low-request-sensitive must not require a CPU/memory request target")
	}
}

func TestRemediationHistoryRejectsInvalidLimit(t *testing.T) {
	srv := &server{}
	cases := []string{"abc", "0", "999"}
	for _, raw := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/remediations/history?limit="+raw, nil)
		rec := httptest.NewRecorder()
		srv.handleRemediationHistory(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("limit=%q expected 400, got %d", raw, rec.Code)
		}
	}
}

func TestRemediationHistoryRejectsInvalidSince(t *testing.T) {
	srv := &server{}
	req := httptest.NewRequest(http.MethodGet, "/api/remediations/history?since=yesterday", nil)
	rec := httptest.NewRecorder()
	srv.handleRemediationHistory(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad since, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "since must be RFC3339") {
		t.Fatalf("expected RFC3339 message, got %s", rec.Body.String())
	}
}

// Lock the engine_status wire shape — the UI strip reads these keys directly
// and a silent rename would render every cluster's status as "Unknown".
func TestAPIResponseEngineStatusWireShape(t *testing.T) {
	resp := apiResponse{
		EngineStatus: &engineStatus{
			AutoApplyEnabled: true,
			AutoApplyLive:    true,
			NudgeEnabled:     false,
			HaltActive:       true,
			HaltReason:       "halt=true",
			LastRunAt:        time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC),
			LastRunActions:   3,
		},
	}
	payload, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := map[string]any{}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	status, ok := got["engine_status"].(map[string]any)
	if !ok {
		t.Fatalf("expected engine_status object, got %v", got["engine_status"])
	}
	for _, key := range []string{"auto_apply_enabled", "auto_apply_live", "halt_active", "halt_reason", "last_run_at", "last_run_actions"} {
		if _, ok := status[key]; !ok {
			t.Errorf("missing engine_status key %q in %v", key, status)
		}
	}
}

func TestDashboardRefreshesReportsAndRelativeTimes(t *testing.T) {
	script, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read dashboard script: %v", err)
	}
	body := string(script)
	for _, want := range []string{
		"const REPORT_REFRESH_INTERVAL_MS = 60 * 1000;",
		"const RELATIVE_TIME_TICK_MS = 30 * 1000;",
		"setInterval(() => loadReports({ preserveSelection: true }), REPORT_REFRESH_INTERVAL_MS);",
		"setInterval(refreshRelativeTimes, RELATIVE_TIME_TICK_MS);",
		"function refreshRelativeTimes()",
		"renderEngineStatus();",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard script is missing %q", want)
		}
	}
}
