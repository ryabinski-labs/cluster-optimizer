#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/deploy-kubernetes.sh [image-tag] [--wait] [--dry-run]
                                    [--no-live-apply] [--no-live-nudge] [--no-live-gc]

Triggers the existing GitHub Actions Kubernetes deploy workflow with an
immutable image tag. When no image tag is provided, the script deploys the
latest successful image built by the Publish Image workflow on GITHUB_REF.
This keeps deployment in CI/CD while making the correct workflow inputs easy
to repeat from a local shell.

Remediation mode is decided by two gates per remediation path: the CLI flag
baked into the manifest args (--auto-apply / --nudge / --gc-completed-pods)
AND the matching env var on the CronJob. The committed manifests always ship
the env vars as "false" so a bare `kubectl apply -f` stays advisory; this
script is the supported way to turn them on, and it turns ALL THREE ON BY
DEFAULT. A default deploy therefore leaves a cluster that patches workload
requests, cordons and evicts for consolidation, and deletes completed pods.

Use --dry-run to deploy every path advisory-only, or the per-path
--no-live-* flags to leave one path advisory while the others mutate. The
halt ConfigMap still overrides every gate regardless of what is deployed.

Live apply also needs manifests/rbac-applier.yaml on the cluster
(scripts/apply-rbac.sh --applier), or every patch attempt 403s at runtime.

Flags:
  --wait            Watch the triggered run to completion.
  --dry-run         Deploy with every live gate off (no mutations at all).
  --live-apply      Force CLUSTER_OPTIMIZER_AUTOAPPLY=true. On by default;
                    kept so an explicit request survives ENABLE_LIVE_APPLY=false.
  --no-live-apply   Leave the workload applier advisory.
  --live-nudge      Force CLUSTER_OPTIMIZER_NUDGE_LIVE=true. On by default.
  --no-live-nudge   Leave the consolidation nudger advisory.
  --live-gc         Force CLUSTER_OPTIMIZER_GC_COMPLETED_PODS_LIVE=true. On by
                    default.
  --no-live-gc      Leave completed-pod cleanup advisory.
  --help, -h        Show this message.

An explicit flag always wins over the matching environment variable, and the
environment variable wins over the live-by-default value.

Environment overrides:
  GITHUB_REF          Git ref to deploy from. Default: main
  CLUSTER_ID          Logical cluster id in reports. Default: default
  ENABLE_DYNAMODB     Persist reports to DynamoDB. Default: true
  DYNAMODB_TABLE      DynamoDB table name. Default: cluster-optimizer-reports
  AWS_REGION          AWS region for DynamoDB. Default: us-east-1
  DOKS_CLUSTER_ID     DigitalOcean Kubernetes cluster id.
                      Default: 7dc99f7c-e0b7-4402-81ae-0e9a1fedcd82
  ENABLE_LIVE_APPLY   Live workload patching. Default: true
  ENABLE_LIVE_NUDGE   Live cordon and eviction. Default: true
  ENABLE_LIVE_GC      Live completed-pod deletion. Default: true

Every live gate requires ENABLE_DYNAMODB=true: the stdout-only manifest
(manifests/cronjob.yaml) ships the gates commented out, so there is nothing
for the deploy to switch on. With ENABLE_DYNAMODB=false the defaults quietly
fall back to dry-run; an explicitly requested gate fails instead of lying.

Examples:
  scripts/deploy-kubernetes.sh --wait                  # live: apply + nudge + gc
  scripts/deploy-kubernetes.sh --wait --dry-run        # advisory only
  scripts/deploy-kubernetes.sh --wait --no-live-gc     # live apply + nudge
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
# Empty means "not chosen on the command line", which is what lets an explicit
# flag outrank ENABLE_LIVE_* and ENABLE_LIVE_* outrank the live-by-default.
LIVE_APPLY_FLAG=""
LIVE_NUDGE_FLAG=""
LIVE_GC_FLAG=""

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
    --dry-run)
      LIVE_APPLY_FLAG=false
      LIVE_NUDGE_FLAG=false
      LIVE_GC_FLAG=false
      shift
      ;;
    --live-apply)
      LIVE_APPLY_FLAG=true
      shift
      ;;
    --no-live-apply)
      LIVE_APPLY_FLAG=false
      shift
      ;;
    --live-nudge)
      LIVE_NUDGE_FLAG=true
      shift
      ;;
    --no-live-nudge)
      LIVE_NUDGE_FLAG=false
      shift
      ;;
    --live-gc)
      LIVE_GC_FLAG=true
      shift
      ;;
    --no-live-gc)
      LIVE_GC_FLAG=false
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

# Remember whether the live posture was asked for or merely inherited from the
# default, before resolution collapses the two into one value. The DynamoDB
# guard below needs the distinction: an explicit request that cannot be honored
# has to fail loudly, while the default can quietly fall back to dry-run.
APPLY_REQUESTED=false
NUDGE_REQUESTED=false
GC_REQUESTED=false
if [ "${LIVE_APPLY_FLAG}" = "true" ] || [ "${ENABLE_LIVE_APPLY:-}" = "true" ]; then
  APPLY_REQUESTED=true
