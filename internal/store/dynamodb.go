package store

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"time"

	"github.com/GipsyChef/cluster-optimizer/internal/analyzer"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type DynamoDBWriter struct {
	table string
	client *dynamodb.Client
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

func (w *DynamoDBWriter) PutReport(ctx context.Context, report analyzer.Report) error {
	payload, err := json.Marshal(report)
	if err != nil {
		return err
	}
	expiresAt := report.GeneratedAt.Add(time.Duration(w.ttlDays) * 24 * time.Hour).Unix()
	_, err = w.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(w.table),
		Item: map[string]types.AttributeValue{
			"pk":          &types.AttributeValueMemberS{Value: "CLUSTER#" + report.ClusterID},
			"sk":          &types.AttributeValueMemberS{Value: "REPORT#" + report.GeneratedAt.Format(time.RFC3339)},
			"cluster_id":  &types.AttributeValueMemberS{Value: report.ClusterID},
			"generated_at": &types.AttributeValueMemberS{Value: report.GeneratedAt.Format(time.RFC3339)},
			"expires_at":  &types.AttributeValueMemberN{Value: strconv.FormatInt(expiresAt, 10)},
			"report_json": &types.AttributeValueMemberS{Value: string(payload)},
		},
	})
	return err
}

