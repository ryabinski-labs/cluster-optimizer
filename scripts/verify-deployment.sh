#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/verify-deployment.sh [image-tag] [--run-job]

Verifies that the live cluster-optimizer CronJob is pinned to the expected
image tag. When no image tag is provided, the script expects the latest
successful image built by the Publish Image workflow on GITHUB_REF. With
--run-job, it also creates a one-off Job from the CronJob and waits for it to
complete.

Environment overrides:
  GITHUB_REF     Git ref used to resolve the latest published image. Default: main
  NAMESPACE      Kubernetes namespace. Default: cluster-optimizer
  CRONJOB        CronJob name. Default: cluster-optimizer
  IMAGE_NAME     Image repository. Default: ghcr.io/gipsychef/cluster-optimizer
  VERIFY_JOB     One-off job name. Default: cluster-optimizer-deploy-verify

Examples:
  scripts/verify-deployment.sh
  scripts/verify-deployment.sh --run-job
  scripts/verify-deployment.sh 2feb71995ad285b48d33b17f9b193a012dc2db24
  scripts/verify-deployment.sh 2feb71995ad285b48d33b17f9b193a012dc2db24 --run-job
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
RUN_JOB=false

while [ "$#" -gt 0 ]; do
  case "$1" in
    --help|-h)
      usage
      exit 0
      ;;
    --run-job)
      RUN_JOB=true
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

require_command kubectl
require_command gh

GITHUB_REF="${GITHUB_REF:-main}"
NAMESPACE="${NAMESPACE:-cluster-optimizer}"
CRONJOB="${CRONJOB:-cluster-optimizer}"
IMAGE_NAME="${IMAGE_NAME:-ghcr.io/gipsychef/cluster-optimizer}"
VERIFY_JOB="${VERIFY_JOB:-cluster-optimizer-deploy-verify}"

if [ -z "${IMAGE_TAG}" ]; then
  echo "Resolving latest successful published image tag on ${GITHUB_REF}..."
  IMAGE_TAG="$(latest_published_image_tag)"
fi

if [ -z "${IMAGE_TAG}" ]; then
  echo "error: no successful Publish Image run found for ${GITHUB_REF}" >&2
  exit 1
fi

if [ "${IMAGE_TAG}" = "latest" ]; then
  echo "error: refuse to verify mutable tag 'latest'; pass the deployed immutable tag" >&2
  exit 2
fi

EXPECTED_IMAGE="${IMAGE_NAME}:${IMAGE_TAG}"

actual_image="$(kubectl get cronjob "${CRONJOB}" -n "${NAMESPACE}" -o jsonpath='{.spec.jobTemplate.spec.template.spec.containers[0].image}')"
pull_policy="$(kubectl get cronjob "${CRONJOB}" -n "${NAMESPACE}" -o jsonpath='{.spec.jobTemplate.spec.template.spec.containers[0].imagePullPolicy}')"
last_success="$(kubectl get cronjob "${CRONJOB}" -n "${NAMESPACE}" -o jsonpath='{.status.lastSuccessfulTime}')"

echo "CronJob: ${NAMESPACE}/${CRONJOB}"
echo "Image: ${actual_image}"
echo "Pull policy: ${pull_policy:-<unset>}"
echo "Last successful schedule: ${last_success:-<none>}"

if [ "${actual_image}" != "${EXPECTED_IMAGE}" ]; then
  echo "error: expected image ${EXPECTED_IMAGE}, got ${actual_image}" >&2
  exit 1
fi

echo "CronJob image matches expected immutable tag."

if [ "${RUN_JOB}" != "true" ]; then
  exit 0
fi

if kubectl get job "${VERIFY_JOB}" -n "${NAMESPACE}" >/dev/null 2>&1; then
  echo "Deleting existing verification job ${NAMESPACE}/${VERIFY_JOB}..."
  kubectl delete job "${VERIFY_JOB}" -n "${NAMESPACE}" --wait=true
fi

echo "Creating verification job ${NAMESPACE}/${VERIFY_JOB}..."
kubectl create job "${VERIFY_JOB}" -n "${NAMESPACE}" "--from=cronjob/${CRONJOB}"
kubectl wait -n "${NAMESPACE}" "--for=condition=complete" "job/${VERIFY_JOB}" --timeout=180s

job_image="$(kubectl get job "${VERIFY_JOB}" -n "${NAMESPACE}" -o jsonpath='{.spec.template.spec.containers[0].image}')"
if [ "${job_image}" != "${EXPECTED_IMAGE}" ]; then
  echo "error: verification job used image ${job_image}, expected ${EXPECTED_IMAGE}" >&2
  exit 1
fi

echo "Verification job completed with image ${job_image}."
