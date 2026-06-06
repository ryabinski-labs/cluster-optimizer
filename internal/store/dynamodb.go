package store

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GipsyChef/cluster-optimizer/internal/analyzer"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type DynamoDBWriter struct {
	table   string
	client  *dynamodb.Client
	ttlDays int
}

// ExistingRecommendation captures the persisted state of a recommendation that
// PutReport needs to preserve across runs (first-seen timestamp and the
// rolling occurrence count). Exposing this struct lets a caller fetch it once
// and pass it back into PutReport, so a single advisory run only issues one
// Query instead of two.
type ExistingRecommendation struct {
	FirstSeenAt string
	Occurrences int64
}

// RemediationEvent is one row in the active-remediation audit log. It records
// what the applier or nudger did (or would have done in dry-run) so the UI
// can show operators that the engine is alive and what it touched.
//
// Mode is "live" or "dry-run". Kind is "patch_request" (applier),
// "cordon_evict" (nudger), or "delete_completed_pod" (pod GC). Applied is
// true only when a live mutation
// completed successfully; Error carries the message when it failed. For
// nudger events Container is empty, BeforeCPUm/AfterCPUm/BeforeMemMiB/
// AfterMemMiB are zero, and TargetNode/Evicted/EvictionErrors describe the
// consolidation outcome.
type RemediationEvent struct {
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
	Deleted         int       `json:"deleted,omitempty"`
	DeletionErrors  int       `json:"deletion_errors,omitempty"`
	OccurrenceCount int64     `json:"occurrence_count,omitempty"`
}

// EngineStatus is the single sentinel item that records the most recent
// posture of the remediation engine for a cluster. The UI reads it to render
// the engine-status strip (mode, halt switch, last-run summary) without
// scanning the event feed.
type EngineStatus struct {
	AutoApplyEnabled bool      `json:"auto_apply_enabled"`
	AutoApplyLive    bool      `json:"auto_apply_live"`
	NudgeEnabled     bool      `json:"nudge_enabled"`
	NudgeLive        bool      `json:"nudge_live"`
	GCEnabled        bool      `json:"gc_enabled"`
	GCLive           bool      `json:"gc_live"`
	HaltActive       bool      `json:"halt_active"`
	HaltReason       string    `json:"halt_reason,omitempty"`
	LastRunAt        time.Time `json:"last_run_at"`
	LastRunActions   int       `json:"last_run_actions"`
	LastRunApplied   int       `json:"last_run_applied"`
	LastRunErrors    int       `json:"last_run_errors"`
	LastClusterID    string    `json:"last_cluster_id,omitempty"`
}

