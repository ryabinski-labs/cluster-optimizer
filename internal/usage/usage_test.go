package usage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFidelity_OnlyTimeSpanningEvidenceIsActionable(t *testing.T) {
	actionable := []Fidelity{FidelityHistoricalP95, FidelityRecommender, FidelityRollup}
	for _, f := range actionable {
		if !f.Actionable() {
			t.Errorf("%s should be actionable", f)
		}
	}
	// A single sample cannot tell an idle workload from one sampled between
	// bursts, so it must never justify removing capacity.
	for _, f := range []Fidelity{FidelityInstant, FidelityNone, Fidelity("something-new")} {
		if f.Actionable() {
			t.Errorf("%s must not be actionable", f)
		}
	}
}

func TestFidelity_RankOrdersStrongestFirst(t *testing.T) {
	if FidelityHistoricalP95.Rank() <= FidelityRollup.Rank() {
		t.Error("a real percentile should outrank our own sampled rollup")
	}
	if FidelityRollup.Rank() <= FidelityInstant.Rank() {
		t.Error("a rollup should outrank a single sample")
	}
	if FidelityInstant.Rank() <= FidelityNone.Rank() {
		t.Error("some data should outrank no data")
	}
}

func TestSet_Coverage(t *testing.T) {
	set := Set{Pods: map[string]Reading{
		"default/a": {CPUm: 10},
		"default/b": {CPUm: 20},
	}}
	keys := []string{"default/a", "default/b", "default/c", "default/d"}
	if got := set.Coverage(keys); got != 0.5 {
		t.Errorf("expected 0.5 coverage, got %v", got)
	}
	if got := set.Coverage(nil); got != 0 {
		t.Errorf("expected 0 coverage for no keys, got %v", got)
	}
}

