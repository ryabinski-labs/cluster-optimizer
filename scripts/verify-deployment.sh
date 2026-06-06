#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/verify-deployment.sh [image-tag] [--run-job]

Answers whether the live cluster-optimizer CronJob is running the latest
successfully published image for GITHUB_REF. When an image tag is provided, it
answers whether that specific image is deployed instead. With --run-job, it
also creates a one-off Job from the CronJob and waits for it to complete.
The script also verifies that the live CronJob matches the rendered repo
manifest, using the same inputs as the deploy workflow.

Environment overrides:
  GITHUB_REF     Git ref used to resolve the latest published image. Default: main
  NAMESPACE      Kubernetes namespace. Default: cluster-optimizer
  CRONJOB        CronJob name. Default: cluster-optimizer
  IMAGE_NAME     Image repository. Default: ghcr.io/ryabinski-labs/cluster-optimizer
  VERIFY_JOB     One-off job name. Default: cluster-optimizer-deploy-verify
  ENABLE_DYNAMODB
                 Use examples/cronjob-dynamodb.yaml as the expected manifest.
                 Default: true
  CONFIG_MANIFEST
                 Expected CronJob manifest. Default: based on ENABLE_DYNAMODB
  CLUSTER_ID     Logical cluster id expected in CronJob env. Default: default
  DYNAMODB_TABLE DynamoDB table expected in CronJob env.
                 Default: cluster-optimizer-reports
  VERIFY_CONFIG  Verify rendered CronJob manifest matches the live CronJob.
                 Default: true
  TARGETS_FILE   Local remediation targets config.
                 Default: config/remediation-targets.json
  TARGETS_NAMESPACE
                 Namespace for the remediation targets ConfigMap.
                 Default: NAMESPACE
  TARGETS_CONFIGMAP
                 Remediation targets ConfigMap name.
                 Default: cluster-optimizer-targets
  TARGETS_KEY    File key inside the remediation targets ConfigMap.
                 Default: remediation-targets.json
  VERIFY_TARGETS_CONFIG
                 Verify TARGETS_FILE matches the live ConfigMap. Default: auto
  SERVICE_ACCOUNT
                 ServiceAccount the CronJob runs as, checked by the RBAC
                 verification. Default: cluster-optimizer
  VERIFY_RBAC    Verify the ServiceAccount has the pod permissions the engine
                 needs (incl. delete pods for completed-pod GC). One of
                 true|false|auto. auto skips when impersonation is
                 unavailable. Default: auto

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

