#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/deploy-remediation-targets.sh [--dry-run] [--trigger-job]

Uploads config/remediation-targets.json to the cluster-optimizer-targets
ConfigMap that the in-cluster CronJob mounts at /etc/cluster-optimizer.
The file is gitignored on purpose (it holds private repo and workload
mappings), so the regular Deploy Kubernetes workflow cannot ship it for
you. Run this script from a workstation that has kubectl pointed at the
target cluster.

The command is idempotent: it generates the ConfigMap YAML client-side and
applies it, so existing keys are replaced and missing keys are removed.

Environment overrides:
  TARGETS_FILE        Source JSON file.
                      Default: config/remediation-targets.json
  TARGETS_NAMESPACE   Kubernetes namespace that owns the CronJob.
                      Default: cluster-optimizer
  TARGETS_CONFIGMAP   ConfigMap name expected by the CronJob mount.
                      Default: cluster-optimizer-targets
  TARGETS_KEY         File key inside the ConfigMap (matches mount path).
                      Default: remediation-targets.json
  KUBECTL             kubectl binary to use. Default: kubectl

Flags:
  --dry-run           Print the generated ConfigMap YAML and exit without
                      applying it. Safe to run with any kubeconfig.
  --trigger-job       After applying, create a one-off Job from the
                      CronJob template so you do not have to wait for the
                      next scheduled run. Requires the CronJob to exist
                      in TARGETS_NAMESPACE.
  --help, -h          Show this message.

Examples:
  scripts/deploy-remediation-targets.sh --dry-run
  scripts/deploy-remediation-targets.sh
  scripts/deploy-remediation-targets.sh --trigger-job
EOF
}

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "error: required command '$1' was not found" >&2
    exit 127
  fi
}

DRY_RUN=false
TRIGGER_JOB=false

while [ "$#" -gt 0 ]; do
  case "$1" in
    --help|-h)
      usage
      exit 0
      ;;
    --dry-run)
      DRY_RUN=true
      shift
      ;;
    --trigger-job)
      TRIGGER_JOB=true
      shift
      ;;
    *)
      echo "error: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

TARGETS_FILE="${TARGETS_FILE:-config/remediation-targets.json}"
TARGETS_NAMESPACE="${TARGETS_NAMESPACE:-cluster-optimizer}"
TARGETS_CONFIGMAP="${TARGETS_CONFIGMAP:-cluster-optimizer-targets}"
TARGETS_KEY="${TARGETS_KEY:-remediation-targets.json}"
KUBECTL="${KUBECTL:-kubectl}"

require_command "${KUBECTL}"

if [ ! -f "${TARGETS_FILE}" ]; then
  echo "error: targets file not found: ${TARGETS_FILE}" >&2
  echo "       cp config/remediation-targets.example.json ${TARGETS_FILE} and edit it first." >&2
  exit 1
fi

# Validate JSON before sending it to the cluster.
if command -v python3 >/dev/null 2>&1; then
  if ! python3 -c "import json,sys; json.load(open(sys.argv[1]))" "${TARGETS_FILE}" >/dev/null 2>&1; then
    echo "error: ${TARGETS_FILE} is not valid JSON" >&2
    exit 1
  fi
fi

CONFIGMAP_YAML="$(
  "${KUBECTL}" create configmap "${TARGETS_CONFIGMAP}" \
    --namespace "${TARGETS_NAMESPACE}" \
    --from-file="${TARGETS_KEY}=${TARGETS_FILE}" \
    --dry-run=client \
    --output yaml
)"

if [ "${DRY_RUN}" = "true" ]; then
  echo "${CONFIGMAP_YAML}"
  echo "# dry-run: nothing was applied." >&2
  exit 0
fi

echo "${CONFIGMAP_YAML}" | "${KUBECTL}" apply -f -

if [ "${TRIGGER_JOB}" = "true" ]; then
  if ! "${KUBECTL}" get cronjob cluster-optimizer -n "${TARGETS_NAMESPACE}" >/dev/null 2>&1; then
    echo "error: CronJob cluster-optimizer not found in namespace ${TARGETS_NAMESPACE}." >&2
    echo "       Skipping --trigger-job. Apply manifests/cronjob.yaml (or examples/cronjob-dynamodb.yaml) first." >&2
    exit 1
  fi
  job_name="cluster-optimizer-manual-$(date +%s)"
  "${KUBECTL}" create job "${job_name}" \
    --namespace "${TARGETS_NAMESPACE}" \
    --from "cronjob/cluster-optimizer"
  echo "Created one-off Job: ${job_name}"
  echo "Follow logs with: ${KUBECTL} -n ${TARGETS_NAMESPACE} logs -f job/${job_name}"
fi
