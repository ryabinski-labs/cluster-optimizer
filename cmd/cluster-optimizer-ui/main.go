package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GipsyChef/cluster-optimizer/internal/analyzer"
	"github.com/GipsyChef/cluster-optimizer/internal/store"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

//go:embed static
var staticFiles embed.FS

type reportRecord struct {
	ClusterID   string             `json:"cluster_id"`
	GeneratedAt time.Time          `json:"generated_at"`
	Summary     map[string]any     `json:"summary"`
	Findings    []analyzer.Finding `json:"findings"`
}

type apiResponse struct {
	ClusterID    string         `json:"cluster_id"`
	Table        string         `json:"table"`
	Region       string         `json:"region"`
	Reports      []reportRecord `json:"reports"`
	Trend        trendResponse  `json:"trend"`
	EngineStatus *engineStatus  `json:"engine_status,omitempty"`
}

// engineStatus mirrors store.EngineStatus for the UI. We keep a thin local
// type so the JSON wire format is owned here and stays stable even if the
// store struct adds fields.
type engineStatus struct {
	AutoApplyEnabled bool      `json:"auto_apply_enabled"`
	AutoApplyLive    bool      `json:"auto_apply_live"`
	NudgeEnabled     bool      `json:"nudge_enabled"`
	NudgeLive        bool      `json:"nudge_live"`
	HaltActive       bool      `json:"halt_active"`
	HaltReason       string    `json:"halt_reason,omitempty"`
	LastRunAt        time.Time `json:"last_run_at"`
	LastRunActions   int       `json:"last_run_actions"`
	LastRunApplied   int       `json:"last_run_applied"`
	LastRunErrors    int       `json:"last_run_errors"`
}

type remediationEvent struct {
	Timestamp       time.Time `json:"timestamp"`
	Mode            string    `json:"mode"`
	Kind            string    `json:"kind"`
	Namespace       string    `json:"namespace,omitempty"`
	Workload        string    `json:"workload,omitempty"`
	WorkloadKind    string    `json:"workload_kind,omitempty"`
	Container       string    `json:"container,omitempty"`
	RuleID          string    `json:"rule_id,omitempty"`
	BeforeCPUm      int64     `json:"before_cpu_m,omitempty"`
	AfterCPUm       int64     `json:"after_cpu_m,omitempty"`
	BeforeMemMiB    int64     `json:"before_memory_mib,omitempty"`
	AfterMemMiB     int64     `json:"after_memory_mib,omitempty"`
	Applied         bool      `json:"applied"`
	Reason          string    `json:"reason,omitempty"`
	Error           string    `json:"error,omitempty"`
	HaltActive      bool      `json:"halt_active,omitempty"`
	TargetNode      string    `json:"target_node,omitempty"`
	Evicted         int       `json:"evicted,omitempty"`
	EvictionErrors  int       `json:"eviction_errors,omitempty"`
	OccurrenceCount int64     `json:"occurrence_count,omitempty"`
}

type remediationHistoryResponse struct {
	ClusterID string             `json:"cluster_id"`
	Events    []remediationEvent `json:"events"`
}

type trendResponse struct {
	Window             trendWindow            `json:"window"`
	Series             []trendPoint           `json:"series"`
	SeverityCounts     map[string]int         `json:"severity_counts"`
	TopRecommendations []recommendationRollup `json:"top_recommendations"`
}

type trendWindow struct {
	FirstReportAt  *time.Time `json:"first_report_at,omitempty"`
	LatestReportAt *time.Time `json:"latest_report_at,omitempty"`
	ReportCount    int        `json:"report_count"`
	ObservedDays   int        `json:"observed_days"`
	RequiredDays   int        `json:"required_days"`
}

type trendPoint struct {
	GeneratedAt        time.Time `json:"generated_at"`
	Findings           int       `json:"findings"`
	High               int       `json:"high"`
	Medium             int       `json:"medium"`
	Low                int       `json:"low"`
	NodeCount          int64     `json:"node_count"`
	RequestedMemoryMiB int64     `json:"requested_memory_mib"`
	ObservedMemoryMiB  *int64    `json:"observed_memory_mib,omitempty"`
	TwoNodeFeasible    *bool     `json:"two_node_feasible,omitempty"`
}

type recommendationRollup struct {
	Key             string             `json:"key"`
	RuleID          string             `json:"rule_id"`
	Severity        string             `json:"severity"`
	Namespace       string             `json:"namespace,omitempty"`
	Workload        string             `json:"workload,omitempty"`
	Scope           string             `json:"scope"`
	Occurrences     int                `json:"occurrences"`
	ObservedDays    int                `json:"observed_days"`
	FirstSeenAt     time.Time          `json:"first_seen_at"`
	LastSeenAt      time.Time          `json:"last_seen_at"`
	Latest          analyzer.Finding   `json:"latest"`
	LatestReportHas bool               `json:"latest_report_has"`
	Remediation     remediationSummary `json:"remediation"`
}

type remediationSummary struct {
	Supported        bool              `json:"supported"`
	Available        bool              `json:"available"`
	Action           string            `json:"action,omitempty"`
	ButtonLabel      string            `json:"button_label,omitempty"`
	Reason           string            `json:"reason"`
	TargetRepo       string            `json:"target_repo,omitempty"`
	ManifestPath     string            `json:"manifest_path,omitempty"`
	InstructionsPath string            `json:"instructions_path,omitempty"`
	Container        string            `json:"container,omitempty"`
	TargetCPU        string            `json:"target_cpu,omitempty"`
	TargetMemory     string            `json:"target_memory,omitempty"`
	Workflow         string            `json:"workflow,omitempty"`
	WorkflowInputs   map[string]string `json:"workflow_inputs,omitempty"`
}

type remediationTarget struct {
	ClusterID        string   `json:"cluster_id"`
	Namespace        string   `json:"namespace"`
	Workload         string   `json:"workload"`
	Repository       string   `json:"repository"`
	ManifestPath     string   `json:"manifest_path"`
	InstructionsPath string   `json:"instructions_path"`
	Container        string   `json:"container"`
	SupportedRules   []string `json:"supported_rules"`
}

type remediationTargetsFile struct {
	Targets []remediationTarget `json:"targets"`
}

type remediationRequest struct {
	ClusterID string `json:"cluster_id"`
	RuleID    string `json:"rule_id"`
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Confirm   bool   `json:"confirm"`
}

type remediationResponse struct {
	Status      string             `json:"status"`
	WorkflowURL string             `json:"workflow_url,omitempty"`
	Remediation remediationSummary `json:"remediation"`
}

type server struct {
	table              string
	region             string
	client             *dynamodb.Client
	static             http.Handler
	remediationTargets map[string]remediationTarget
	minRemediationDays int
	githubRepository   string
	githubWorkflow     string
	rewriteWorkflow    string
	githubRef          string

	// trendReportCap bounds how many recent REPORT# items we pull from
	// DynamoDB to build trend/rollup data. Each report carries a full
	// embedded JSON payload, so a smaller cap directly shrinks the response
	// body the SDK has to deserialize within the request timeout.
	trendReportCap int32

	// cacheTTL controls how long the per-cluster reports + rollups response
	// is reused across UI polls. Set to 0 to disable the cache.
	cacheTTL time.Duration

	cacheMu sync.Mutex
	cache   map[string]*reportsCacheEntry
}

