#!/usr/bin/env bash
set -euo pipefail

TABLE="${DYNAMODB_TABLE:-cluster-optimizer-reports}"
REGION="${AWS_REGION:-us-east-1}"
ADDR="${CLUSTER_OPTIMIZER_UI_ADDR:-127.0.0.1:8088}"

echo "Starting Cluster Optimizer UI"
echo "  table:  ${TABLE}"
echo "  region: ${REGION}"
echo "  addr:   http://${ADDR}"

exec go run ./cmd/cluster-optimizer-ui \
  --table "${TABLE}" \
  --region "${REGION}" \
  --addr "${ADDR}"