fi
if [ "${LIVE_NUDGE_FLAG}" = "true" ] || [ "${ENABLE_LIVE_NUDGE:-}" = "true" ]; then
  NUDGE_REQUESTED=true
fi
if [ "${LIVE_GC_FLAG}" = "true" ] || [ "${ENABLE_LIVE_GC:-}" = "true" ]; then
  GC_REQUESTED=true
fi

# Precedence: explicit flag, then the env override, then live-by-default. A
# flag now downgrades as well as upgrades, so `--no-live-gc` is honored even
# with ENABLE_LIVE_GC=true exported in the shell.
if [ -n "${LIVE_APPLY_FLAG}" ]; then
  ENABLE_LIVE_APPLY="${LIVE_APPLY_FLAG}"
fi
if [ -n "${LIVE_NUDGE_FLAG}" ]; then
  ENABLE_LIVE_NUDGE="${LIVE_NUDGE_FLAG}"
fi
if [ -n "${LIVE_GC_FLAG}" ]; then
  ENABLE_LIVE_GC="${LIVE_GC_FLAG}"
fi
ENABLE_LIVE_APPLY="${ENABLE_LIVE_APPLY:-true}"
ENABLE_LIVE_NUDGE="${ENABLE_LIVE_NUDGE:-true}"
ENABLE_LIVE_GC="${ENABLE_LIVE_GC:-true}"

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

case "${ENABLE_LIVE_GC}" in
  true|false) ;;
  *)
    echo "error: ENABLE_LIVE_GC must be 'true' or 'false'" >&2
    exit 2
    ;;
esac

# Every live gate depends on DynamoDB, for two different reasons. The applier
# requires a finding to recur across 3 consecutive runs, which it cannot
# establish without persistence. The nudger and the pod GC have a mechanical
# dependency instead: manifests/cronjob.yaml (the ENABLE_DYNAMODB=false path)
# keeps their env vars commented out, and the workflow only rewrites the value
# of an env var that already exists.
#
# When live came from the default, downgrade and say so -- refusing to deploy
# at all would make `--dry-run`-equivalent stdout-only deploys impossible. When
# it was asked for explicitly, refuse: deploying dry-run while reporting
# success is exactly the misreport this posture reporting exists to prevent.
# Writes through the variable named by $1 rather than echoing, so the refusal
# below is a real `exit` and not one swallowed by a command substitution.
resolve_gate_against_dynamodb() {
  local var="$1" name="$2" requested="$3" why="$4"

  if [ "${!var}" != "true" ] || [ "${ENABLE_DYNAMODB}" = "true" ]; then
    return
  fi
  if [ "${requested}" = "true" ]; then
    echo "error: live ${name} requires ENABLE_DYNAMODB=true; ${why}" >&2
    exit 2
  fi
  echo "note: ENABLE_DYNAMODB=false, so live ${name} is not available; deploying it dry-run." >&2
  printf -v "${var}" 'false'
}

resolve_gate_against_dynamodb ENABLE_LIVE_APPLY apply "${APPLY_REQUESTED}" \
  "the applier has no run history without it"
resolve_gate_against_dynamodb ENABLE_LIVE_NUDGE nudge "${NUDGE_REQUESTED}" \
  "manifests/cronjob.yaml has no gate to turn on"
resolve_gate_against_dynamodb ENABLE_LIVE_GC "pod GC" "${GC_REQUESTED}" \
  "manifests/cronjob.yaml has no gate to turn on"

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
describe_gate "pod GC" "${ENABLE_LIVE_GC}" CLUSTER_OPTIMIZER_GC_COMPLETED_PODS_LIVE

# Name the paths that will still only report, so a partially live deploy never
# reads as fully live. The dashboard and verify-deployment.sh make the same
# distinction; this is the earliest point an operator can see it.
advisory=""
[ "${ENABLE_LIVE_APPLY}" = "true" ] || advisory="auto-apply"
[ "${ENABLE_LIVE_NUDGE}" = "true" ] || advisory="${advisory:+${advisory}, }nudge"
[ "${ENABLE_LIVE_GC}" = "true" ] || advisory="${advisory:+${advisory}, }pod GC"
if [ -n "${advisory}" ]; then
  echo "  not enabled (reports only): ${advisory}"
fi
if [ "${ENABLE_LIVE_APPLY}" = "true" ] || [ "${ENABLE_LIVE_NUDGE}" = "true" ] || [ "${ENABLE_LIVE_GC}" = "true" ]; then
  echo "  note: the cluster-optimizer/cluster-optimizer-halt ConfigMap still overrides every gate."
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
  -f "enable_live_nudge=${ENABLE_LIVE_NUDGE}" \
  -f "enable_live_gc=${ENABLE_LIVE_GC}"

if [ "${WAIT}" = "true" ]; then
  sleep 3
  run_id="$(gh run list --workflow "Deploy Kubernetes" --branch "${GITHUB_REF}" --limit 1 --json databaseId --jq '.[0].databaseId')"
  if [ -z "${run_id}" ]; then
    echo "error: could not find the triggered Deploy Kubernetes run" >&2
    exit 1
  fi
  gh run watch "${run_id}" --exit-status
fi