type reportsCacheEntry struct {
	expiresAt time.Time
	reports   []reportRecord
	rollups   map[string]recommendationRollup
}

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		log.Fatalf("cluster-optimizer-ui: %v", err)
	}
}

func run(ctx context.Context, args []string) error {
	var addr string
	var table string
	var region string
	var remediationTargetsPath string
	var githubRepository string
	var githubWorkflow string
	var rewriteWorkflow string
	var githubRef string
	var minRemediationDays int
	var trendReportCap int
	var cacheTTLSeconds int
	flags := flag.NewFlagSet("cluster-optimizer-ui", flag.ContinueOnError)
	flags.StringVar(&addr, "addr", envOr("CLUSTER_OPTIMIZER_UI_ADDR", "127.0.0.1:8088"), "listen address")
	flags.StringVar(&table, "table", envOr("DYNAMODB_TABLE", "cluster-optimizer-reports"), "DynamoDB table")
	flags.StringVar(&region, "region", envOr("AWS_REGION", "us-east-1"), "AWS region")
	flags.StringVar(&remediationTargetsPath, "remediation-targets", envOr("REMEDIATION_TARGETS", "config/remediation-targets.json"), "JSON file mapping workloads to api.yml repositories")
	flags.IntVar(&minRemediationDays, "min-remediation-days", envIntOr("MIN_REMEDIATION_DAYS", 3), "minimum observed days before remediation can be dispatched")
	flags.StringVar(&githubRepository, "github-repository", envOr("GITHUB_REPOSITORY", "GipsyChef/cluster-optimizer"), "repository that owns the remediation workflow")
	flags.StringVar(&githubWorkflow, "github-workflow", envOr("REMEDIATION_WORKFLOW", "remediate-api-yml.yml"), "GitHub Actions workflow file to dispatch")
	flags.StringVar(&rewriteWorkflow, "rewrite-workflow", envOr("REWRITE_PLAN_WORKFLOW", "generate-rewrite-instructions.yml"), "GitHub Actions workflow file that creates coding-agent rewrite instructions")
	flags.StringVar(&githubRef, "github-ref", envOr("REMEDIATION_WORKFLOW_REF", "main"), "Git ref for workflow_dispatch")
	flags.IntVar(&trendReportCap, "trend-report-cap", envIntOr("TREND_REPORT_CAP", 50), "max recent reports fetched from DynamoDB to build trend/rollups")
	flags.IntVar(&cacheTTLSeconds, "cache-ttl-seconds", envIntOr("REPORTS_CACHE_TTL_SECONDS", 30), "in-memory TTL for reports+rollups responses (0 disables the cache)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	client, err := store.NewDynamoDBClient(ctx, config.WithRegion(region))
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	staticRoot, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return err
	}
	targets, err := loadRemediationTargets(remediationTargetsPath)
	if err != nil {
		log.Printf("remediation targets disabled: %v", err)
	}
	if trendReportCap < 1 {
		trendReportCap = 1
	}
	if trendReportCap > 200 {
		trendReportCap = 200
	}
	srv := &server{
		table:              table,
		region:             region,
		client:             client,
		static:             http.FileServer(http.FS(staticRoot)),
		remediationTargets: targets,
		minRemediationDays: minRemediationDays,
		githubRepository:   githubRepository,
		githubWorkflow:     githubWorkflow,
		rewriteWorkflow:    rewriteWorkflow,
		githubRef:          githubRef,
		trendReportCap:     int32(trendReportCap),
		cacheTTL:           time.Duration(cacheTTLSeconds) * time.Second,
		cache:              map[string]*reportsCacheEntry{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/reports", srv.handleReports)
	mux.HandleFunc("/api/remediations", srv.handleRemediation)
	mux.HandleFunc("/api/remediations/download", srv.handleDownload)
	mux.HandleFunc("/api/remediations/history", srv.handleRemediationHistory)
	mux.HandleFunc("/api/health", srv.handleHealth)
	mux.Handle("/", srv.static)

	log.Printf("cluster optimizer UI listening on http://%s", addr)
	log.Printf("reading DynamoDB table %s in %s with the default AWS credential chain", table, region)
	return http.ListenAndServe(addr, mux)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "table": s.table, "region": s.region})
}

func (s *server) handleReports(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	clusterID := r.URL.Query().Get("cluster_id")
	if clusterID == "" {
		clusterID = "default"
	}
	limit := int32(25)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || value < 1 || value > 100 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return
		}
		limit = int32(value)
	}

	allReports, rollups, err := s.reportsAndRollups(ctx, clusterID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	reports := allReports
	if int(limit) < len(allReports) {
		reports = allReports[:limit]
	}

	// Engine status is best-effort — a missing sentinel just means this
	// cluster has never run the remediation engine, and the UI should
	// render the strip in its "no data yet" state. Don't fail the whole
	// response for it.
	var status *engineStatus
	if loaded, loadErr := store.LoadEngineStatus(ctx, s.client, s.table, clusterID); loadErr != nil {
		log.Printf("engine status load failed for %s: %v", clusterID, loadErr)
	} else if loaded != nil {
		status = &engineStatus{
			AutoApplyEnabled: loaded.AutoApplyEnabled,
			AutoApplyLive:    loaded.AutoApplyLive,
			NudgeEnabled:     loaded.NudgeEnabled,
			NudgeLive:        loaded.NudgeLive,
			HaltActive:       loaded.HaltActive,
			HaltReason:       loaded.HaltReason,
			LastRunAt:        loaded.LastRunAt,
			LastRunActions:   loaded.LastRunActions,
			LastRunApplied:   loaded.LastRunApplied,
			LastRunErrors:    loaded.LastRunErrors,
		}
	}

	writeJSON(w, http.StatusOK, apiResponse{
		ClusterID:    clusterID,
		Table:        s.table,
		Region:       s.region,
		Reports:      reports,
		Trend:        s.buildTrend(allReports, rollups),
		EngineStatus: status,
	})
}

// handleRemediationHistory returns the recent remediation audit log for a
// cluster. Defaults: 7-day window, 50-event cap; UI may raise either via
// `since` (RFC3339) and `limit` (1-200) query params.
func (s *server) handleRemediationHistory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	clusterID := r.URL.Query().Get("cluster_id")
	if clusterID == "" {
		clusterID = "default"
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 200 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 200")
			return
		}
		limit = value
	}
	since := time.Now().Add(-7 * 24 * time.Hour)
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be RFC3339")
			return
		}
		since = parsed
	}

	loaded, err := store.LoadRemediations(ctx, s.client, s.table, clusterID, since, limit)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	events := make([]remediationEvent, 0, len(loaded))
	for _, event := range loaded {
		events = append(events, remediationEvent{
			Timestamp:       event.Timestamp,
			Mode:            event.Mode,
			Kind:            event.Kind,
			Namespace:       event.Namespace,
			Workload:        event.Workload,
			WorkloadKind:    event.WorkloadKind,
			Container:       event.Container,
			RuleID:          event.RuleID,
			BeforeCPUm:      event.BeforeCPUm,
			AfterCPUm:       event.AfterCPUm,
			BeforeMemMiB:    event.BeforeMemMiB,
			AfterMemMiB:     event.AfterMemMiB,
			Applied:         event.Applied,
			Reason:          event.Reason,
			Error:           event.Error,
			HaltActive:      event.HaltActive,
			TargetNode:      event.TargetNode,
			Evicted:         event.Evicted,
			EvictionErrors:  event.EvictionErrors,
			OccurrenceCount: event.OccurrenceCount,
		})
	}
	writeJSON(w, http.StatusOK, remediationHistoryResponse{
		ClusterID: clusterID,
		Events:    events,
	})
}