resolve_local_path() {
  local path="$1"

  case "${path}" in
    /*)
      printf '%s\n' "${path}"
      ;;
    *)
      if [ -e "${path}" ]; then
        printf '%s\n' "${path}"
      else
        printf '%s/%s\n' "${REPO_ROOT}" "${path}"
      fi
      ;;
  esac
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

render_expected_cronjob_manifest() {
  local manifest="$1"

  if [ "${ENABLE_DYNAMODB}" = "true" ]; then
    kubectl patch --local -f "${manifest}" --type=merge \
      -p "{\"metadata\":{\"name\":\"${CRONJOB}\",\"namespace\":\"${NAMESPACE}\"}}" \
      -o yaml |
      kubectl set image --local -f - "optimizer=${EXPECTED_IMAGE}" -o yaml |
      kubectl set env --local -f - "CLUSTER_OPTIMIZER_CLUSTER_ID=${CLUSTER_ID}" -o yaml |
      kubectl set env --local -f - "DYNAMODB_TABLE=${DYNAMODB_TABLE}" -o yaml
  else
    kubectl patch --local -f "${manifest}" --type=merge \
      -p "{\"metadata\":{\"name\":\"${CRONJOB}\",\"namespace\":\"${NAMESPACE}\"}}" \
      -o yaml |
      kubectl set image --local -f - "optimizer=${EXPECTED_IMAGE}" -o yaml |
      kubectl set env --local -f - "CLUSTER_OPTIMIZER_CLUSTER_ID=${CLUSTER_ID}" -o yaml
  fi
}

verify_targets_config_sync() {
  local local_targets_config
  local cluster_targets_config

  if [ ! -f "${TARGETS_FILE}" ]; then
    if [ "${VERIFY_TARGETS_CONFIG}" = "auto" ]; then
      echo "Targets config sync: SKIPPED - ${TARGETS_FILE} was not found."
      return
    fi
    echo "Targets config sync: NO - local config file not found: ${TARGETS_FILE}" >&2
    exit 1
  fi

  echo "Targets config: ${TARGETS_NAMESPACE}/${TARGETS_CONFIGMAP}:${TARGETS_KEY}"
  local_targets_config="$(cat "${TARGETS_FILE}")"
  if ! cluster_targets_config="$(kubectl get configmap "${TARGETS_CONFIGMAP}" \
    -n "${TARGETS_NAMESPACE}" \
    -o "go-template={{ index .data \"${TARGETS_KEY}\" }}" 2>&1)"; then
    echo "Targets config sync: NO - could not read the live ConfigMap." >&2
    echo "${cluster_targets_config}" >&2
    exit 1
  fi

  if [ "${cluster_targets_config}" != "${local_targets_config}" ]; then
    echo "Targets config sync: NO - ${TARGETS_FILE} differs from the live ConfigMap." >&2
    echo "       Run scripts/deploy-remediation-targets.sh to apply the local file." >&2
    exit 1
  fi

  echo "Targets config sync: YES - the live ConfigMap matches ${TARGETS_FILE}."
}

# rbac_can_i echoes "yes", "no", or "error" for a SubjectAccessReview run as
# the engine's ServiceAccount. "error" means the review could not be evaluated
# (typically the caller cannot impersonate), which is distinct from a denied
# "no". Extra args (e.g. --subresource=eviction) are passed through.
rbac_can_i() {
  local verb="$1"
  local resource="$2"
  shift 2
  local out
  if out="$(kubectl auth can-i "${verb}" "${resource}" "$@" \
    --as="${SA_USER}" -n "${NAMESPACE}" 2>/dev/null)"; then
    printf 'yes\n'
  elif [ "${out}" = "no" ]; then
    printf 'no\n'
  else
    printf 'error\n'
  fi
}

# verify_rbac_permissions confirms the deployed ServiceAccount can perform the
# pod verbs the engine relies on, including "delete pods" for the completed-pod
# GC and "create pods/eviction" for the nudger. This catches RBAC drift where
# an older manifests/rbac.yaml is live and a remediation path would silently
# 403 at runtime. When impersonation is unavailable it skips (auto) rather than
# failing, since not every kubeconfig can run a SubjectAccessReview as another
# subject.
verify_rbac_permissions() {
  local probe
  probe="$(rbac_can_i get pods)"
  if [ "${probe}" = "error" ]; then
    if [ "${VERIFY_RBAC}" = "auto" ]; then
      echo "RBAC check: SKIPPED - cannot run SubjectAccessReview as ${SA_USER} (impersonation unavailable)."
      return
    fi
    echo "RBAC check: ERROR - cannot evaluate permissions for ${SA_USER} (impersonation unavailable)." >&2
    exit 1
  fi

  echo "RBAC subject: ${SA_USER}"
  local failures=0
  local label verb resource extra verdict
  while IFS='|' read -r label verb resource extra; do
    [ -n "${label}" ] || continue
    if [ -n "${extra}" ]; then
      verdict="$(rbac_can_i "${verb}" "${resource}" "${extra}")"
    else
      verdict="$(rbac_can_i "${verb}" "${resource}")"
    fi
    if [ "${verdict}" = "yes" ]; then
      echo "  ${label}: yes"
    else
      echo "  ${label}: ${verdict}" >&2
      failures=$((failures + 1))
    fi
  done <<'PERMS'
get pods|get|pods|
list pods|list|pods|
watch pods|watch|pods|
delete pods (completed-pod GC)|delete|pods|
create pods/eviction (nudger)|create|pods|--subresource=eviction
PERMS

  if [ "${failures}" -gt 0 ]; then
    echo "RBAC check: NO - ${SA_USER} is missing ${failures} permission(s) the engine needs." >&2
    echo "       Apply the current role: scripts/apply-rbac.sh" >&2
    exit 1
  fi
  echo "RBAC check: YES - ${SA_USER} has the pod permissions the engine needs."
}

IMAGE_TAG=""
EXPECTING_LATEST=true
RUN_JOB=false
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"

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
      EXPECTING_LATEST=false
      shift
      ;;
  esac
done

require_command kubectl
require_command gh

GITHUB_REF="${GITHUB_REF:-main}"
NAMESPACE="${NAMESPACE:-cluster-optimizer}"
CRONJOB="${CRONJOB:-cluster-optimizer}"
IMAGE_NAME="${IMAGE_NAME:-ghcr.io/ryabinski-labs/cluster-optimizer}"
VERIFY_JOB="${VERIFY_JOB:-cluster-optimizer-deploy-verify}"
ENABLE_DYNAMODB="${ENABLE_DYNAMODB:-true}"
CLUSTER_ID="${CLUSTER_ID:-default}"
DYNAMODB_TABLE="${DYNAMODB_TABLE:-cluster-optimizer-reports}"
VERIFY_CONFIG="${VERIFY_CONFIG:-true}"
TARGETS_FILE="${TARGETS_FILE:-config/remediation-targets.json}"
TARGETS_NAMESPACE="${TARGETS_NAMESPACE:-${NAMESPACE}}"
TARGETS_CONFIGMAP="${TARGETS_CONFIGMAP:-cluster-optimizer-targets}"
TARGETS_KEY="${TARGETS_KEY:-remediation-targets.json}"
VERIFY_TARGETS_CONFIG="${VERIFY_TARGETS_CONFIG:-auto}"
SERVICE_ACCOUNT="${SERVICE_ACCOUNT:-cluster-optimizer}"
VERIFY_RBAC="${VERIFY_RBAC:-auto}"
SA_USER="system:serviceaccount:${NAMESPACE}:${SERVICE_ACCOUNT}"

case "${ENABLE_DYNAMODB}" in
  true|false) ;;
  *)
    echo "error: ENABLE_DYNAMODB must be 'true' or 'false'" >&2
    exit 2
    ;;
esac

case "${VERIFY_CONFIG}" in
  true|false) ;;
  *)
    echo "error: VERIFY_CONFIG must be 'true' or 'false'" >&2
    exit 2
    ;;
esac

case "${VERIFY_TARGETS_CONFIG}" in
  true|false|auto) ;;
  *)
    echo "error: VERIFY_TARGETS_CONFIG must be 'true', 'false', or 'auto'" >&2
    exit 2
    ;;
esac

case "${VERIFY_RBAC}" in
  true|false|auto) ;;
  *)
    echo "error: VERIFY_RBAC must be 'true', 'false', or 'auto'" >&2
    exit 2
    ;;
esac

if [ -z "${CONFIG_MANIFEST:-}" ]; then
  if [ "${ENABLE_DYNAMODB}" = "true" ]; then
    CONFIG_MANIFEST="examples/cronjob-dynamodb.yaml"
  else
    CONFIG_MANIFEST="manifests/cronjob.yaml"
  fi
fi

CONFIG_MANIFEST="$(resolve_local_path "${CONFIG_MANIFEST}")"
TARGETS_FILE="$(resolve_local_path "${TARGETS_FILE}")"

if [ -z "${IMAGE_TAG}" ]; then
  echo "Checking whether the latest published version is deployed..."
  echo "Finding the newest successfully published image for ${GITHUB_REF}..."
  IMAGE_TAG="$(latest_published_image_tag)"
else
  echo "Checking whether the requested image is deployed..."
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

if [ "${VERIFY_CONFIG}" = "true" ] && [ ! -f "${CONFIG_MANIFEST}" ]; then
  echo "error: expected config manifest not found: ${CONFIG_MANIFEST}" >&2
  exit 1
fi

actual_image="$(kubectl get cronjob "${CRONJOB}" -n "${NAMESPACE}" -o jsonpath='{.spec.jobTemplate.spec.template.spec.containers[0].image}')"
pull_policy="$(kubectl get cronjob "${CRONJOB}" -n "${NAMESPACE}" -o jsonpath='{.spec.jobTemplate.spec.template.spec.containers[0].imagePullPolicy}')"
last_success="$(kubectl get cronjob "${CRONJOB}" -n "${NAMESPACE}" -o jsonpath='{.status.lastSuccessfulTime}')"

echo "CronJob: ${NAMESPACE}/${CRONJOB}"
if [ "${EXPECTING_LATEST}" = "true" ]; then
  echo "Latest published image: ${EXPECTED_IMAGE}"
else
  echo "Expected image: ${EXPECTED_IMAGE}"
fi
echo "Currently deployed image: ${actual_image}"
echo "Pull policy: ${pull_policy:-<unset>}"
echo "Last successful schedule: ${last_success:-<none>}"
if [ "${VERIFY_CONFIG}" = "true" ]; then
  echo "Expected config manifest: ${CONFIG_MANIFEST}"
fi

if [ "${actual_image}" != "${EXPECTED_IMAGE}" ]; then
  if [ "${EXPECTING_LATEST}" = "true" ]; then
    echo "Result: NO - the latest published version is not deployed." >&2
  else
    echo "Result: NO - the requested image is not deployed." >&2
  fi
  exit 1
fi

if [ "${EXPECTING_LATEST}" = "true" ]; then
  echo "Result: YES - the latest published version is deployed."
else
  echo "Result: YES - the requested image is deployed."
fi

if [ "${VERIFY_CONFIG}" = "true" ]; then
  expected_manifest="$(mktemp)"
  trap 'rm -f "${expected_manifest:-}"' EXIT
  render_expected_cronjob_manifest "${CONFIG_MANIFEST}" >"${expected_manifest}"

  if config_diff="$(kubectl diff -f "${expected_manifest}" 2>&1)"; then
    echo "CronJob config sync: YES - the live CronJob matches the rendered repo manifest."
  else
    diff_status=$?
    if [ "${diff_status}" -eq 1 ]; then
      echo "CronJob config sync: NO - the live CronJob differs from the rendered repo manifest." >&2
      echo "${config_diff}" >&2
      exit 1
    fi
    echo "CronJob config sync: ERROR - kubectl diff failed." >&2
    echo "${config_diff}" >&2
    exit "${diff_status}"
  fi
fi

if [ "${VERIFY_TARGETS_CONFIG}" != "false" ]; then
  verify_targets_config_sync
fi

if [ "${VERIFY_RBAC}" != "false" ]; then
  verify_rbac_permissions
fi

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
  echo "Runtime check: FAILED - the verification job used ${job_image}, expected ${EXPECTED_IMAGE}." >&2
  exit 1
fi

echo "Runtime check: PASSED - the verification job completed with ${job_image}."