// NewDynamoDBClient builds a DynamoDB client tuned for this project's usage
// pattern: short-lived advisory runs and a polling UI where a single stalled
// request would otherwise block waiting on the SDK's retry token bucket past
// the caller's context deadline.
//
// Changes vs. the SDK defaults:
//   - HTTP client gets explicit connection-level timeouts so a slow body
//     read fails fast rather than hanging until the request context fires.
//   - The retry rate limiter is replaced with ratelimit.None so a transient
//     timeout does not turn into "failed to get rate limit token, canceled".
//   - RetryMaxAttempts is bumped slightly so a single network blip recovers
//     within the existing context budget instead of failing the run.
func NewDynamoDBClient(ctx context.Context, optFns ...func(*config.LoadOptions) error) (*dynamodb.Client, error) {
	httpClient := awshttp.NewBuildableClient().
		WithTimeout(15 * time.Second).
		WithTransportOptions(func(t *http.Transport) {
			t.MaxIdleConns = 32
			t.MaxIdleConnsPerHost = 16
			t.IdleConnTimeout = 90 * time.Second
			t.ResponseHeaderTimeout = 5 * time.Second
			t.DialContext = (&net.Dialer{
				Timeout:   3 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext
		})

	baseOpts := []func(*config.LoadOptions) error{
		config.WithHTTPClient(httpClient),
		config.WithRetryer(func() aws.Retryer {
			return retry.NewStandard(func(o *retry.StandardOptions) {
				o.MaxAttempts = 4
				o.RateLimiter = ratelimit.None
			})
		}),
	}
	baseOpts = append(baseOpts, optFns...)

	cfg, err := config.LoadDefaultConfig(ctx, baseOpts...)
	if err != nil {
		return nil, err
	}
	return dynamodb.NewFromConfig(cfg), nil
}

func NewDynamoDBWriter(ctx context.Context, table string) (*DynamoDBWriter, error) {
	client, err := NewDynamoDBClient(ctx)
	if err != nil {
		return nil, err
	}
	ttlDays := 90
	if raw := os.Getenv("REPORT_TTL_DAYS"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			ttlDays = parsed
		}
	}
	return &DynamoDBWriter{table: table, client: client, ttlDays: ttlDays}, nil
}

// Occurrences returns the persisted recommendation occurrence counts keyed by
// "RuleID\x00Namespace\x00Workload" — the same format the planner uses.
// Live applier uses this to verify multi-run evidence before mutating.
// A query error returns an empty map rather than failing the whole run;
// the planner treats a missing entry as "no evidence yet" and skips.
func (w *DynamoDBWriter) Occurrences(ctx context.Context, clusterID string) (map[string]int64, error) {
	recs, err := w.existingRecommendations(ctx, clusterID)
	if err != nil {
		return map[string]int64{}, err
	}
	out := make(map[string]int64, len(recs))
	for key, rec := range recs {
		out[key] = rec.Occurrences
	}
	return out, nil
}

// ExistingRecommendations returns the persisted first_seen_at and occurrence
// count for every recommendation under clusterID. PutReport accepts the same
// map, so callers that already need occurrences (e.g. the CLI) can pass it
// back and skip the second Query that would otherwise run inside PutReport.
func (w *DynamoDBWriter) ExistingRecommendations(ctx context.Context, clusterID string) (map[string]ExistingRecommendation, error) {
	return w.existingRecommendations(ctx, clusterID)
}

// existingRecommendations issues a paginated Query that only projects the
// attributes PutReport needs (sk, first_seen_at, occurrences). Dropping the
// heavy latest_finding_json payload from this read shrinks the response by
// 1-2 orders of magnitude on busy clusters, which is the typical trigger for
// the "deserialization failed / context deadline exceeded" errors we saw.
func (w *DynamoDBWriter) existingRecommendations(ctx context.Context, clusterID string) (map[string]ExistingRecommendation, error) {
	out := make(map[string]ExistingRecommendation)
	var lastEvaluatedKey map[string]types.AttributeValue
	const prefix = "REC#"

	for {
		page, err := w.client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(w.table),
			KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :sk_prefix)"),
			ProjectionExpression:   aws.String("sk, first_seen_at, occurrences"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk":        &types.AttributeValueMemberS{Value: "CLUSTER#" + clusterID},
				":sk_prefix": &types.AttributeValueMemberS{Value: prefix},
			},
			ExclusiveStartKey: lastEvaluatedKey,
		})
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			skVal, ok := item["sk"].(*types.AttributeValueMemberS)
			if !ok || !strings.HasPrefix(skVal.Value, prefix) {
				continue
			}
			key := strings.TrimPrefix(skVal.Value, prefix)
			rec := ExistingRecommendation{}
			if fsa, ok := item["first_seen_at"].(*types.AttributeValueMemberS); ok {
				rec.FirstSeenAt = fsa.Value
			}
			if occ, ok := item["occurrences"].(*types.AttributeValueMemberN); ok {
				if parsed, err := strconv.ParseInt(occ.Value, 10, 64); err == nil {
					rec.Occurrences = parsed
				}
			}
			out[key] = rec
		}
		if page.LastEvaluatedKey == nil {
			break
		}
		lastEvaluatedKey = page.LastEvaluatedKey
	}
	return out, nil
}