func (s *server) handleRemediation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req remediationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !req.Confirm {
		writeError(w, http.StatusBadRequest, "confirm must be true")
		return
	}
	if req.ClusterID == "" {
		req.ClusterID = "default"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	_, rollups, err := s.reportsAndRollups(ctx, req.ClusterID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	rollup, ok := rollups[findingKey(req.RuleID, req.Namespace, req.Workload)]
	if !ok {
		writeError(w, http.StatusNotFound, "recommendation was not found in recent reports")
		return
	}
	if !rollup.Remediation.Available {
		writeJSON(w, http.StatusConflict, remediationResponse{Status: "blocked", Remediation: rollup.Remediation})
		return
	}

	token, err := githubToken(ctx)
	if err != nil {
		writeError(w, http.StatusPreconditionFailed, err.Error())
		return
	}
	workflow := rollup.Remediation.Workflow
	if workflow == "" {
		workflow = s.githubWorkflow
	}
	if err := s.dispatchWorkflow(ctx, token, workflow, rollup.Remediation.WorkflowInputs); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	workflowURL := fmt.Sprintf("https://github.com/%s/actions/workflows/%s", s.githubRepository, workflow)
	writeJSON(w, http.StatusAccepted, remediationResponse{Status: "dispatched", WorkflowURL: workflowURL, Remediation: rollup.Remediation})
}

func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	clusterID := r.URL.Query().Get("cluster_id")
	if clusterID == "" {
		clusterID = "default"
	}
	ruleID := r.URL.Query().Get("rule_id")
	namespace := r.URL.Query().Get("namespace")
	workload := r.URL.Query().Get("workload")

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	_, rollups, err := s.reportsAndRollups(ctx, clusterID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	rollup, ok := rollups[findingKey(ruleID, namespace, workload)]
	if !ok {
		writeError(w, http.StatusNotFound, "recommendation not found")
		return
	}

	title := strings.ReplaceAll(rollup.Workload, "/", " ")
	var b strings.Builder

	if rollup.RuleID == "runtime-modernization-candidate" {
		fmt.Fprintf(&b, "# Runtime Modernization Instructions: %s\n\n", title)
		fmt.Fprintf(&b, "Generated by Cluster Optimizer on %s.\n\n", time.Now().UTC().Format(time.RFC3339))

		b.WriteString("## Mission\n\n")
		b.WriteString("Implement a runtime modernization for this service only if measurement proves the current implementation cannot meet the target resource envelope with smaller, reversible changes. Prefer a focused Go implementation for memory-heavy always-on paths when it can preserve product behavior, reduce steady-state memory, and simplify operations. Do not rewrite broad product surface area without profiling evidence and an explicit migration plan.\n\n")

		b.WriteString("## Observed Recommendation\n\n")
		fmt.Fprintf(&b, "- Cluster: %s\n", clusterID)
		fmt.Fprintf(&b, "- Namespace: %s\n", rollup.Namespace)
		fmt.Fprintf(&b, "- Workload: %s\n", rollup.Workload)
		fmt.Fprintf(&b, "- Rule: %s\n", rollup.RuleID)
		fmt.Fprintf(&b, "- Severity: %s\n", rollup.Severity)
		fmt.Fprintf(&b, "- Evidence: %s\n", rollup.Latest.Evidence)
		fmt.Fprintf(&b, "- Recommendation: %s\n", rollup.Latest.Recommendation)
		fmt.Fprintf(&b, "- Expected cost effect: %s\n", rollup.Latest.ExpectedCostEffect)
		fmt.Fprintf(&b, "- Risk: %s\n", rollup.Latest.Risk)
		fmt.Fprintf(&b, "- Confidence: %s\n", rollup.Latest.Confidence)
		fmt.Fprintf(&b, "- Observed days: %d\n", rollup.ObservedDays)
		fmt.Fprintf(&b, "- Report occurrences: %d\n", rollup.Occurrences)
		fmt.Fprintf(&b, "- First seen: %s\n", rollup.FirstSeenAt.Format(time.RFC3339))
		fmt.Fprintf(&b, "- Last seen: %s\n", rollup.LastSeenAt.Format(time.RFC3339))

		b.WriteString("\n## Required Discovery\n\n")
		b.WriteString("- Read the existing repository before editing code. Identify the service entrypoint, public API, health endpoints, background workers, persistence layer, queues, third-party clients, auth model, config/env vars, and deployment manifests.\n")
		b.WriteString("- Establish the user-critical flows this service owns. Preserve request/response contracts, status codes, headers, error shapes, idempotency behavior, auth behavior, data semantics, and backwards-compatible environment variables.\n")
		b.WriteString("- Collect a baseline before changing implementation: container image size, startup time, idle RSS, peak RSS, CPU under representative load, p50/p95/p99 latency, throughput, error rate, pod restarts, OOMKilled events, and current Kubernetes requests/limits.\n")
		b.WriteString("- Profile the current implementation. Capture CPU and memory profiles, allocation hotspots, dependency latency, serialization overhead, database/query behavior, queue depth, and any runtime GC or event-loop/thread-pool pressure.\n")
		b.WriteString("- Document every assumption that cannot be verified locally and add a follow-up task for production or staging evidence.\n\n")

		b.WriteString("## Architecture Direction\n\n")
		b.WriteString("- First evaluate non-rewrite options: request tuning, lazy loading, dependency reduction, concurrency limits, caching with clear invalidation, worker separation, scale-to-zero, and moving only the hot path to a smaller service.\n")
		b.WriteString("- If a rewrite is justified, use an evolutionary migration. Keep the existing service as the rollback path and introduce the new Go service behind a feature flag, route split, queue consumer split, or shadow-read/shadow-write pattern where applicable.\n")
		b.WriteString("- Keep service boundaries boring and explicit. The new implementation should own the same contract unless the PR includes a reviewed API migration plan.\n")
		b.WriteString("- Apply AWS Well-Architected and Kubernetes production concerns: operational ownership, least privilege, secret handling, reliability during deploys, performance efficiency, cost controls, and sustainability through lower steady-state compute.\n")
		b.WriteString("- Do not add a new database, cache, queue, cloud service, or protocol unless the architecture section of the PR explains why existing components cannot satisfy the workload.\n\n")

		b.WriteString("## Implementation Requirements\n\n")
		b.WriteString("- Prefer Go for the first implementation unless repository evidence shows Rust is the better fit for an existing ecosystem or a CPU-bound native library path.\n")
		b.WriteString("- Keep memory use explicit: bounded buffers, request body limits, streaming where useful, connection pool limits, timeouts, cancellation propagation, and no unbounded goroutine or channel growth.\n")
		b.WriteString("- Preserve observability: structured logs, request IDs/correlation IDs, metrics for latency/error/saturation, health/readiness endpoints, dependency timing, and clear startup/shutdown logs.\n")
		b.WriteString("- Preserve security controls: authentication, authorization, input validation, output encoding, CORS/security headers if this service owns HTTP responses, secret redaction, dependency scanning, and no secret values in logs/tests/fixtures.\n")
		b.WriteString("- Preserve reliability controls: graceful shutdown, readiness that fails until dependencies are usable, liveness only for deadlock/crash detection, retry limits, backoff, idempotency for mutating operations, and explicit timeout budgets.\n")
		b.WriteString("- Keep deployment compatible with the current CI/CD path. Update Dockerfile, build workflow, Kubernetes manifest, api.yml, Helm values, or Kustomize overlays only where the service owner already maintains them.\n")
		b.WriteString("- Do not lower Kubernetes requests as part of the first rewrite PR unless the PR includes before/after load data. Resource reductions should follow validation, not precede it.\n\n")

		b.WriteString("## Performance Acceptance Criteria\n\n")
		b.WriteString("- The PR must include a baseline table and an after table using the same test method, data volume, concurrency, container limits, and environment.\n")
		b.WriteString("- Memory target: reduce steady-state and representative-load memory enough to create real bin-packing value. If the recommendation came from high observed memory, the new service must also prove it does not under-request memory.\n")
		b.WriteString("- Latency target: p50 and p95 latency must stay within current SLOs or improve. Do not trade lower memory for materially worse tail latency.\n")
		b.WriteString("- Throughput target: match or exceed current sustainable throughput at the same or lower CPU request, unless the product owner explicitly accepts lower throughput.\n")
		b.WriteString("- Startup and rollout target: startup time, readiness time, graceful shutdown, and rollout behavior must be measured and documented.\n")
		b.WriteString("- Cost target: explain how the measured resource reduction changes node bin-packing, HPA behavior, or scale-to-zero potential. Do not claim node savings without scheduler/request evidence.\n\n")

		b.WriteString("## Deep QA And Validation Plan\n\n")
		b.WriteString("- Unit tests: cover parsing, validation, business rules, error handling, retries/timeouts, config loading, and boundary cases.\n")
		b.WriteString("- Contract tests: compare old and new behavior for every public endpoint, queue message, webhook, CLI command, or background job contract. Include success, validation failure, auth failure, dependency failure, and empty-state cases.\n")
		b.WriteString("- Integration tests: run against the same backing stores or realistic local substitutes. Verify migrations, schema compatibility, transactional behavior, and integration bounds.\n")
		b.WriteString("- End-to-end tests: exercise user-critical flows through the service boundary. Capture logs and evidence for successful and failed paths.\n")
		b.WriteString("- Load tests: define traffic shape, ramp, duration, concurrency, test data, stop conditions, monitoring, and expected p50/p95/p99 latency, throughput, errors, CPU, memory, GC, and dependency saturation.\n")
		b.WriteString("- Security tests: safe input-validation probes, auth/authorization checks, dependency audit, secret scan, container image scan, CI permission review, and verification that logs do not expose credentials or user-sensitive data.\n")
		b.WriteString("- Kubernetes tests: deploy to staging, verify readiness/liveness, resource usage, HPA math if present, PDB behavior, rolling update, rollback, pod restarts, OOMKilled events, CPU throttling, and node placement.\n")
		b.WriteString("- Canary: start with shadow traffic or a small routed percentage. Monitor latency, errors, saturation, memory, CPU, restarts, and business metrics for at least one peak period before full rollout.\n")
		b.WriteString("- Rollback: keep the old service deployable, document the exact rollback command or PR revert path, and verify rollback in staging before production rollout.\n\n")

		b.WriteString("## Pull Request Requirements\n\n")
		b.WriteString("- Include an architecture summary with alternatives considered and why the chosen approach wins.\n")
		b.WriteString("- Include the baseline and after measurements, method, sample size, environment, and confidence level.\n")
		b.WriteString("- Include a risk register covering correctness, reliability, security, performance, cost, operations, and rollback.\n")
		b.WriteString("- Include release steps, feature flags or traffic controls, validation dashboards/queries, alert expectations, and rollback steps.\n")
		b.WriteString("- Link this instructions file and the originating Cluster Optimizer finding in the PR body.\n\n")
	} else if rollup.RuleID == "cpu-hpa-low-request-sensitive" {
		// The 60s and 300s windows below mirror the safe minimums applied by
		// cmd/api-yml-remediator (hpaScaleUpStabilizationSeconds and
		// hpaScaleDownStabilizationSeconds). Keep them in sync if those change.
		fmt.Fprintf(&b, "# Remediation Instructions: %s\n\n", title)
		fmt.Fprintf(&b, "Generated by Cluster Optimizer on %s.\n\n", time.Now().UTC().Format(time.RFC3339))

		b.WriteString("## Mission\n\n")
		fmt.Fprintf(&b, "Stabilize the HorizontalPodAutoscaler for this workload by adding or raising its scale-up and scale-down stabilization windows in `%s`. This dampens replica churn so CPU request tuning does not make the autoscaler flap or trigger avoidable node scale-ups. Stabilization is the safe first step: do not lower the CPU request or change the CPU target in this patch.\n\n", rollup.Remediation.ManifestPath)

		b.WriteString("## Observed Recommendation\n\n")
		fmt.Fprintf(&b, "- Cluster: %s\n", clusterID)
		fmt.Fprintf(&b, "- Namespace: %s\n", rollup.Namespace)
		fmt.Fprintf(&b, "- Workload: %s\n", rollup.Workload)
		fmt.Fprintf(&b, "- Rule: %s\n", rollup.RuleID)
		fmt.Fprintf(&b, "- Severity: %s\n", rollup.Severity)
		fmt.Fprintf(&b, "- Evidence: %s\n", rollup.Latest.Evidence)
		fmt.Fprintf(&b, "- Recommendation: %s\n", rollup.Latest.Recommendation)
		fmt.Fprintf(&b, "- Expected cost effect: %s\n", rollup.Latest.ExpectedCostEffect)
		fmt.Fprintf(&b, "- Risk: %s\n", rollup.Latest.Risk)
		fmt.Fprintf(&b, "- Confidence: %s\n", rollup.Latest.Confidence)
		fmt.Fprintf(&b, "- Observed days: %d\n", rollup.ObservedDays)
		fmt.Fprintf(&b, "- Report occurrences: %d\n", rollup.Occurrences)
		fmt.Fprintf(&b, "- First seen: %s\n", rollup.FirstSeenAt.Format(time.RFC3339))
		fmt.Fprintf(&b, "- Last seen: %s\n", rollup.LastSeenAt.Format(time.RFC3339))

		b.WriteString("\n## Remediation Target\n\n")
		fmt.Fprintf(&b, "- Repository: %s\n", rollup.Remediation.TargetRepo)
		fmt.Fprintf(&b, "- Manifest Path: %s\n", rollup.Remediation.ManifestPath)
		fmt.Fprintf(&b, "- Autoscaler target: %s\n", rollup.Workload)
		b.WriteString("- Scale-up stabilization window: at least 60 seconds\n")
		b.WriteString("- Scale-down stabilization window: at least 300 seconds\n")

		b.WriteString("\n## Action Steps for Coding Agent / Developer\n\n")
		fmt.Fprintf(&b, "1. Clone or pull the target repository: `git clone git@github.com:%s.git`\n", rollup.Remediation.TargetRepo)
		fmt.Fprintf(&b, "2. Locate the manifest file at: `%s`\n", rollup.Remediation.ManifestPath)
		fmt.Fprintf(&b, "3. Find the `HorizontalPodAutoscaler` whose `spec.scaleTargetRef` points at `%s`.\n", rollup.Workload)
		b.WriteString("4. Confirm the autoscaler uses `apiVersion: autoscaling/v2`; the `spec.behavior` field is required for stabilization windows.\n")
		b.WriteString("5. Under `spec.behavior.scaleUp`, set `stabilizationWindowSeconds` to at least `60`. Only raise it; never lower an existing larger value.\n")
		b.WriteString("6. Under `spec.behavior.scaleDown`, set `stabilizationWindowSeconds` to at least `300`. Only raise it; never lower an existing larger value.\n")
		b.WriteString("7. Save the file.\n")
		fmt.Fprintf(&b, "8. Commit and push the changes on a new branch `remediate/%s`.\n", strings.ToLower(strings.ReplaceAll(rollup.Workload, "/", "-")))
		b.WriteString("9. Open a Pull Request against the main branch.\n\n")

		b.WriteString("## Verification and Safety Guidelines\n\n")
		b.WriteString("- Verify the manifest still parses as valid YAML and the HPA schema is correct.\n")
		b.WriteString("- Do not lower CPU requests or change the CPU utilization target in this patch; stabilization must land and be validated first.\n")
		b.WriteString("- After rollout, watch the HPA replica history for reduced churn, and validate latency and error-rate SLOs plus burst handling on a staging or test cluster before full production rollout.\n")
		b.WriteString("- Roll back by restoring the previous stabilization window values if burst handling regresses.\n")
	} else {
		fmt.Fprintf(&b, "# Remediation Instructions: %s\n\n", title)
		fmt.Fprintf(&b, "Generated by Cluster Optimizer on %s.\n\n", time.Now().UTC().Format(time.RFC3339))

		b.WriteString("## Mission\n\n")
		b.WriteString("Apply the recommended resource request tuning or configuration patch for this workload in its deployment manifests. This improves resource efficiency, allows safe bin-packing, and optimizes cluster cost without trading away operational stability.\n\n")

		b.WriteString("## Observed Recommendation\n\n")
		fmt.Fprintf(&b, "- Cluster: %s\n", clusterID)
		fmt.Fprintf(&b, "- Namespace: %s\n", rollup.Namespace)
		fmt.Fprintf(&b, "- Workload: %s\n", rollup.Workload)
		fmt.Fprintf(&b, "- Rule: %s\n", rollup.RuleID)
		fmt.Fprintf(&b, "- Severity: %s\n", rollup.Severity)
		fmt.Fprintf(&b, "- Evidence: %s\n", rollup.Latest.Evidence)
		fmt.Fprintf(&b, "- Recommendation: %s\n", rollup.Latest.Recommendation)
		fmt.Fprintf(&b, "- Expected cost effect: %s\n", rollup.Latest.ExpectedCostEffect)
		fmt.Fprintf(&b, "- Risk: %s\n", rollup.Latest.Risk)
		fmt.Fprintf(&b, "- Confidence: %s\n", rollup.Latest.Confidence)
		fmt.Fprintf(&b, "- Observed days: %d\n", rollup.ObservedDays)
		fmt.Fprintf(&b, "- Report occurrences: %d\n", rollup.Occurrences)
		fmt.Fprintf(&b, "- First seen: %s\n", rollup.FirstSeenAt.Format(time.RFC3339))
		fmt.Fprintf(&b, "- Last seen: %s\n", rollup.LastSeenAt.Format(time.RFC3339))

		b.WriteString("\n## Remediation Target\n\n")
		fmt.Fprintf(&b, "- Repository: %s\n", rollup.Remediation.TargetRepo)
		fmt.Fprintf(&b, "- Manifest Path: %s\n", rollup.Remediation.ManifestPath)
		fmt.Fprintf(&b, "- Container: %s\n", rollup.Remediation.Container)
		fmt.Fprintf(&b, "- Target CPU: %s\n", rollup.Remediation.TargetCPU)
		fmt.Fprintf(&b, "- Target Memory: %s\n", rollup.Remediation.TargetMemory)

		b.WriteString("\n## Action Steps for Coding Agent / Developer\n\n")
		fmt.Fprintf(&b, "1. Clone or pull the target repository: `git clone git@github.com:%s.git`\n", rollup.Remediation.TargetRepo)
		fmt.Fprintf(&b, "2. Locate the manifest file at: `%s`\n", rollup.Remediation.ManifestPath)
		fmt.Fprintf(&b, "3. Locate the container config for `%s` inside the workload.\n", rollup.Remediation.Container)
		b.WriteString("4. Update the resource requests:\n")
		if rollup.Remediation.TargetCPU != "" {
			fmt.Fprintf(&b, "   - CPU request: Set to `%s`\n", rollup.Remediation.TargetCPU)
		}
		if rollup.Remediation.TargetMemory != "" {
			fmt.Fprintf(&b, "   - Memory request: Set to `%s`\n", rollup.Remediation.TargetMemory)
		}
		b.WriteString("5. Save the file.\n")
		fmt.Fprintf(&b, "6. Commit and push the changes on a new branch `remediate/%s`.\n", strings.ToLower(strings.ReplaceAll(rollup.Workload, "/", "-")))
		b.WriteString("7. Open a Pull Request against the main branch.\n\n")

		b.WriteString("## Verification and Safety Guidelines\n\n")
		b.WriteString("- Verify the manifest parses and compiles correctly.\n")
		b.WriteString("- Ensure that resource request limits/requests are set logically (requests <= limits).\n")
		b.WriteString("- Verify the rollout behaves correctly on a staging or test cluster before full production rollout.\n")
	}

	filename := fmt.Sprintf("modernization-instructions-%s.md", strings.ToLower(strings.ReplaceAll(rollup.Workload, "/", "-")))
	w.Header().Set("Content-Type", "text/markdown")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(b.String()))
}

