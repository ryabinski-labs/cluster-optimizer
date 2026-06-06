#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: scripts/apply-rbac.sh [--dry-run]

Applies manifests/rbac.yaml (Namespace, ServiceAccount, ClusterRole, and
ClusterRoleBinding) to the cluster your kubectl is pointed at. This grants the
cluster-optimizer ServiceAccount the permissions the engine needs, including
"delete pods" for the opt-in completed-pod GC (--gc-completed-pods).

Run this whenever scripts/verify-deployment.sh reports the ServiceAccount is
missing a permission (RBAC drift), e.g.:

  delete pods (completed-pod GC): no

The cluster-optimizer ClusterRole/ClusterRoleBinding were renamed from
"cluster-optimizer-readonly" to "cluster-optimizer-engine" (the role is no
longer read-only). This script removes the old objects after applying the new
ones so a renamed apply does not leave them orphaned on the cluster.

Environment overrides:
  KUBECTL        kubectl binary to use. Default: kubectl
  MANIFEST       RBAC manifest to apply. Default: manifests/rbac.yaml
  LEGACY_ROLE    Old ClusterRole/ClusterRoleBinding name to clean up.
                 Default: cluster-optimizer-readonly

Flags:
  --dry-run      Show what would be applied and removed without changing
                 anything. Safe to run with any kubeconfig.
  --help, -h     Show this message.

Examples:
  scripts/apply-rbac.sh --dry-run
  scripts/apply-rbac.sh
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

DRY_RUN=false
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"

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
    *)
      echo "error: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

KUBECTL="${KUBECTL:-kubectl}"
MANIFEST="${MANIFEST:-manifests/rbac.yaml}"
LEGACY_ROLE="${LEGACY_ROLE:-cluster-optimizer-readonly}"

require_command "${KUBECTL}"

MANIFEST="$(resolve_local_path "${MANIFEST}")"
if [ ! -f "${MANIFEST}" ]; then
  echo "error: RBAC manifest not found: ${MANIFEST}" >&2
  exit 1
fi

if [ "${DRY_RUN}" = "true" ]; then
  echo "# dry-run: would apply ${MANIFEST}"
  "${KUBECTL}" apply -f "${MANIFEST}" --dry-run=client
  echo "# dry-run: would remove legacy objects (if present):" >&2
  echo "#   clusterrolebinding/${LEGACY_ROLE}" >&2
  echo "#   clusterrole/${LEGACY_ROLE}" >&2
  echo "# dry-run: nothing was applied." >&2
  exit 0
fi

"${KUBECTL}" apply -f "${MANIFEST}"

# Remove the pre-rename objects so the renamed role does not leave an orphan
# ClusterRole/ClusterRoleBinding behind. Idempotent: a no-op once cleaned up.
"${KUBECTL}" delete clusterrolebinding "${LEGACY_ROLE}" --ignore-not-found
"${KUBECTL}" delete clusterrole "${LEGACY_ROLE}" --ignore-not-found

echo "RBAC applied. The cluster-optimizer ServiceAccount now has the engine permissions."
echo "Verify with: scripts/verify-deployment.sh (or kubectl auth can-i delete pods \\"
echo "  --as=system:serviceaccount:cluster-optimizer:cluster-optimizer -n cluster-optimizer)"
