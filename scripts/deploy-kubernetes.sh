#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/deploy-kubernetes.sh [image-tag] [--wait]

Triggers the existing GitHub Actions Kubernetes deploy workflow with an
immutable image tag. When no image tag is provided, the script deploys the
latest successful image built by the Publish Image workflow on GITHUB_REF.
This keeps deployment in CI/CD while making the correct workflow inputs easy
to repeat from a local shell.

Environment overrides:
  GITHUB_REF          Git ref to deploy from. Default: main
  CLUSTER_ID          Logical cluster id in reports. Default: default
  ENABLE_DYNAMODB     Persist reports to DynamoDB. Default: true
  DYNAMODB_TABLE      DynamoDB table name. Default: cluster-optimizer-reports
  AWS_REGION          AWS region for DynamoDB. Default: us-east-1
  DOKS_CLUSTER_ID     DigitalOcean Kubernetes cluster id.
                      Default: 7dc99f7c-e0b7-4402-81ae-0e9a1fedcd82

Examples:
  scripts/deploy-kubernetes.sh --wait
  scripts/deploy-kubernetes.sh 2feb71995ad285b48d33b17f9b193a012dc2db24
  scripts/deploy-kubernetes.sh 2feb71995ad285b48d33b17f9b193a012dc2db24 --wait
EOF
}

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "error: required command '$1' was not found" >&2
    exit 127
  fi
}

latest_published_image_tag() {
  gh run list \
    --workflow "Publish Image" \
    --branch "${GITHUB_REF}" \
    --status completed \
    --limit 20 \
    --json conclusion,headSha \
    --jq 'map(select(.conclusion == "success"))[0].headSha // ""'
}

IMAGE_TAG=""
WAIT=false

while [ "$#" -gt 0 ]; do
  case "$1" in
    --help|-h)
      usage
      exit 0
      ;;
    --wait)
      WAIT=true
      shift
      ;;
    -*)
      echo "error: unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
    *)
      if [ -n "${IMAGE_TAG}" ]; then
        echo "error: image tag was provided more than once" >&2
        usage >&2
        exit 2
      fi
      IMAGE_TAG="$1"
      shift
      ;;
  esac
done

require_command gh

GITHUB_REF="${GITHUB_REF:-main}"
CLUSTER_ID="${CLUSTER_ID:-default}"
ENABLE_DYNAMODB="${ENABLE_DYNAMODB:-true}"
DYNAMODB_TABLE="${DYNAMODB_TABLE:-cluster-optimizer-reports}"
AWS_REGION="${AWS_REGION:-us-east-1}"
DOKS_CLUSTER_ID="${DOKS_CLUSTER_ID:-7dc99f7c-e0b7-4402-81ae-0e9a1fedcd82}"

case "${ENABLE_DYNAMODB}" in
  true|false) ;;
  *)
    echo "error: ENABLE_DYNAMODB must be 'true' or 'false'" >&2
    exit 2
    ;;
esac

if [ -z "${IMAGE_TAG}" ]; then
  echo "Resolving latest successful published image tag on ${GITHUB_REF}..."
  IMAGE_TAG="$(latest_published_image_tag)"
fi

if [ -z "${IMAGE_TAG}" ]; then
  echo "error: no successful Publish Image run found for ${GITHUB_REF}" >&2
  exit 1
fi

if [ "${IMAGE_TAG}" = "latest" ]; then
  echo "error: refuse to deploy mutable tag 'latest'; pass a commit SHA or release tag" >&2
  exit 2
fi

echo "Triggering Deploy Kubernetes for image tag ${IMAGE_TAG}..."
gh workflow run deploy-kubernetes.yml \
  --ref "${GITHUB_REF}" \
  -f "image_tag=${IMAGE_TAG}" \
  -f "cluster_id=${CLUSTER_ID}" \
  -f "enable_dynamodb=${ENABLE_DYNAMODB}" \
  -f "dynamodb_table=${DYNAMODB_TABLE}" \
  -f "aws_region=${AWS_REGION}" \
  -f "doks_cluster_id=${DOKS_CLUSTER_ID}"

if [ "${WAIT}" = "true" ]; then
  sleep 3
  run_id="$(gh run list --workflow "Deploy Kubernetes" --branch "${GITHUB_REF}" --limit 1 --json databaseId --jq '.[0].databaseId')"
  if [ -z "${run_id}" ]; then
    echo "error: could not find the triggered Deploy Kubernetes run" >&2
    exit 1
  fi
  gh run watch "${run_id}" --exit-status
fi