// reportsAndRollups returns the latest reports + computed rollups for a
// cluster, reusing a short-lived in-memory cache when configured. The cache
// absorbs the cost of repeated dashboard polls — each invocation would
// otherwise issue at least two DynamoDB Queries (REPORT#... and REC#...)
// each carrying multi-KB JSON payloads.
func (s *server) reportsAndRollups(ctx context.Context, clusterID string) ([]reportRecord, map[string]recommendationRollup, error) {
	if s.cacheTTL > 0 {
		s.cacheMu.Lock()
		if entry, ok := s.cache[clusterID]; ok && time.Now().Before(entry.expiresAt) {
			reports, rollups := entry.reports, entry.rollups
			s.cacheMu.Unlock()
			return reports, rollups, nil
		}
		s.cacheMu.Unlock()
	}

	reports, err := s.queryReports(ctx, clusterID, s.trendReportCap)
	if err != nil {
		return nil, nil, err
	}
	rollups := s.rollups(ctx, reports)

	if s.cacheTTL > 0 {
		s.cacheMu.Lock()
		s.cache[clusterID] = &reportsCacheEntry{
			expiresAt: time.Now().Add(s.cacheTTL),
			reports:   reports,
			rollups:   rollups,
		}
		s.cacheMu.Unlock()
	}
	return reports, rollups, nil
}

