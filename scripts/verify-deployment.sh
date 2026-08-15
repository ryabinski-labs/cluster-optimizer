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

The remediation-mode report reads the CronJob, so it describes the next run.
It closes with the posture the most recent run actually used, since a CronJob
update only reaches Jobs created after it — until the next tick the last run's
logs still show the previous posture. --run-job creates a run that uses the
current one immediately (on a live cluster, that run mutates workloads).

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
  ENABLE_LIVE_APPLY
                 Expected CLUSTER_OPTIMIZER_AUTOAPPLY value. Set true when the
                 cluster was deployed with scripts/deploy-kubernetes.sh
                 --live-apply, otherwise the config diff reports drift.
                 Default: false
  ENABLE_LIVE_NUDGE
                 Expected CLUSTER_OPTIMIZER_NUDGE_LIVE value. Set true when the
                 cluster was deployed with --live-nudge. Default: false
  HALT_CONFIGMAP Halt kill-switch ConfigMap name.
                 Default: cluster-optimizer-halt
  HALT_KEY       Key read from the halt ConfigMap. Default: halt
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
  ENABLE_LIVE_APPLY=true ENABLE_LIVE_NUDGE=true scripts/verify-deployment.sh
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

# kubectl_local runs a --local kubectl mutation over the manifest streamed on
# stdin and echoes the result. kubectl (confirmed on v1.35) prints NOTHING and
# exits 0 when the requested value is already in effect — which is the common
# case here, since CLUSTER_ID defaults to "default" and DYNAMODB_TABLE defaults
# to the value the manifest already carries. Piping that straight into the next
# stage empties the whole rendered manifest and turns the config-sync check
# into a vacuous pass, so fall back to the unchanged input.
kubectl_local() {
  local input result
  input="$(cat)"
  result="$(printf '%s\n' "${input}" | kubectl "$@" --local -f - -o yaml)"
  printf '%s\n' "${result:-${input}}"
}

# set_gate_env rewrites a live-remediation gate on the manifest streamed on
# stdin, but only when that manifest already declares the variable. The deploy
# workflow rewrites the value of an existing env entry; it never adds one. In
# manifests/cronjob.yaml both gates are commented out, so setting them here
# would invent an env var the live CronJob cannot have and report drift that
# does not exist.
set_gate_env() {
  local name="$1"
  local value="$2"
  local rendered
  rendered="$(cat)"

  if printf '%s\n' "${rendered}" | grep -q "name: ${name}$"; then
    printf '%s\n' "${rendered}" | kubectl_local set env "${name}=${value}"
  else
    printf '%s\n' "${rendered}"
  fi
}

