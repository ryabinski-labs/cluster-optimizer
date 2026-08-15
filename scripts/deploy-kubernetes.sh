#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/deploy-kubernetes.sh [image-tag] [--wait]
                                    [--live-apply] [--live-nudge]

Triggers the existing GitHub Actions Kubernetes deploy workflow with an
immutable image tag. When no image tag is provided, the script deploys the
latest successful image built by the Publish Image workflow on GITHUB_REF.
This keeps deployment in CI/CD while making the correct workflow inputs easy
to repeat from a local shell.

Remediation mode is decided by two gates per remediation path: the CLI flag
baked into the manifest args (--auto-apply / --nudge) AND the matching env
var on the CronJob. The committed manifests always ship the env vars as
"false", so the engine stays in dry-run until a deploy turns a gate on.
--live-apply and --live-nudge below are the only supported way to do that;
they set CLUSTER_OPTIMIZER_AUTOAPPLY=true and CLUSTER_OPTIMIZER_NUDGE_LIVE=true
on the deployed CronJob. Omitting them returns the cluster to dry-run. The
halt ConfigMap overrides both gates regardless of what is deployed.

Live apply also needs manifests/rbac-applier.yaml on the cluster
(scripts/apply-rbac.sh --applier), or every patch attempt 403s at runtime.

Flags:
  --wait          Watch the triggered run to completion.
  --live-apply    Deploy with CLUSTER_OPTIMIZER_AUTOAPPLY=true (live workload
                  patching). Requires ENABLE_DYNAMODB=true.
  --live-nudge    Deploy with CLUSTER_OPTIMIZER_NUDGE_LIVE=true (live cordon
                  and eviction for consolidation).
  --help, -h      Show this message.

Environment overrides:
  GITHUB_REF          Git ref to deploy from. Default: main
  CLUSTER_ID          Logical cluster id in reports. Default: default
  ENABLE_DYNAMODB     Persist reports to DynamoDB. Default: true
  DYNAMODB_TABLE      DynamoDB table name. Default: cluster-optimizer-reports
  AWS_REGION          AWS region for DynamoDB. Default: us-east-1
  DOKS_CLUSTER_ID     DigitalOcean Kubernetes cluster id.
                      Default: 7dc99f7c-e0b7-4402-81ae-0e9a1fedcd82
  ENABLE_LIVE_APPLY   Same as --live-apply. Default: false
  ENABLE_LIVE_NUDGE   Same as --live-nudge. Default: false

Examples:
  scripts/deploy-kubernetes.sh --wait
  scripts/deploy-kubernetes.sh 2feb71995ad285b48d33b17f9b193a012dc2db24
  scripts/deploy-kubernetes.sh 2feb71995ad285b48d33b17f9b193a012dc2db24 --wait
  scripts/deploy-kubernetes.sh --live-apply --live-nudge --wait
  scripts/deploy-kubernetes.sh --wait   # re-deploy back to dry-run
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
LIVE_APPLY_FLAG=false
LIVE_NUDGE_FLAG=false

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
    --live-apply)
      LIVE_APPLY_FLAG=true
      shift
      ;;
    --live-nudge)
      LIVE_NUDGE_FLAG=true
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

# A flag can only turn a gate on. It never silently downgrades an explicit
# ENABLE_LIVE_* env value, so `ENABLE_LIVE_APPLY=false ... --live-apply`
# deploys live rather than resolving to an ambiguous middle state.
if [ "${LIVE_APPLY_FLAG}" = "true" ]; then
  ENABLE_LIVE_APPLY=true
fi
if [ "${LIVE_NUDGE_FLAG}" = "true" ]; then
  ENABLE_LIVE_NUDGE=true
fi
ENABLE_LIVE_APPLY="${ENABLE_LIVE_APPLY:-false}"
ENABLE_LIVE_NUDGE="${ENABLE_LIVE_NUDGE:-false}"

case "${ENABLE_DYNAMODB}" in
  true|false) ;;
  *)
    echo "error: ENABLE_DYNAMODB must be 'true' or 'false'" >&2
    exit 2
    ;;
esac

case "${ENABLE_LIVE_APPLY}" in
  true|false) ;;
  *)
    echo "error: ENABLE_LIVE_APPLY must be 'true' or 'false'" >&2
    exit 2
    ;;
esac

case "${ENABLE_LIVE_NUDGE}" in
  true|false) ;;
  *)
    echo "error: ENABLE_LIVE_NUDGE must be 'true' or 'false'" >&2
    exit 2
    ;;
esac

# The workflow rejects this combination too, but failing here saves a CI
# round-trip: the applier requires a finding to recur across 3 consecutive
# runs, which it cannot establish without DynamoDB persistence.
if [ "${ENABLE_LIVE_APPLY}" = "true" ] && [ "${ENABLE_DYNAMODB}" != "true" ]; then
  echo "error: live apply requires ENABLE_DYNAMODB=true; the applier has no run history without it" >&2
  exit 2
fi

# manifests/cronjob.yaml (the ENABLE_DYNAMODB=false path) keeps both gates
# commented out, and the workflow only rewrites the value of an env var that
# already exists. Asking for a live gate there would deploy dry-run while
# reporting success, so refuse instead of lying about the posture.
if [ "${ENABLE_LIVE_NUDGE}" = "true" ] && [ "${ENABLE_DYNAMODB}" != "true" ]; then
  echo "error: live nudge requires ENABLE_DYNAMODB=true; manifests/cronjob.yaml has no gate to turn on" >&2
  exit 2
fi

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

describe_gate() {
  if [ "$2" = "true" ]; then
    echo "  $1: LIVE ($3=true)"
  else
    echo "  $1: dry-run ($3=false)"
  fi
}

echo "Triggering Deploy Kubernetes for image tag ${IMAGE_TAG}..."
echo "Remediation mode this deploy will leave behind:"
describe_gate "auto-apply" "${ENABLE_LIVE_APPLY}" CLUSTER_OPTIMIZER_AUTOAPPLY
describe_gate "nudge" "${ENABLE_LIVE_NUDGE}" CLUSTER_OPTIMIZER_NUDGE_LIVE
if [ "${ENABLE_LIVE_APPLY}" = "true" ] || [ "${ENABLE_LIVE_NUDGE}" = "true" ]; then
  echo "  note: the cluster-optimizer/cluster-optimizer-halt ConfigMap still overrides both gates."
fi

gh workflow run deploy-kubernetes.yml \
  --ref "${GITHUB_REF}" \
  -f "image_tag=${IMAGE_TAG}" \
  -f "cluster_id=${CLUSTER_ID}" \
  -f "enable_dynamodb=${ENABLE_DYNAMODB}" \
  -f "dynamodb_table=${DYNAMODB_TABLE}" \
  -f "aws_region=${AWS_REGION}" \
  -f "doks_cluster_id=${DOKS_CLUSTER_ID}" \
  -f "enable_live_apply=${ENABLE_LIVE_APPLY}" \
  -f "enable_live_nudge=${ENABLE_LIVE_NUDGE}"

if [ "${WAIT}" = "true" ]; then
  sleep 3
  run_id="$(gh run list --workflow "Deploy Kubernetes" --branch "${GITHUB_REF}" --limit 1 --json databaseId --jq '.[0].databaseId')"
  if [ -z "${run_id}" ]; then
    echo "error: could not find the triggered Deploy Kubernetes run" >&2
    exit 1
  fi
  gh run watch "${run_id}" --exit-status
fi