// PutReport persists the report and bumps the rolling recommendation
// rollups. If existing is non-nil it is used as the source of truth for
// first_seen_at and prior occurrence counts — callers that already issued a
// Query (e.g. via Occurrences/ExistingRecommendations) should pass it in to
// avoid a second round-trip. Passing nil falls back to a Query inside this
// call.
func (w *DynamoDBWriter) PutReport(ctx context.Context, report analyzer.Report, existing map[string]ExistingRecommendation) error {
	payload, err := json.Marshal(report)
	if err != nil {
		return err
	}
	expiresAt := report.GeneratedAt.Add(time.Duration(w.ttlDays) * 24 * time.Hour).Unix()
	expiresAtStr := strconv.FormatInt(expiresAt, 10)
	generatedAt := report.GeneratedAt.Format(time.RFC3339)

	_, err = w.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(w.table),
		Item: map[string]types.AttributeValue{
			"pk":           &types.AttributeValueMemberS{Value: "CLUSTER#" + report.ClusterID},
			"sk":           &types.AttributeValueMemberS{Value: "REPORT#" + generatedAt},
			"cluster_id":   &types.AttributeValueMemberS{Value: report.ClusterID},
			"generated_at": &types.AttributeValueMemberS{Value: generatedAt},
			"expires_at":   &types.AttributeValueMemberN{Value: expiresAtStr},
			"report_json":  &types.AttributeValueMemberS{Value: string(payload)},
		},
	})
	if err != nil {
		return err
	}

	if existing == nil {
		// No caller-supplied state — fetch it ourselves. A failure here only
		// resets the rolling counters for this run; we still want the report
		// itself to be persisted, so we swallow the error.
		fetched, fetchErr := w.existingRecommendations(ctx, report.ClusterID)
		if fetchErr != nil {
			existing = map[string]ExistingRecommendation{}
		} else {
			existing = fetched
		}
	}

	if len(report.Findings) == 0 {
		return nil
	}

	// Build the REC#... items in memory, then write them via BatchWriteItem
	// (25 per request) instead of one PutItem per finding. This replaces an
	// N+1 write fan-out with ceil(N/25) calls and lets the SDK handle the
	// per-batch UnprocessedItems retry loop.
	requests := make([]types.WriteRequest, 0, len(report.Findings))
	for _, finding := range report.Findings {
		key := strings.Join([]string{finding.RuleID, finding.Namespace, finding.Workload}, "\x00")
		sk := "REC#" + key

		firstSeenAt := generatedAt
		var occurrences int64
		if prev, ok := existing[key]; ok {
			if prev.FirstSeenAt != "" {
				firstSeenAt = prev.FirstSeenAt
			}
			occurrences = prev.Occurrences
		}
		occurrences++

		findingPayload, _ := json.Marshal(finding)
		requests = append(requests, types.WriteRequest{
			PutRequest: &types.PutRequest{
				Item: map[string]types.AttributeValue{
					"pk":                  &types.AttributeValueMemberS{Value: "CLUSTER#" + report.ClusterID},
					"sk":                  &types.AttributeValueMemberS{Value: sk},
					"rule_id":             &types.AttributeValueMemberS{Value: finding.RuleID},
					"severity":            &types.AttributeValueMemberS{Value: finding.Severity},
					"namespace":           &types.AttributeValueMemberS{Value: finding.Namespace},
					"workload":            &types.AttributeValueMemberS{Value: finding.Workload},
					"first_seen_at":       &types.AttributeValueMemberS{Value: firstSeenAt},
					"last_seen_at":        &types.AttributeValueMemberS{Value: generatedAt},
					"occurrences":         &types.AttributeValueMemberN{Value: strconv.FormatInt(occurrences, 10)},
					"latest_finding_json": &types.AttributeValueMemberS{Value: string(findingPayload)},
					"expires_at":          &types.AttributeValueMemberN{Value: expiresAtStr},
				},
			},
		})
	}

	return w.batchWrite(ctx, requests)
}

// LoadEngineStatus fetches the sentinel ENGINE_STATUS row for clusterID.
// Returns (nil, nil) when no row exists yet (cluster has never run the
// remediation engine), which the UI shows as "no remediation activity yet".
func LoadEngineStatus(ctx context.Context, client *dynamodb.Client, table, clusterID string) (*EngineStatus, error) {
	out, err := client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(table),
		Key: map[string]types.AttributeValue{
			"pk": &types.AttributeValueMemberS{Value: "CLUSTER#" + clusterID},
			"sk": &types.AttributeValueMemberS{Value: "ENGINE_STATUS"},
		},
	})
	if err != nil {
		return nil, err
	}
	if len(out.Item) == 0 {
		return nil, nil
	}
	raw, ok := out.Item["status_json"].(*types.AttributeValueMemberS)
	if !ok || strings.TrimSpace(raw.Value) == "" {
		return nil, nil
	}
	var status EngineStatus
	if err := json.Unmarshal([]byte(raw.Value), &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// LoadRemediations returns RemediationEvents for clusterID newer than `since`,
// capped at limit, newest first. A zero `since` returns the full TTL window.
// A non-positive limit defaults to 100.
func LoadRemediations(ctx context.Context, client *dynamodb.Client, table, clusterID string, since time.Time, limit int) ([]RemediationEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	events := make([]RemediationEvent, 0, limit)
	var lastEvaluatedKey map[string]types.AttributeValue
	for len(events) < limit {
		input := &dynamodb.QueryInput{
			TableName:              aws.String(table),
			KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :sk_prefix)"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk":        &types.AttributeValueMemberS{Value: "CLUSTER#" + clusterID},
				":sk_prefix": &types.AttributeValueMemberS{Value: "REMEDIATION#"},
			},
			ScanIndexForward:  aws.Bool(false),
			Limit:             aws.Int32(int32(limit - len(events))),
			ExclusiveStartKey: lastEvaluatedKey,
		}
		page, err := client.Query(ctx, input)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			raw, ok := item["event_json"].(*types.AttributeValueMemberS)
			if !ok || strings.TrimSpace(raw.Value) == "" {
				continue
			}
			var event RemediationEvent
			if err := json.Unmarshal([]byte(raw.Value), &event); err != nil {
				continue
			}
			if !since.IsZero() && event.Timestamp.Before(since) {
				return events, nil
			}
			events = append(events, event)
			if len(events) >= limit {
				return events, nil
			}
		}
		if page.LastEvaluatedKey == nil {
			break
		}
		lastEvaluatedKey = page.LastEvaluatedKey
	}
	return events, nil
}