func TestPromDuration(t *testing.T) {
	cases := map[time.Duration]string{
		7 * 24 * time.Hour: "7d",
		24 * time.Hour:     "1d",
		6 * time.Hour:      "6h",
		90 * time.Minute:   "90m",
		0:                  "7d",
	}
	for in, want := range cases {
		if got := promDuration(in); got != want {
			t.Errorf("promDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

// promServer returns a stub Prometheus that answers both queries with the
// supplied vector payloads, in the order they are requested (CPU, then memory).
func promServer(t *testing.T, payloads ...string) *httptest.Server {
	t.Helper()
	var calls int
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		idx := calls
		calls++
		if idx >= len(payloads) {
			idx = len(payloads) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(payloads[idx]))
	}))
}

func TestPrometheusProvider_ParsesVectorAndConvertsUnits(t *testing.T) {
	cpu := `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"namespace":"default","pod":"web-1"},"value":[1755259200,"0.25"]}
	]}}`
	mem := `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"namespace":"default","pod":"web-1"},"value":[1755259200,"536870912"]}
	]}}`
	srv := promServer(t, cpu, mem)
	defer srv.Close()

	p := &PrometheusProvider{BaseURL: srv.URL}
	set, err := p.PodUsage(context.Background(), 7*24*time.Hour, 0.95)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if set.Fidelity != FidelityHistoricalP95 {
		t.Errorf("expected historical p95 fidelity, got %s", set.Fidelity)
	}
	r, ok := set.Get("default", "web-1")
	if !ok {
		t.Fatal("expected a reading for default/web-1")
	}
	if r.CPUm != 250 {
		t.Errorf("0.25 cores should be 250m, got %d", r.CPUm)
	}
	if r.MemoryMiB != 512 {
		t.Errorf("536870912 bytes should be 512MiB, got %d", r.MemoryMiB)
	}
}

func TestPrometheusProvider_RoundsUpNeverDown(t *testing.T) {
	// Under-reporting usage is what lets a simulation claim a node is
	// removable when it is not, so conversions round up.
	cpu := `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"namespace":"default","pod":"web-1"},"value":[1,"0.0001"]}
	]}}`
	mem := `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"namespace":"default","pod":"web-1"},"value":[1,"1048577"]}
	]}}`
	srv := promServer(t, cpu, mem)
	defer srv.Close()

	set, err := (&PrometheusProvider{BaseURL: srv.URL}).PodUsage(context.Background(), time.Hour, 0.95)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r, _ := set.Get("default", "web-1")
	if r.CPUm != 1 {
		t.Errorf("0.0001 cores must round up to 1m, got %d", r.CPUm)
	}
	if r.MemoryMiB != 2 {
		t.Errorf("1048577 bytes must round up to 2MiB, got %d", r.MemoryMiB)
	}
}

func TestPrometheusProvider_ErrorsOnRejectedQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"error","error":"parse error"}`))
	}))
	defer srv.Close()

	set, err := (&PrometheusProvider{BaseURL: srv.URL}).PodUsage(context.Background(), time.Hour, 0.95)
	if err == nil {
		t.Fatal("expected an error when Prometheus rejects the query")
	}
	if set.Fidelity != FidelityNone {
		t.Errorf("a failed query must yield no fidelity, got %s", set.Fidelity)
	}
}

func TestPrometheusProvider_ErrorsWhenNoURLConfigured(t *testing.T) {
	if _, err := (&PrometheusProvider{}).PodUsage(context.Background(), time.Hour, 0.95); err == nil {
		t.Fatal("expected an error with no base URL")
	}
}

func TestResolve_FallsBackToInstantInAutoMode(t *testing.T) {
	instant := &InstantProvider{Readings: map[string]Reading{"default/web-1": {CPUm: 100, MemoryMiB: 200}}}
	set := Resolve(context.Background(), Config{Mode: "auto"}, instant)
	if set.Fidelity != FidelityInstant {
		t.Fatalf("expected an instant fallback, got %s", set.Fidelity)
	}
	if set.Actionable() {
		t.Error("the instant fallback must not be actionable")
	}
}

func TestResolve_ExplicitPrometheusDoesNotSilentlyDowngrade(t *testing.T) {
	// The operator asked for percentile evidence. Substituting a single live
	// sample would look identical downstream while meaning something else
	// entirely, so the resolver returns nothing instead.
	instant := &InstantProvider{Readings: map[string]Reading{"default/web-1": {CPUm: 100}}}
	set := Resolve(context.Background(), Config{Mode: "prometheus"}, instant)
	if set.Fidelity != FidelityNone {
		t.Fatalf("expected no data, got %s", set.Fidelity)
	}
	if set.Err == "" {
		t.Error("expected the reason for the failure to be reported")
	}
}

func TestResolve_RejectsProviderWithThinCoverage(t *testing.T) {
	cpu := `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"namespace":"default","pod":"web-1"},"value":[1,"0.25"]}
	]}}`
	srv := promServer(t, cpu, cpu)
	defer srv.Close()

	instant := &InstantProvider{Readings: map[string]Reading{
		"default/web-1": {CPUm: 100}, "default/web-2": {CPUm: 100},
		"default/web-3": {CPUm: 100}, "default/web-4": {CPUm: 100},
	}}
	// Prometheus knows one pod of four — 25%, well under MinCoverage.
	set := Resolve(context.Background(), Config{
		Mode:          "auto",
		PrometheusURL: srv.URL,
		PodKeys:       []string{"default/web-1", "default/web-2", "default/web-3", "default/web-4"},
	}, instant)

	if set.Fidelity != FidelityInstant {
		t.Fatalf("thin Prometheus coverage should fall back to instant, got %s", set.Fidelity)
	}
	if set.Err == "" {
		t.Error("expected the coverage shortfall to be explained")
	}
}

func TestResolve_UsesPrometheusWhenCoverageIsGood(t *testing.T) {
	cpu := `{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"namespace":"default","pod":"web-1"},"value":[1,"0.25"]},
		{"metric":{"namespace":"default","pod":"web-2"},"value":[1,"0.5"]}
	]}}`
	srv := promServer(t, cpu, cpu)
	defer srv.Close()

	set := Resolve(context.Background(), Config{
		Mode:          "auto",
		PrometheusURL: srv.URL,
		PodKeys:       []string{"default/web-1", "default/web-2"},
	}, &InstantProvider{Readings: map[string]Reading{"default/web-1": {CPUm: 9999}}})

	if set.Fidelity != FidelityHistoricalP95 {
		t.Fatalf("expected Prometheus to win, got %s", set.Fidelity)
	}
	if r, _ := set.Get("default", "web-2"); r.CPUm != 500 {
		t.Errorf("expected the Prometheus reading, got %d", r.CPUm)
	}
}

func TestEffective_TakesTheMoreDemandingNumber(t *testing.T) {
	// Request larger than observed usage: the request wins, because the
	// percentile window may not have seen the real peak.
	cpu, mem := Effective(1000, 2048, Reading{CPUm: 200, MemoryMiB: 512}, FidelityHistoricalP95, 20)
	if cpu != 1000 || mem != 2048 {
		t.Errorf("expected the request to win, got %dm / %dMi", cpu, mem)
	}
	// Observed usage above the request: usage plus headroom wins, because
	// the request was simply set too low.
	cpu, mem = Effective(100, 128, Reading{CPUm: 1000, MemoryMiB: 2048}, FidelityHistoricalP95, 20)
	if cpu != 1200 {
		t.Errorf("expected 1000m + 20%% headroom = 1200m, got %d", cpu)
	}
	if mem != 2457 {
		t.Errorf("expected 2048Mi + 20%% headroom = 2457Mi, got %d", mem)
	}
}

func TestEffective_WeakEvidenceCannotShrinkAPod(t *testing.T) {
	// A single live sample showing near-zero usage must not be allowed to
	// reduce a pod's modelled footprint.
	cpu, mem := Effective(1000, 2048, Reading{CPUm: 5, MemoryMiB: 10}, FidelityInstant, 20)
	if cpu != 1000 || mem != 2048 {
		t.Errorf("instant data must leave the request untouched, got %dm / %dMi", cpu, mem)
	}
	cpu, mem = Effective(1000, 2048, Reading{}, FidelityNone, 20)
	if cpu != 1000 || mem != 2048 {
		t.Errorf("absent data must leave the request untouched, got %dm / %dMi", cpu, mem)
	}
}

// A misspelled provider name must be reported, not silently downgraded to the
// weakest source with no explanation for why enforcement stays locked.
func TestResolve_UnknownProviderIsReported(t *testing.T) {
	set := Resolve(context.Background(), Config{Mode: "promethus"},
		&InstantProvider{Readings: map[string]Reading{"ns/a": {CPUm: 10, MemoryMiB: 20}}})

	if set.Fidelity != FidelityInstant {
		t.Errorf("fidelity = %q, want %q", set.Fidelity, FidelityInstant)
	}
	if !strings.Contains(set.Err, "unknown usage provider") {
		t.Errorf("Err = %q, want it to name the unknown provider", set.Err)
	}
}

// An unknown mode must never be treated as an implicit request for Prometheus.
func TestResolve_UnknownProviderDoesNotQueryPrometheus(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	Resolve(context.Background(), Config{Mode: "p95", PrometheusURL: server.URL}, nil)
	if called {
		t.Error("an unknown provider name reached the Prometheus endpoint")
	}
}
