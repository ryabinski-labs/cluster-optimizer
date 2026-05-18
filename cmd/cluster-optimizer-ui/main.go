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

//go:embed static
var staticFiles embed.FS

type reportRecord struct {
	ClusterID   string             `json:"cluster_id"`
	GeneratedAt time.Time          `json:"generated_at"`
	Summary     json.RawMessage    `json:"summary"`
	Findings    []analyzer.Finding `json:"findings"`
}

type apiResponse struct {
	ClusterID string         `json:"cluster_id"`
	Table     string         `json:"table"`
	Region    string         `json:"region"`
	Reports   []reportRecord `json:"reports"`
}

type server struct {
	table  string
	region string
	client *dynamodb.Client
	static http.Handler
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
	flags := flag.NewFlagSet("cluster-optimizer-ui", flag.ContinueOnError)
	flags.StringVar(&addr, "addr", envOr("CLUSTER_OPTIMIZER_UI_ADDR", "127.0.0.1:8088"), "listen address")
	flags.StringVar(&table, "table", envOr("DYNAMODB_TABLE", "cluster-optimizer-reports"), "DynamoDB table")
	flags.StringVar(&region, "region", envOr("AWS_REGION", "us-east-1"), "AWS region")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	staticRoot, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return err
	}
	srv := &server{
		table:  table,
		region: region,
		client: dynamodb.NewFromConfig(cfg),
		static: http.FileServer(http.FS(staticRoot)),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/reports", srv.handleReports)
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
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
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

	reports, err := s.queryReports(ctx, clusterID, limit)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, apiResponse{
		ClusterID: clusterID,
		Table:     s.table,
		Region:    s.region,
		Reports:   reports,
	})
}

func (s *server) queryReports(ctx context.Context, clusterID string, limit int32) ([]reportRecord, error) {
	out, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("pk = :pk AND begins_with(sk, :sk_prefix)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":        &types.AttributeValueMemberS{Value: "CLUSTER#" + clusterID},
			":sk_prefix": &types.AttributeValueMemberS{Value: "REPORT#"},
		},
		ScanIndexForward: aws.Bool(false),
		Limit:            aws.Int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("query DynamoDB reports: %w", err)
	}

	reports := make([]reportRecord, 0, len(out.Items))
	for _, item := range out.Items {
		record, err := decodeReport(item)
		if err != nil {
			return nil, err
		}
		reports = append(reports, record)
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
	summary, err := json.Marshal(report.Summary)
	if err != nil {
		return reportRecord{}, fmt.Errorf("encode report summary: %w", err)
	}
	return reportRecord{
		ClusterID:   report.ClusterID,
		GeneratedAt: report.GeneratedAt,
		Summary:     summary,
		Findings:    report.Findings,
	}, nil
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

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
