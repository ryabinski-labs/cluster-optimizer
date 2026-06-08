#!/usr/bin/env bash
# Create the cluster-optimizer single-table schema in DynamoDB Local.
# Safe to re-run: ignores "table already exists".
set -euo pipefail

TABLE="${DYNAMODB_TABLE:-cluster-optimizer-reports}"
ENDPOINT="${AWS_ENDPOINT_URL_DYNAMODB:-http://localhost:8001}"
REGION="${AWS_REGION:-us-east-1}"

export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-local}"
export AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-local}"

aws dynamodb create-table \
  --table-name "$TABLE" \
  --attribute-definitions \
    AttributeName=pk,AttributeType=S \
    AttributeName=sk,AttributeType=S \
  --key-schema \
    AttributeName=pk,KeyType=HASH \
    AttributeName=sk,KeyType=RANGE \
  --billing-mode PAY_PER_REQUEST \
  --endpoint-url "$ENDPOINT" \
  --region "$REGION" 2>&1 | grep -v "Table already exists" || true

echo "Table '$TABLE' ready at $ENDPOINT"