func (s *server) queryReports(ctx context.Context, clusterID string, limit int32) ([]reportRecord, error) {
	var reports []reportRecord
	var lastEvaluatedKey map[string]types.AttributeValue

	for int32(len(reports)) < limit {
		input := &dynamodb.QueryInput{
			TableName:              aws.String(s.table),
			KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :sk_prefix)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk":        &types.AttributeValueMemberS{Value: "CLUSTER#" + clusterID},
				":sk_prefix": &types.AttributeValueMemberS{Value: "REPORT#"},
			},
			ScanIndexForward:  aws.Bool(false),
			Limit:             aws.Int32(limit - int32(len(reports))),
			ExclusiveStartKey: lastEvaluatedKey,
		}

		out, err := s.client.Query(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("query DynamoDB reports: %w", err)
		}

		for _, item := range out.Items {
			record, err := decodeReport(item)
			if err != nil {
				return nil, err
			}
			reports = append(reports, record)
		}

		if out.LastEvaluatedKey == nil {
			break
		}
		lastEvaluatedKey = out.LastEvaluatedKey
	}

	return reports, nil
}

func decodeReport(item map[string]types.AttributeValue) (reportRecord, error) {
	raw, ok := item["report_json"].(*types.AttributeValueMemberS)
	if !ok || strings.TrimSpace(raw.Value) == "" {
		return reportRecord{}, errors.New("DynamoDB item is missing report_json")
	}
	var report analyzer.Report
	if err := json.Unmarshal([]byte(raw.Value), &report); err != nil {
		return reportRecord{}, fmt.Errorf("decode report_json: %w", err)
	}
	return reportRecord{
		ClusterID:   report.ClusterID,
		GeneratedAt: report.GeneratedAt,
		Summary:     report.Summary,
		Findings:    report.Findings,
	}, nil
}