render_expected_cronjob_manifest() {
  local manifest="$1"

  # Mirrors the deploy workflow's render: image, cluster id, table, and the
  # two live-remediation gates it rewrites at deploy time. The committed
  # manifests always carry "false", so a cluster deployed with
  # --live-apply/--live-nudge only matches when the same expectation is
  # declared here.
  if [ "${ENABLE_DYNAMODB}" = "true" ]; then
    kubectl patch --local -f "${manifest}" --type=merge \
      -p "{\"metadata\":{\"name\":\"${CRONJOB}\",\"namespace\":\"${NAMESPACE}\"}}" \
      -o yaml |
      kubectl_local set image "optimizer=${EXPECTED_IMAGE}" |
      kubectl_local set env "CLUSTER_OPTIMIZER_CLUSTER_ID=${CLUSTER_ID}" |
      kubectl_local set env "DYNAMODB_TABLE=${DYNAMODB_TABLE}" |
      set_gate_env CLUSTER_OPTIMIZER_AUTOAPPLY "${ENABLE_LIVE_APPLY}" |
      set_gate_env CLUSTER_OPTIMIZER_NUDGE_LIVE "${ENABLE_LIVE_NUDGE}"
  else
    kubectl patch --local -f "${manifest}" --type=merge \
      -p "{\"metadata\":{\"name\":\"${CRONJOB}\",\"namespace\":\"${NAMESPACE}\"}}" \
      -o yaml |
      kubectl_local set image "optimizer=${EXPECTED_IMAGE}" |
      kubectl_local set env "CLUSTER_OPTIMIZER_CLUSTER_ID=${CLUSTER_ID}" |
      set_gate_env CLUSTER_OPTIMIZER_AUTOAPPLY "${ENABLE_LIVE_APPLY}" |
      set_gate_env CLUSTER_OPTIMIZER_NUDGE_LIVE "${ENABLE_LIVE_NUDGE}"
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

# The remediation gates live on the pod template's single container, which
# sits at a different path on a CronJob than on the Jobs it creates. Reading
# both through the same helpers is what lets the posture the CronJob will use
# next be compared against the posture the last run actually used.
CRONJOB_CONTAINER='.spec.jobTemplate.spec.template.spec.containers[0]'
JOB_CONTAINER='.spec.template.spec.containers[0]'

container_arg_present() {
  local resource="$1" path="$2" arg="$3"
  local args

  # Padded so the glob below matches whole arguments only, and compared with
  # case rather than a grep pipeline: under `set -o pipefail` a `grep -q` that
  # exits on its first match can leave the pipeline reporting the SIGPIPE of
  # an upstream stage, which would read as "argument absent".
  args=" $(kubectl get "${resource}" -n "${NAMESPACE}" -o "jsonpath={${path}.args[*]}" 2>/dev/null) "
  case "${args}" in
    *" ${arg} "*) return 0 ;;
    *) return 1 ;;
  esac
}

container_env_value() {
  local resource="$1" path="$2" name="$3"
  kubectl get "${resource}" -n "${NAMESPACE}" \
    -o "jsonpath={${path}.env[?(@.name=='${name}')].value}" 2>/dev/null
}

# gate_state echoes a container spec's four remediation gate inputs as
# "apply_flag apply_env nudge_flag nudge_env", each "true" or "false".
gate_state() {
  local resource="$1" path="$2"
  local apply_flag=false apply_env=false nudge_flag=false nudge_env=false

  if container_arg_present "${resource}" "${path}" --auto-apply; then apply_flag=true; fi
  if [ "$(container_env_value "${resource}" "${path}" CLUSTER_OPTIMIZER_AUTOAPPLY)" = "true" ]; then apply_env=true; fi
  if container_arg_present "${resource}" "${path}" --nudge; then nudge_flag=true; fi
  if [ "$(container_env_value "${resource}" "${path}" CLUSTER_OPTIMIZER_NUDGE_LIVE)" = "true" ]; then nudge_env=true; fi

  printf '%s %s %s %s\n' "${apply_flag}" "${apply_env}" "${nudge_flag}" "${nudge_env}"
}

# effective_mode echoes the live remediation paths a gate_state line implies,
# or "dry-run". A path mutates only when its CLI flag AND its env gate are
# both true; either one alone keeps it advisory.
effective_mode() {
  local apply_flag="$1" apply_env="$2" nudge_flag="$3" nudge_env="$4"
  local live=""

  if [ "${apply_flag}" = "true" ] && [ "${apply_env}" = "true" ]; then
    live="auto-apply"
  fi
  if [ "${nudge_flag}" = "true" ] && [ "${nudge_env}" = "true" ]; then
    live="${live:+${live}, }nudge"
  fi
  printf '%s\n' "${live:-dry-run}"
}

describe_mode() {
  if [ "$1" = "dry-run" ]; then
    printf 'dry-run\n'
  else
    printf 'LIVE (%s)\n' "$1"
  fi
}

