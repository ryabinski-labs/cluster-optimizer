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

// TestRemediationTargetsFromEvidenceProposesRealReductions locks the invariant
// that, given per-replica evidence (the analyzer now emits per-pod values), the
// memory/CPU over-provisioned targets are a strict reduction below the parsed
// per-pod request and the below-usage target is an increase above observed
// usage. A target equal to the current request would be a no-op recommendation
// that recurs forever, which is the defect this guards against.
func TestRemediationTargetsFromEvidenceProposesRealReductions(t *testing.T) {
	// memory over-provisioned: max(128, 50*3/2=75)=128, capped at 512*0.8=409,
	// rounded up to 16 => 128Mi, a real reduction below the 512Mi per-pod request.
	if _, mem := remediationTargetsFromEvidence(
		"memory-request-over-provisioned",
		"Observed memory 50Mi is less than half of request 512Mi.",
	); mem != "128Mi" {
		t.Fatalf("memory over-provisioned target = %q, want 128Mi (a reduction below 512Mi)", mem)
	}

	// cpu over-provisioned: max(50, 15*3=45)=50, capped at 100*0.7=70, floor 25,
	// rounded up to 5 => 50m, a real reduction below the 100m per-pod request.
	if cpu, _ := remediationTargetsFromEvidence(
		"cpu-request-over-provisioned",
		"Observed CPU 15m is materially below request 100m.",
	); cpu != "50m" {
		t.Fatalf("cpu over-provisioned target = %q, want 50m (a reduction below 100m)", cpu)
	}

	// below-usage: 300*5/4=375, rounded up to 16 => 384Mi, above observed usage.
	if _, mem := remediationTargetsFromEvidence(
		"memory-request-below-usage",
		"Observed memory 300Mi exceeds request 128Mi.",
	); mem != "384Mi" {
		t.Fatalf("below-usage target = %q, want 384Mi (above observed 300Mi usage)", mem)
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

// Lock the frontend filter-segment values to the set the activity-row
// renderer's filterActivity() switch expects. Adding a filter without
// updating the renderer is a silent regression that produces a pill that
// returns zero events. The skipped-event collapse moved out of the
// segmented filter and into the "Show skipped inline" toggle, so the
// segmented set is intentionally narrower.
func TestDashboardActivityFilterSegments(t *testing.T) {
	page, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read dashboard html: %v", err)
	}
	body := string(page)
	for _, want := range []string{
		`data-activity-filter="all"`,
		`data-activity-filter="errors"`,
		`data-activity-filter="dry-run"`,
		`id="activitySkipsInline"`,
		`id="activityLive"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard html is missing required activity control %q", want)
		}
	}
	for _, removed := range []string{
		`data-activity-filter="active"`,
		`data-activity-filter="live"`,
		`data-activity-filter="skips"`,
	} {
		if strings.Contains(body, removed) {
			t.Fatalf("dashboard html still contains retired filter segment %q", removed)
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

// Lock the QA-driven fixes from the activity-feed redesign so they
// don't silently regress:
//   - Sticky run divider must use a JS-measured CSS variable, not the
//     56px desktop guess that hid the divider under the mobile topbar.
//   - Skip-reason help links must point to anchors that actually exist
//     in README.md AND are specific to a fix path. The previous
//     iteration sent every skip reason to a single generic anchor
//     (`#live-auto-apply-opt-in`), which set the expectation of a
//     specific answer and delivered a generic bullet list — so users
//     learned the help affordance didn't help. Only the actionable
//     subset (no-target / missing-container → `#remediation-workflow`,
//     persistence → `#dynamodb-persistence`) carries a link; the
//     intentional guardrail rows (low confidence, provider-managed,
//     no safe trim, needs more runs) must not.
//   - The live pill must render "just now" rather than "0s ago"
//     immediately after a refresh; "0s ago" reads as broken.
func TestActivityFeedQARegressionLocks(t *testing.T) {
	script, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read dashboard script: %v", err)
	}
	js := string(script)
	for _, want := range []string{
		"function syncActivityStickyTop",
		"--activity-sticky-top",
		`helpLink("remediation-workflow")`,
		`helpLink("dynamodb-persistence")`,
		`"Configure target →"`,
		`"Enable persistence →"`,
		`"just now"`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("dashboard script is missing QA-lock token %q", want)
		}
	}
	for _, banned := range []string{
		`README.md#min-confidence`,
		`README.md#remediation-targets`,
		`README.md#live-auto-apply-opt-in`,
		`README.md#provider-managed`,
		`README.md#safe-trim`,
		`README.md#min-occurrences`,
		`helpLink("min-confidence")`,
		`helpLink("provider-managed")`,
		`helpLink("safe-trim")`,
		`helpLink("min-occurrences")`,
	} {
		if strings.Contains(js, banned) {
			t.Fatalf("dashboard script must not use %q — either the README anchor doesn't exist, or the reason is an intentional guardrail with no fix path and shouldn't carry a help link", banned)
		}
	}

	styles, err := os.ReadFile("static/styles.css")
	if err != nil {
		t.Fatalf("read dashboard styles: %v", err)
	}
	if !strings.Contains(string(styles), "var(--activity-sticky-top") {
		t.Fatalf("activity-run-divider must read --activity-sticky-top so the sticky offset matches the live topbar height")
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	for _, heading := range []string{
		"## Remediation Workflow",
		"## DynamoDB Persistence",
	} {
		if !strings.Contains(string(readme), heading) {
			t.Fatalf("README missing %q — the activity-feed help links deep-link to its anchor", heading)
		}
	}
}