func (s *server) buildTrend(reports []reportRecord, rollups map[string]recommendationRollup) trendResponse {
	series := make([]trendPoint, 0, len(reports))
	for i := len(reports) - 1; i >= 0; i-- {
		report := reports[i]
		var high, medium, low int
		for _, finding := range report.Findings {
			switch finding.Severity {
			case "high":
				high++
			case "medium":
				medium++
			case "low":
				low++
			}
		}
		series = append(series, trendPoint{
			GeneratedAt:        report.GeneratedAt,
			Findings:           len(report.Findings),
			High:               high,
			Medium:             medium,
			Low:                low,
			NodeCount:          intFromSummary(report.Summary, "node_count"),
			RequestedMemoryMiB: intFromSummary(report.Summary, "requested_memory_mib"),
			ObservedMemoryMiB:  optionalIntFromSummary(report.Summary, "observed_memory_mib"),
			TwoNodeFeasible:    twoNodeFeasible(report.Summary),
		})
	}
	top := make([]recommendationRollup, 0, len(rollups))
	for _, rollup := range rollups {
		top = append(top, rollup)
	}
	sortRollups(top)

	counts := map[string]int{"high": 0, "medium": 0, "low": 0}
	if len(reports) > 0 {
		for _, finding := range reports[0].Findings {
			counts[finding.Severity]++
		}
	}
	return trendResponse{
		Window: trendWindow{
			FirstReportAt:  firstReportTime(reports),
			LatestReportAt: latestReportTime(reports),
			ReportCount:    len(reports),
			ObservedDays:   observedDays(reports),
			RequiredDays:   s.minRemediationDays,
		},
		Series:             series,
		SeverityCounts:     counts,
		TopRecommendations: top,
	}
}

func strAttr(item map[string]types.AttributeValue, key string) string {
	if val, ok := item[key].(*types.AttributeValueMemberS); ok {
		return val.Value
	}
	return ""
}

func intAttr(item map[string]types.AttributeValue, key string) int64 {
	if val, ok := item[key].(*types.AttributeValueMemberN); ok {
		if parsed, err := strconv.ParseInt(val.Value, 10, 64); err == nil {
			return parsed
		}
	}
	return 0
}

func (s *server) loadRecommendations(ctx context.Context, clusterID string) (map[string]recommendationRollup, error) {
	recs := make(map[string]recommendationRollup)
	var lastEvaluatedKey map[string]types.AttributeValue
	for {
		out, err := s.client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(s.table),
			KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :sk_prefix)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk":        &types.AttributeValueMemberS{Value: "CLUSTER#" + clusterID},
				":sk_prefix": &types.AttributeValueMemberS{Value: "REC#"},
			},
			ExclusiveStartKey: lastEvaluatedKey,
		})
		if err != nil {
			return nil, err
		}

		for _, item := range out.Items {
			skVal := strAttr(item, "sk")
			if !strings.HasPrefix(skVal, "REC#") {
				continue
			}
			key := strings.TrimPrefix(skVal, "REC#")

			firstSeenStr := strAttr(item, "first_seen_at")
			lastSeenStr := strAttr(item, "last_seen_at")
			var firstSeenAt, lastSeenAt time.Time
			if t, err := time.Parse(time.RFC3339, firstSeenStr); err == nil {
				firstSeenAt = t
			}
			if t, err := time.Parse(time.RFC3339, lastSeenStr); err == nil {
				lastSeenAt = t
			}

			occurrences := intAttr(item, "occurrences")

			var latest analyzer.Finding
			if rawFinding, ok := item["latest_finding_json"].(*types.AttributeValueMemberS); ok {
				_ = json.Unmarshal([]byte(rawFinding.Value), &latest)
			}

			recs[key] = recommendationRollup{
				Key:          key,
				RuleID:       strAttr(item, "rule_id"),
				Severity:     strAttr(item, "severity"),
				Namespace:    strAttr(item, "namespace"),
				Workload:     strAttr(item, "workload"),
				FirstSeenAt:  firstSeenAt,
				LastSeenAt:   lastSeenAt,
				Occurrences:  int(occurrences),
				Latest:       latest,
				ObservedDays: daysBetweenInclusive(firstSeenAt, lastSeenAt),
			}
		}

		if out.LastEvaluatedKey == nil {
			break
		}
		lastEvaluatedKey = out.LastEvaluatedKey
	}
	return recs, nil
}