// PutRemediations persists a batch of RemediationEvents under the cluster
// partition. Sort key format: `REMEDIATION#<RFC3339Nano>#<seq>` so events
// from the same run keep deterministic order and a descending scan returns
// newest first. TTL matches REPORT# items.
//
// Best-effort by design: callers should log+continue on error so a failing
// audit log never aborts a remediation run.
func (w *DynamoDBWriter) PutRemediations(ctx context.Context, clusterID string, events []RemediationEvent) error {
	if len(events) == 0 {
		return nil
	}
	requests := make([]types.WriteRequest, 0, len(events))
	for i, event := range events {
		ts := event.Timestamp
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		expiresAt := ts.Add(time.Duration(w.ttlDays) * 24 * time.Hour).Unix()
		payload, err := json.Marshal(event)
		if err != nil {
			return err
		}
		sk := "REMEDIATION#" + ts.UTC().Format(time.RFC3339Nano) + "#" + strconv.Itoa(i)
		item := map[string]types.AttributeValue{
			"pk":         &types.AttributeValueMemberS{Value: "CLUSTER#" + clusterID},
			"sk":         &types.AttributeValueMemberS{Value: sk},
			"timestamp":  &types.AttributeValueMemberS{Value: ts.UTC().Format(time.RFC3339Nano)},
			"mode":       &types.AttributeValueMemberS{Value: event.Mode},
			"kind":       &types.AttributeValueMemberS{Value: event.Kind},
			"event_json": &types.AttributeValueMemberS{Value: string(payload)},
			"expires_at": &types.AttributeValueMemberN{Value: strconv.FormatInt(expiresAt, 10)},
		}
		requests = append(requests, types.WriteRequest{PutRequest: &types.PutRequest{Item: item}})
	}
	return w.batchWrite(ctx, requests)
}

// PutEngineStatus overwrites the sentinel ENGINE_STATUS item for clusterID.
// One row per cluster — last writer wins. Cheap and idempotent.
func (w *DynamoDBWriter) PutEngineStatus(ctx context.Context, clusterID string, status EngineStatus) error {
	ts := status.LastRunAt
	if ts.IsZero() {
		ts = time.Now().UTC()
		status.LastRunAt = ts
	}
	payload, err := json.Marshal(status)
	if err != nil {
		return err
	}
	expiresAt := ts.Add(time.Duration(w.ttlDays) * 24 * time.Hour).Unix()
	_, err = w.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(w.table),
		Item: map[string]types.AttributeValue{
			"pk":          &types.AttributeValueMemberS{Value: "CLUSTER#" + clusterID},
			"sk":          &types.AttributeValueMemberS{Value: "ENGINE_STATUS"},
			"status_json": &types.AttributeValueMemberS{Value: string(payload)},
			"updated_at":  &types.AttributeValueMemberS{Value: ts.UTC().Format(time.RFC3339Nano)},
			"expires_at":  &types.AttributeValueMemberN{Value: strconv.FormatInt(expiresAt, 10)},
		},
	})
	return err
}

// batchWrite drains requests in chunks of 25 and re-submits any
// UnprocessedItems the service returns. Each round is bounded by the caller's
// ctx, so a stalled retry can't outlive the run.
func (w *DynamoDBWriter) batchWrite(ctx context.Context, requests []types.WriteRequest) error {
	const chunkSize = 25
	for i := 0; i < len(requests); i += chunkSize {
		end := i + chunkSize
		if end > len(requests) {
			end = len(requests)
		}
		batch := map[string][]types.WriteRequest{w.table: requests[i:end]}

		for attempt := 0; attempt < 5; attempt++ {
			out, err := w.client.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{
				RequestItems: batch,
			})
			if err != nil {
				return err
			}
			if len(out.UnprocessedItems) == 0 || len(out.UnprocessedItems[w.table]) == 0 {
				break
			}
			batch = out.UnprocessedItems
			if attempt == 4 {
				return errors.New("batchWrite: unprocessed items remain after retries")
			}
		}
	}
	return nil
}