# latest_cronjob_run echoes "<job-name> <creationTimestamp>" for the most
# recently created Job this CronJob owns, or nothing when the history limits
# have pruned them all. `kubectl create job --from=cronjob/...` sets the same
# ownerReference, so a --run-job verification counts as a run here too.
latest_cronjob_run() {
  kubectl get jobs -n "${NAMESPACE}" \
    --sort-by=.metadata.creationTimestamp \
    -o go-template="{{range .items}}{{\$job := .}}{{range .metadata.ownerReferences}}{{if and (eq .kind \"CronJob\") (eq .name \"${CRONJOB}\")}}{{\$job.metadata.name}} {{\$job.metadata.creationTimestamp}}{{\"\n\"}}{{end}}{{end}}{{end}}" \
    2>/dev/null | tail -1
}

gate_row() {
  local label="$1"
  local ok="$2"
  local hint="$3"

  if [ "${ok}" = "true" ]; then
    echo "  ${label}: yes - ${hint}"
  else
    echo "  ${label}: no  - ${hint}"
  fi
}

# report_last_run_mode reconciles the posture reported above -- which is read
# off the CronJob, and so describes the NEXT run -- with the posture the most
# recent run actually used. A CronJob update only reaches Jobs created after
# it, so for up to one schedule interval the two disagree: the block above can
# read LIVE while every run an operator can inspect is still dry-run, and the
# logs appear to contradict the report. Say which one the logs are showing
# instead of leaving that to be worked out. Informational, like the block
# above: a posture that has not taken effect yet is not a failure.
report_last_run_mode() {
  local expected="$1"
  local latest job_name job_created actual schedule
  local apply_flag apply_env nudge_flag nudge_env

  latest="$(latest_cronjob_run)" || latest=""
  if [ -z "${latest}" ]; then
    echo "Last run: none - no Job from this CronJob is left in history."
    return
  fi

  read -r job_name job_created <<<"${latest}"

  # The history limits can prune this Job between the listing above and the
  # gate reads below, and an unreadable spec reads as all-gates-false. Probing
  # first keeps that from being reported as "ran dry-run", which would claim a
  # posture had not taken effect when it may well have -- the same misleading
  # conclusion this whole report exists to prevent.
  if ! kubectl get "job/${job_name}" -n "${NAMESPACE}" \
    -o "jsonpath={${JOB_CONTAINER}.image}" >/dev/null 2>&1; then
    echo "Last run: ${job_name} (${job_created}) - its spec is gone, so its posture cannot be read."
    return
  fi

  read -r apply_flag apply_env nudge_flag nudge_env \
    <<<"$(gate_state "job/${job_name}" "${JOB_CONTAINER}")"
  actual="$(effective_mode "${apply_flag}" "${apply_env}" "${nudge_flag}" "${nudge_env}")"

  if [ "${actual}" = "${expected}" ]; then
    echo "Last run: ${job_name} (${job_created}) ran with this posture."
    return
  fi

  schedule="$(kubectl get cronjob "${CRONJOB}" -n "${NAMESPACE}" -o jsonpath='{.spec.schedule}' 2>/dev/null)"
  echo "Last run: ${job_name} (${job_created}) ran $(describe_mode "${actual}"), not $(describe_mode "${expected}")."
  echo "       A CronJob update only reaches Jobs created after it, so the posture above"
  echo "       first takes effect on the next scheduled run (${schedule:-unknown schedule})."
  echo "       To exercise it now: scripts/verify-deployment.sh --run-job"
}