func (s *server) rollups(ctx context.Context, reports []reportRecord) map[string]recommendationRollup {
	rollups := map[string]recommendationRollup{}
	latestKeys := map[string]bool{}
	if len(reports) > 0 {
		for _, finding := range reports[0].Findings {
			latestKeys[findingKey(finding.RuleID, finding.Namespace, finding.Workload)] = true
		}
	}
	for _, report := range reports {
		for _, finding := range report.Findings {
			key := findingKey(finding.RuleID, finding.Namespace, finding.Workload)
			rollup, ok := rollups[key]
			if !ok {
				rollup = recommendationRollup{
					Key:         key,
					RuleID:      finding.RuleID,
					Severity:    finding.Severity,
					Namespace:   finding.Namespace,
					Workload:    finding.Workload,
					Scope:       findingScope(finding),
					FirstSeenAt: report.GeneratedAt,
					LastSeenAt:  report.GeneratedAt,
					Latest:      finding,
				}
			}
			rollup.Occurrences++
			if report.GeneratedAt.Before(rollup.FirstSeenAt) {
				rollup.FirstSeenAt = report.GeneratedAt
			}
			if report.GeneratedAt.After(rollup.LastSeenAt) {
				rollup.LastSeenAt = report.GeneratedAt
				rollup.Latest = finding
				rollup.Severity = finding.Severity
			}
			rollups[key] = rollup
		}
	}

	var clusterID string
	if len(reports) > 0 {
		clusterID = reports[0].ClusterID
	}
	if clusterID != "" {
		if dbRecs, err := s.loadRecommendations(ctx, clusterID); err == nil && len(dbRecs) > 0 {
			for key, dbRec := range dbRecs {
				if rollup, exists := rollups[key]; exists {
					if dbRec.FirstSeenAt.Before(rollup.FirstSeenAt) {
						rollup.FirstSeenAt = dbRec.FirstSeenAt
					}
					if dbRec.LastSeenAt.After(rollup.LastSeenAt) {
						rollup.LastSeenAt = dbRec.LastSeenAt
						rollup.Latest = dbRec.Latest
						rollup.Severity = dbRec.Severity
					}
					if dbRec.Occurrences > rollup.Occurrences {
						rollup.Occurrences = dbRec.Occurrences
					}
					rollups[key] = rollup
				} else if dbRec.LastSeenAt.Equal(reports[0].GeneratedAt) {
					dbRec.Scope = findingScope(dbRec.Latest)
					rollups[key] = dbRec
				}
			}
		}
	}

	for key, rollup := range rollups {
		rollup.ObservedDays = daysBetweenInclusive(rollup.FirstSeenAt, rollup.LastSeenAt)
		rollup.LatestReportHas = latestKeys[key]
		rollup.Remediation = s.remediationFor(rollup)
		rollups[key] = rollup
	}
	return rollups
}

func (s *server) remediationFor(rollup recommendationRollup) remediationSummary {
	target, targetFound := s.remediationTargets[targetKey("", rollup.Namespace, rollup.Workload)]
	if !targetFound {
		target, targetFound = s.remediationTargets[targetKey("default", rollup.Namespace, rollup.Workload)]
	}
	if !targetFound {
		return remediationSummary{Supported: false, Available: false, Reason: "No remediation target is configured for this workload."}
	}
	if rollup.RuleID == "runtime-modernization-candidate" {
		return s.rewritePlanFor(rollup, target)
	}
	if !ruleSupported(target, rollup.RuleID) || !ruleCanPatchAPIYAML(rollup.RuleID) {
		return remediationSummary{Supported: false, Available: false, Reason: "This recommendation is not implemented as an api.yml patch yet."}
	}
	if target.ManifestPath == "" {
		return remediationSummary{Supported: false, Available: false, Reason: "No manifest_path is configured for this api.yml remediation."}
	}
	remediation := remediationSummary{
		Supported:    true,
		Available:    false,
		Action:       "api_yml_patch",
		ButtonLabel:  "Remediate",
		TargetRepo:   target.Repository,
		ManifestPath: target.ManifestPath,
		Container:    target.Container,
		Workflow:     s.githubWorkflow,
	}
	if !rollup.LatestReportHas {
		remediation.Reason = "The latest report no longer contains this recommendation."
		return remediation
	}
	if rollup.ObservedDays < s.minRemediationDays {
		remediation.Reason = fmt.Sprintf("Observed for %d day(s); remediation unlocks after %d days.", rollup.ObservedDays, s.minRemediationDays)
		return remediation
	}
	targetCPU, targetMemory := remediationTargetsFromEvidence(rollup.RuleID, rollup.Latest.Evidence)
	if needsResourceTarget(rollup.RuleID) && targetCPU == "" && targetMemory == "" {
		remediation.Reason = "Could not derive a safe request target from the recommendation evidence."
		return remediation
	}
	remediation.TargetCPU = targetCPU
	remediation.TargetMemory = targetMemory
	contextJSON, _ := json.Marshal(map[string]string{
		"recommendation": rollup.Latest.Recommendation,
		"evidence":       rollup.Latest.Evidence,
		"observed_days":  strconv.Itoa(rollup.ObservedDays),
		"occurrences":    strconv.Itoa(rollup.Occurrences),
	})
	inputs := map[string]string{
		"cluster_id":    "default",
		"rule_id":       rollup.RuleID,
		"namespace":     rollup.Namespace,
		"workload":      rollup.Workload,
		"repository":    target.Repository,
		"manifest_path": target.ManifestPath,
		"container":     target.Container,
		"target_cpu":    targetCPU,
		"target_memory": targetMemory,
		"context_json":  string(contextJSON),
	}
	if target.ClusterID != "" {
		inputs["cluster_id"] = target.ClusterID
	}
	remediation.WorkflowInputs = inputs
	remediation.Available = true
	remediation.Reason = "Ready to create an api.yml PR through CI/CD."
	return remediation
}

