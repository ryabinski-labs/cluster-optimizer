package store

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GipsyChef/cluster-optimizer/internal/analyzer"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type DynamoDBWriter struct {
	table   string
	client  *dynamodb.Client
	ttlDays int
}

func NewDynamoDBWriter(ctx context.Context, table string) (*DynamoDBWriter, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	ttlDays := 90
	if raw := os.Getenv("REPORT_TTL_DAYS"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			ttlDays = parsed
		}
	}
	return &DynamoDBWriter{table: table, client: dynamodb.NewFromConfig(cfg), ttlDays: ttlDays}, nil
}

// Occurrences returns a map of recommendation occurrence counts keyed by
// "RuleID\x00Namespace\x00Workload" — the same format the planner uses.
// Live applier uses this to verify multi-run evidence before mutating.
// A query error returns an empty map rather than failing the whole run;
// the planner treats a missing entry as "no evidence yet" and skips.
func (w *DynamoDBWriter) Occurrences(ctx context.Context, clusterID string) (map[string]int64, error) {
	recs, err := w.getRecommendations(ctx, clusterID)
	if err != nil {
		return map[string]int64{}, err
	}
	out := make(map[string]int64, len(recs))
	for sk, item := range recs {
		// sk format: "REC#" + RuleID + "\x00" + Namespace + "\x00" + Workload
		// Strip the "REC#" prefix.
		const prefix = "REC#"
		if len(sk) < len(prefix) || sk[:len(prefix)] != prefix {
			continue
		}
		key := sk[len(prefix):]
		if occ, ok := item["occurrences"].(*types.AttributeValueMemberN); ok {
			parsed, err := strconv.ParseInt(occ.Value, 10, 64)
			if err == nil {
				out[key] = parsed
			}
		}
	}
	return out, nil
}

func (w *DynamoDBWriter) getRecommendations(ctx context.Context, clusterID string) (map[string]map[string]types.AttributeValue, error) {
	out, err := w.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(w.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :sk_prefix)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":        &types.AttributeValueMemberS{Value: "CLUSTER#" + clusterID},
			":sk_prefix": &types.AttributeValueMemberS{Value: "REC#"},
		},
	})
	if err != nil {
		return nil, err
	}
	items := make(map[string]map[string]types.AttributeValue)
	for _, item := range out.Items {
		if skVal, ok := item["sk"].(*types.AttributeValueMemberS); ok {
			items[skVal.Value] = item
		}
	}
	return items, nil
}

func (w *DynamoDBWriter) PutReport(ctx context.Context, report analyzer.Report) error {
	payload, err := json.Marshal(report)
	if err != nil {
		return err
	}
	expiresAt := report.GeneratedAt.Add(time.Duration(w.ttlDays) * 24 * time.Hour).Unix()
	_, err = w.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(w.table),
		Item: map[string]types.AttributeValue{
			"pk":           &types.AttributeValueMemberS{Value: "CLUSTER#" + report.ClusterID},
			"sk":           &types.AttributeValueMemberS{Value: "REPORT#" + report.GeneratedAt.Format(time.RFC3339)},
			"cluster_id":   &types.AttributeValueMemberS{Value: report.ClusterID},
			"generated_at": &types.AttributeValueMemberS{Value: report.GeneratedAt.Format(time.RFC3339)},
			"expires_at":   &types.AttributeValueMemberN{Value: strconv.FormatInt(expiresAt, 10)},
			"report_json":  &types.AttributeValueMemberS{Value: string(payload)},
		},
	})
	if err != nil {
		return err
	}

	// Fetch existing recommendation records for this cluster to avoid sliding window loss
	existingRecs, err := w.getRecommendations(ctx, report.ClusterID)
	if err != nil {
		// Log or handle gracefully but don't fail the whole scan write if getRecommendations fails
		return nil
	}

	for _, finding := range report.Findings {
		key := strings.Join([]string{finding.RuleID, finding.Namespace, finding.Workload}, "\x00")
		sk := "REC#" + key

		var firstSeenAt string
		var occurrences int64

		if existing, ok := existingRecs[sk]; ok {
			if fsa, ok := existing["first_seen_at"].(*types.AttributeValueMemberS); ok {
				firstSeenAt = fsa.Value
			}
			if occStr, ok := existing["occurrences"].(*types.AttributeValueMemberN); ok {
				if parsed, err := strconv.ParseInt(occStr.Value, 10, 64); err == nil {
					occurrences = parsed
				}
			}
		}

		if firstSeenAt == "" {
			firstSeenAt = report.GeneratedAt.Format(time.RFC3339)
		}
		occurrences++

		findingPayload, _ := json.Marshal(finding)

		_, _ = w.client.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(w.table),
			Item: map[string]types.AttributeValue{
				"pk":                  &types.AttributeValueMemberS{Value: "CLUSTER#" + report.ClusterID},
				"sk":                  &types.AttributeValueMemberS{Value: sk},
				"rule_id":             &types.AttributeValueMemberS{Value: finding.RuleID},
				"severity":            &types.AttributeValueMemberS{Value: finding.Severity},
				"namespace":           &types.AttributeValueMemberS{Value: finding.Namespace},
				"workload":            &types.AttributeValueMemberS{Value: finding.Workload},
				"first_seen_at":       &types.AttributeValueMemberS{Value: firstSeenAt},
				"last_seen_at":        &types.AttributeValueMemberS{Value: report.GeneratedAt.Format(time.RFC3339)},
				"occurrences":         &types.AttributeValueMemberN{Value: strconv.FormatInt(occurrences, 10)},
				"latest_finding_json": &types.AttributeValueMemberS{Value: string(findingPayload)},
				"expires_at":          &types.AttributeValueMemberN{Value: strconv.FormatInt(expiresAt, 10)},
			},
		})
	}

	return nil
}
