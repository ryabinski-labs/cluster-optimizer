package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PrometheusProvider reads true percentiles from a Prometheus-compatible HTTP
// API (Prometheus, Thanos, Mimir, VictoriaMetrics — all serve /api/v1/query).
//
// It speaks the HTTP API directly rather than pulling in a client library:
// two instant queries against a documented, stable endpoint is not enough
// surface to justify a dependency in a tool whose whole job is reducing waste.
type PrometheusProvider struct {
	BaseURL string
	Client  *http.Client
	// CPUQuery and MemoryQuery override the default expressions. The
	// placeholders %[1]s (quantile) and %[2]s (lookback) are substituted.
	// Clusters relabel cAdvisor metrics often enough that this needs to be
	// operator-adjustable.
	CPUQuery    string
	MemoryQuery string
}

// Default expressions assume stock kubelet/cAdvisor metric names.
//
// The subquery form `<instant vector>[<lookback>:<step>]` is what makes this a
// real percentile: it evaluates the inner expression at every step across the
// window and takes the 95th percentile of those evaluations, rather than
// reporting whatever the value happens to be right now.
const (
	defaultCPUQuery = `quantile_over_time(%[1]s, ` +
		`sum by (namespace, pod) (rate(container_cpu_usage_seconds_total{container!="",container!="POD",pod!=""}[5m]))` +
		`[%[2]s:5m])`
	defaultMemoryQuery = `quantile_over_time(%[1]s, ` +
		`sum by (namespace, pod) (container_memory_working_set_bytes{container!="",container!="POD",pod!=""})` +
		`[%[2]s:5m])`
)

func (p *PrometheusProvider) Name() string { return "prometheus" }

func (p *PrometheusProvider) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (p *PrometheusProvider) PodUsage(ctx context.Context, lookback time.Duration, quantile float64) (Set, error) {
	set := Set{
		Pods:     map[string]Reading{},
		Fidelity: FidelityHistoricalP95,
		Source:   p.Name(),
		Lookback: lookback,
		Quantile: quantile,
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		return Set{Fidelity: FidelityNone}, fmt.Errorf("prometheus base URL is not configured")
	}

	window := promDuration(lookback)
	q := strconv.FormatFloat(quantile, 'f', -1, 64)

	cpuExpr := p.CPUQuery
	if cpuExpr == "" {
		cpuExpr = defaultCPUQuery
	}
	memExpr := p.MemoryQuery
	if memExpr == "" {
		memExpr = defaultMemoryQuery
	}

	cpuSeries, err := p.query(ctx, fmt.Sprintf(cpuExpr, q, window))
	if err != nil {
		return Set{Fidelity: FidelityNone}, fmt.Errorf("cpu query: %w", err)
	}
	memSeries, err := p.query(ctx, fmt.Sprintf(memExpr, q, window))
	if err != nil {
		return Set{Fidelity: FidelityNone}, fmt.Errorf("memory query: %w", err)
	}

	// CPU comes back in cores; the model works in millicores. Round up:
	// under-reporting a pod's CPU is what would let the simulator claim a
	// node is removable when it is not.
	for key, cores := range cpuSeries {
		r := set.Pods[key]
		r.CPUm = int64(math.Ceil(cores * 1000))
		set.Pods[key] = r
	}
	// Memory comes back in bytes.
	for key, bytes := range memSeries {
		r := set.Pods[key]
		r.MemoryMiB = int64(math.Ceil(bytes / (1024 * 1024)))
		set.Pods[key] = r
	}

	if len(set.Pods) == 0 {
		return Set{Fidelity: FidelityNone}, fmt.Errorf("prometheus returned no pod series for the %s window", window)
	}
	return set, nil
}

// promResponse is the documented shape of /api/v1/query for a vector result.
type promResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			// Value is [unixTime, "stringified number"].
			Value []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// query runs one instant query and returns namespace/pod → value.
func (p *PrometheusProvider) query(ctx context.Context, expr string) (map[string]float64, error) {
	endpoint := strings.TrimSuffix(p.BaseURL, "/") + "/api/v1/query"
	form := url.Values{"query": []string{expr}}

	// POST rather than GET: subquery expressions across a long lookback
	// exceed what some proxies allow in a URL.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var parsed promResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode response (HTTP %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || parsed.Status != "success" {
		reason := parsed.Error
		if reason == "" {
			reason = resp.Status
		}
		return nil, fmt.Errorf("prometheus rejected the query: %s", reason)
	}
	if parsed.Data.ResultType != "vector" {
		return nil, fmt.Errorf("expected a vector result, got %q", parsed.Data.ResultType)
	}

	out := make(map[string]float64, len(parsed.Data.Result))
	for _, series := range parsed.Data.Result {
		namespace := series.Metric["namespace"]
		pod := series.Metric["pod"]
		if namespace == "" || pod == "" {
			continue
		}
		if len(series.Value) != 2 {
			continue
		}
		var raw string
		if err := json.Unmarshal(series.Value[1], &raw); err != nil {
			continue
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		out[Key(namespace, pod)] = value
	}
	return out, nil
}

// promDuration renders a Go duration in Prometheus's own syntax. Go prints
// "168h0m0s" for a week, which Prometheus rejects.
func promDuration(d time.Duration) string {
	if d <= 0 {
		return "7d"
	}
	if d%(24*time.Hour) == 0 {
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	}
	if d%time.Hour == 0 {
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	}
	return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
}