func (s *server) rewritePlanFor(rollup recommendationRollup, target remediationTarget) remediationSummary {
	remediation := remediationSummary{
		Supported:        true,
		Available:        false,
		Action:           "rewrite_plan",
		ButtonLabel:      "Plan Rewrite",
		TargetRepo:       target.Repository,
		InstructionsPath: target.InstructionsPath,
		Workflow:         s.rewriteWorkflow,
	}
	if !ruleSupported(target, rollup.RuleID) {
		remediation.Supported = false
		remediation.Reason = "This target does not allow rewrite planning for this rule."
		return remediation
	}
	if target.InstructionsPath == "" {
		remediation.Supported = false
		remediation.Reason = "No instructions_path is configured for this workload."
		return remediation
	}
	if !rollup.LatestReportHas {
		remediation.Reason = "The latest report no longer contains this recommendation."
		return remediation
	}
	if rollup.ObservedDays < s.minRemediationDays {
		remediation.Reason = fmt.Sprintf("Observed for %d day(s); rewrite planning unlocks after %d days.", rollup.ObservedDays, s.minRemediationDays)
		return remediation
	}
	contextJSON, _ := json.Marshal(map[string]string{
		"rule_id":              rollup.RuleID,
		"severity":             rollup.Severity,
		"recommendation":       rollup.Latest.Recommendation,
		"evidence":             rollup.Latest.Evidence,
		"risk":                 rollup.Latest.Risk,
		"confidence":           rollup.Latest.Confidence,
		"expected_cost_effect": rollup.Latest.ExpectedCostEffect,
		"observed_days":        strconv.Itoa(rollup.ObservedDays),
		"occurrences":          strconv.Itoa(rollup.Occurrences),
		"first_seen_at":        rollup.FirstSeenAt.Format(time.RFC3339),
		"last_seen_at":         rollup.LastSeenAt.Format(time.RFC3339),
	})
	clusterID := "default"
	if target.ClusterID != "" {
		clusterID = target.ClusterID
	}
	remediation.WorkflowInputs = map[string]string{
		"cluster_id":        clusterID,
		"namespace":         rollup.Namespace,
		"workload":          rollup.Workload,
		"repository":        target.Repository,
		"instructions_path": target.InstructionsPath,
		"context_json":      string(contextJSON),
	}
	remediation.Available = true
	remediation.Reason = "Ready to create coding-agent rewrite instructions through CI/CD."
	return remediation
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (s *server) dispatchWorkflow(ctx context.Context, token, workflow string, inputs map[string]string) error {
	ownerRepo := strings.Trim(s.githubRepository, "/")
	if !strings.Contains(ownerRepo, "/") {
		return fmt.Errorf("github repository must be owner/name")
	}
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/actions/workflows/%s/dispatches", ownerRepo, url.PathEscape(workflow))
	body, err := json.Marshal(map[string]any{
		"ref":    s.githubRef,
		"inputs": inputs,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("dispatch remediation workflow: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var payload map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&payload)
		if message, ok := payload["message"].(string); ok && message != "" {
			return fmt.Errorf("dispatch remediation workflow: GitHub returned %s: %s", resp.Status, message)
		}
		return fmt.Errorf("dispatch remediation workflow: GitHub returned %s", resp.Status)
	}
	return nil
}

func githubToken(ctx context.Context) (string, error) {
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		return token, nil
	}
	if token := strings.TrimSpace(os.Getenv("GH_TOKEN")); token != "" {
		return token, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", errors.New("set GITHUB_TOKEN/GH_TOKEN or run gh auth login before dispatching remediations")
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", errors.New("GitHub token was empty")
	}
	return token, nil
}

func loadRemediationTargets(path string) (map[string]remediationTarget, error) {
	if strings.TrimSpace(path) == "" {
		return map[string]remediationTarget{}, nil
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return map[string]remediationTarget{}, err
	}
	var file remediationTargetsFile
	if err := json.Unmarshal(payload, &file); err != nil {
		return map[string]remediationTarget{}, err
	}
	targets := map[string]remediationTarget{}
	for _, target := range file.Targets {
		if target.Namespace == "" || target.Workload == "" || target.Repository == "" {
			continue
		}
		targets[targetKey(target.ClusterID, target.Namespace, target.Workload)] = target
	}
	return targets, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func findingKey(ruleID, namespace, workload string) string {
	return strings.Join([]string{ruleID, namespace, workload}, "\x00")
}

func targetKey(clusterID, namespace, workload string) string {
	return strings.Join([]string{clusterID, namespace, workload}, "\x00")
}

func findingScope(finding analyzer.Finding) string {
	if finding.Namespace != "" && finding.Workload != "" {
		return finding.Namespace + "/" + finding.Workload
	}
	if finding.Workload != "" {
		return finding.Workload
	}
	return "cluster"
}

func sortRollups(rollups []recommendationRollup) {
	severityRank := map[string]int{"high": 0, "medium": 1, "low": 2}
	for i := 0; i < len(rollups)-1; i++ {
		for j := i + 1; j < len(rollups); j++ {
			left, right := rollups[i], rollups[j]
			swap := false
			if left.LatestReportHas != right.LatestReportHas {
				swap = !left.LatestReportHas && right.LatestReportHas
			} else if left.Occurrences != right.Occurrences {
				swap = left.Occurrences < right.Occurrences
			} else if severityRank[left.Severity] != severityRank[right.Severity] {
				swap = severityRank[left.Severity] > severityRank[right.Severity]
			} else {
				swap = left.Scope > right.Scope
			}
			if swap {
				rollups[i], rollups[j] = rollups[j], rollups[i]
			}
		}
	}
}

func firstReportTime(reports []reportRecord) *time.Time {
	if len(reports) == 0 {
		return nil
	}
	value := reports[len(reports)-1].GeneratedAt
	return &value
}

func latestReportTime(reports []reportRecord) *time.Time {
	if len(reports) == 0 {
		return nil
	}
	value := reports[0].GeneratedAt
	return &value
}

func observedDays(reports []reportRecord) int {
	if len(reports) == 0 {
		return 0
	}
	return daysBetweenInclusive(reports[len(reports)-1].GeneratedAt, reports[0].GeneratedAt)
}

func daysBetweenInclusive(start, end time.Time) int {
	if end.Before(start) {
		start, end = end, start
	}
	startDate := utcDate(start)
	endDate := utcDate(end)
	return int(endDate.Sub(startDate).Hours()/24) + 1
}

func utcDate(value time.Time) time.Time {
	year, month, day := value.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

func intFromSummary(summary map[string]any, key string) int64 {
	if value := optionalIntFromSummary(summary, key); value != nil {
		return *value
	}
	return 0
}

func optionalIntFromSummary(summary map[string]any, key string) *int64 {
	switch value := summary[key].(type) {
	case int64:
		return &value
	case int:
		converted := int64(value)
		return &converted
	case float64:
		converted := int64(value)
		return &converted
	case json.Number:
		converted, err := value.Int64()
		if err == nil {
			return &converted
		}
	}
	return nil
}

func twoNodeFeasible(summary map[string]any) *bool {
	raw, ok := summary["two_node_estimate"].(map[string]any)
	if !ok {
		return nil
	}
	value, ok := raw["feasible"].(bool)
	if !ok {
		return nil
	}
	return &value
}

func ruleSupported(target remediationTarget, ruleID string) bool {
	if len(target.SupportedRules) == 0 {
		return true
	}
	for _, supported := range target.SupportedRules {
		if supported == ruleID {
			return true
		}
	}
	return false
}

func ruleCanPatchAPIYAML(ruleID string) bool {
	switch ruleID {
	case "cpu-request-over-provisioned", "memory-request-over-provisioned", "memory-request-below-usage", "single-replica-pdb-blocks-drain", "cpu-hpa-low-request-sensitive":
		return true
	default:
		return false
	}
}

func needsResourceTarget(ruleID string) bool {
	return ruleID == "cpu-request-over-provisioned" || ruleID == "memory-request-over-provisioned" || ruleID == "memory-request-below-usage"
}

var (
	cpuEvidencePattern    = regexp.MustCompile(`Observed CPU ([0-9]+)m .* request ([0-9]+)m`)
	memoryEvidencePattern = regexp.MustCompile(`Observed memory ([0-9]+)Mi .* request ([0-9]+)Mi`)
)

func remediationTargetsFromEvidence(ruleID, evidence string) (targetCPU, targetMemory string) {
	switch ruleID {
	case "cpu-request-over-provisioned":
		matches := cpuEvidencePattern.FindStringSubmatch(evidence)
		if len(matches) != 3 {
			return "", ""
		}
		observed, _ := strconv.ParseInt(matches[1], 10, 64)
		current, _ := strconv.ParseInt(matches[2], 10, 64)
		target := maxInt64(50, observed*3)
		target = minInt64(target, current*70/100)
		target = maxInt64(target, 25)
		return fmt.Sprintf("%dm", roundUp(target, 5)), ""
	case "memory-request-over-provisioned":
		matches := memoryEvidencePattern.FindStringSubmatch(evidence)
		if len(matches) != 3 {
			return "", ""
		}
		observed, _ := strconv.ParseInt(matches[1], 10, 64)
		current, _ := strconv.ParseInt(matches[2], 10, 64)
		target := maxInt64(128, observed*3/2)
		target = minInt64(target, current*80/100)
		return "", fmt.Sprintf("%dMi", roundUp(target, 16))
	case "memory-request-below-usage":
		matches := memoryEvidencePattern.FindStringSubmatch(evidence)
		if len(matches) != 3 {
			return "", ""
		}
		observed, _ := strconv.ParseInt(matches[1], 10, 64)
		target := observed * 5 / 4
		return "", fmt.Sprintf("%dMi", roundUp(target, 16))
	default:
		return "", ""
	}
}

func roundUp(value, step int64) int64 {
	if step <= 0 {
		return value
	}
	if value%step == 0 {
		return value
	}
	return value + step - value%step
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
