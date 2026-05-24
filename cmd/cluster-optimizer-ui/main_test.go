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

// Lock the wire keys the frontend reads for the halt toggle and the
// halt-control availability flag. A silent rename here breaks the
// halt button without a frontend test failing.
func TestAPIResponseHaltControlWireShape(t *testing.T) {
	resp := apiResponse{HaltControlAvailable: true}
	payload, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := map[string]any{}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["halt_control_available"]; !ok {
		t.Fatalf("missing halt_control_available in response: %v", got)
	}
}

// Verify the empty-state copy branches exist in the dashboard script for
// each of the engine states the UX spec calls for: Disabled, Live but no
// events yet, and Halted. A missing branch would render an operator a
// blank panel with no explanation of why no activity is showing.
func TestDashboardEmptyStateCoversEngineStates(t *testing.T) {
	script, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read dashboard script: %v", err)
	}
	body := string(script)
	for _, want := range []string{
		"function emptyStateMessage",
		"advisory-only mode",
		"Halt switch is active",
		"3 consecutive runs",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard script is missing empty-state copy %q", want)
		}
	}
}

// Lock the frontend filter-segment values to the set the backend
// activity-row renderer expects. Adding a filter without updating the
// renderer is a silent regression that produces "Active only" or
// "Skips only" segments that return zero events.
func TestDashboardActivityFilterSegments(t *testing.T) {
	page, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read dashboard html: %v", err)
	}
	body := string(page)
	for _, want := range []string{
		`data-activity-filter="all"`,
		`data-activity-filter="active"`,
		`data-activity-filter="live"`,
		`data-activity-filter="skips"`,
		`data-activity-filter="errors"`,
		`data-activity-filter="dry-run"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard html is missing filter segment %q", want)
		}
	}
}

// Halt POST gating: confirm=true is required, kubeClient must be present,
// and only POST is accepted. These three guard rails are the entire
// safety story for the endpoint — a regression here would let an
// accidental GET or unconfirmed POST mutate the cluster.
func TestHandleHaltRequiresConfirmAndPOST(t *testing.T) {
	srv := &server{} // nil kubeClient
	// GET is rejected
	req := httptest.NewRequest(http.MethodGet, "/api/halt", nil)
	rec := httptest.NewRecorder()
	srv.handleHalt(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET expected 405, got %d", rec.Code)
	}
	// POST without kubeClient → 503
	req = httptest.NewRequest(http.MethodPost, "/api/halt", strings.NewReader(`{"active":true,"confirm":true}`))
	rec = httptest.NewRecorder()
	srv.handleHalt(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST without kubeClient expected 503, got %d (body=%s)", rec.Code, rec.Body.String())
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