# report_remediation_mode prints the same decision the UI's "How remediation
# mode is decided" popover shows, read from the live CronJob rather than from
# a report. A path mutates only when its CLI flag AND its env gate are both
# true and the halt ConfigMap is not set; either gate alone keeps it dry-run.
# This is a report, not an assertion: dry-run is a valid posture, so it never
# fails the verification.
report_remediation_mode() {
  local apply_flag apply_env nudge_flag nudge_env
  local halt_value halt_active=false

  read -r apply_flag apply_env nudge_flag nudge_env \
    <<<"$(gate_state "cronjob/${CRONJOB}" "${CRONJOB_CONTAINER}")"

  if halt_value="$(kubectl get configmap "${HALT_CONFIGMAP}" -n "${NAMESPACE}" \
    -o "jsonpath={.data.${HALT_KEY}}" 2>/dev/null)"; then
    if [ "${halt_value}" = "true" ]; then halt_active=true; fi
  fi

  echo "Remediation mode: how it is decided"
  gate_row "auto-apply flag" "${apply_flag}" \
    "$([ "${apply_flag}" = "true" ] && echo "--auto-apply is passed" || echo "--auto-apply not set on the CronJob")"
  gate_row "auto-apply env " "${apply_env}" \
    "$([ "${apply_env}" = "true" ] && echo "CLUSTER_OPTIMIZER_AUTOAPPLY=true" || echo "CLUSTER_OPTIMIZER_AUTOAPPLY is not true")"
  gate_row "nudge flag     " "${nudge_flag}" \
    "$([ "${nudge_flag}" = "true" ] && echo "--nudge is passed" || echo "--nudge not set on the CronJob")"
  gate_row "nudge live env " "${nudge_env}" \
    "$([ "${nudge_env}" = "true" ] && echo "CLUSTER_OPTIMIZER_NUDGE_LIVE=true" || echo "CLUSTER_OPTIMIZER_NUDGE_LIVE is not true")"
  gate_row "halt ConfigMap " "$([ "${halt_active}" = "true" ] && echo false || echo true)" \
    "$([ "${halt_active}" = "true" ] && echo "halted: ${NAMESPACE}/${HALT_CONFIGMAP} ${HALT_KEY}=true" || echo "no halt detected")"

  if [ "${halt_active}" = "true" ]; then
    # Halt is read by the engine at run time, not baked into the pod spec, so
    # it needs no last-run reconciliation: it applies from the next run on
    # without a redeploy.
    echo "Effective mode: HALTED - the halt ConfigMap overrides both gates; nothing mutates."
    return
  fi

  EFFECTIVE_MODE="$(effective_mode "${apply_flag}" "${apply_env}" "${nudge_flag}" "${nudge_env}")"
  if [ "${EFFECTIVE_MODE}" = "dry-run" ]; then
    echo "Effective mode: dry-run - findings are reported, nothing is mutated."
    echo "       To go live: scripts/deploy-kubernetes.sh --live-apply [--live-nudge]"
  else
    echo "Effective mode: LIVE (${EFFECTIVE_MODE}) - this cluster mutates workloads."
  fi

  report_last_run_mode "${EFFECTIVE_MODE}"
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
ENABLE_LIVE_APPLY="${ENABLE_LIVE_APPLY:-false}"
ENABLE_LIVE_NUDGE="${ENABLE_LIVE_NUDGE:-false}"
HALT_CONFIGMAP="${HALT_CONFIGMAP:-cluster-optimizer-halt}"
HALT_KEY="${HALT_KEY:-halt}"
SA_USER="system:serviceaccount:${NAMESPACE}:${SERVICE_ACCOUNT}"
# Set by report_remediation_mode to the CronJob's effective mode, so the
# --run-job path can report whether the job it just ran exercised it. Stays
# empty when the halt switch short-circuits the report.
EFFECTIVE_MODE=""

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
  echo "Expected live gates: CLUSTER_OPTIMIZER_AUTOAPPLY=${ENABLE_LIVE_APPLY} CLUSTER_OPTIMIZER_NUDGE_LIVE=${ENABLE_LIVE_NUDGE}"
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

report_remediation_mode

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

# The job just created is now the most recent run, so this closes the loop the
# staleness warning above points at: it is the run that used the CronJob's
# current posture.
if [ -n "${EFFECTIVE_MODE}" ]; then
  report_last_run_mode "${EFFECTIVE_MODE}"
fi
