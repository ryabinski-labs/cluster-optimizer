# Operator Runbook

Recovery and operational procedures for the Cluster Optimizer in-cluster
deployment. The optimizer is advisory by default; this runbook is mostly
relevant when the optional live-apply or live-nudger features are enabled.

## Quick reference

| Symptom                                                  | First action                                  |
|----------------------------------------------------------|-----------------------------------------------|
| Need to stop ALL future mutations right now              | [Activate the halt switch](#activate-the-halt-switch) |
| A workload was trimmed too far and is unhealthy          | [Roll back a single workload patch](#roll-back-a-single-workload-patch) |
| A node was cordoned and won't accept new pods            | [Uncordon a node](#uncordon-a-node)           |
| The CronJob is misbehaving and you want it off entirely  | [Suspend the CronJob](#suspend-the-cronjob)   |
| Operator wants to revoke live-apply capability           | [Revoke applier RBAC](#revoke-applier-rbac)   |
| Investigating what the optimizer did                     | [Read the recent run logs](#read-the-recent-run-logs) |
| Want to validate behaviour without risk                  | [Return to dry-run mode](#return-to-dry-run-mode) |
| Edited `remediation-targets.json` and need it in cluster | [Update the targets ConfigMap](#update-the-targets-configmap) |

---

## Activate the halt switch

The halt switch is the fastest way to stop the optimizer from making any new
changes. It is read at the start of every mutation pass; setting it stops both
the live applier and the live nudger.

```bash
kubectl -n cluster-optimizer create configmap cluster-optimizer-halt \
  --from-literal=halt=true \
  --dry-run=client -o yaml | kubectl apply -f -
```

Verify:

```bash
kubectl -n cluster-optimizer get configmap cluster-optimizer-halt -o jsonpath='{.data.halt}{"\n"}'
# → true
```

A halted run logs `Applier: halt configmap set (halt=true), refusing to apply`
in the CronJob logs and produces no patches.

**Reverse when ready:**

```bash
kubectl -n cluster-optimizer delete configmap cluster-optimizer-halt
# or, to leave the ConfigMap in place:
kubectl -n cluster-optimizer patch configmap cluster-optimizer-halt --type merge -p '{"data":{"halt":"false"}}'
```

**Fail-closed behaviour:** if the halt ConfigMap exists but the optimizer's
ServiceAccount cannot read it (e.g. RBAC was revoked), the applier treats this
as a halted state and refuses to mutate. You can deliberately remove the
`cluster-optimizer-halt-reader` RoleBinding as a "deny-everything" lever.

## Roll back a single workload patch

The applier patches with field manager `cluster-optimizer-applier`. The
recommended rollback is to reassert your source manifest, which uses a
different field manager and will overwrite our values cleanly.

**Preferred — apply your source manifest:**

```bash
# from the workload's own repo
kubectl apply -f path/to/manifest.yaml
kubectl rollout status deployment/<name> -n <namespace>
```

**Alternative — restart with the current spec, after editing requests inline:**

```bash
kubectl set resources deployment/<name> -n <namespace> \
  --containers=<container> --requests=memory=512Mi,cpu=200m
kubectl rollout status deployment/<name> -n <namespace>
```

**Find what the optimizer changed:**

```bash
# Most recent reports include the planned actions; the applier logs each
# applied change with the before → after values.
kubectl -n cluster-optimizer logs -l app.kubernetes.io/name=cluster-optimizer --tail=200 | grep "Applier LIVE"
```

If DynamoDB persistence is enabled, every applied action is also stored on
the report row for that run.

## Uncordon a node

If the nudger cordoned a node and you want it schedulable again:

```bash
kubectl get nodes
kubectl uncordon <node-name>
```

The nudger only cordons; it never deletes nodes. A cordoned-and-empty node
will be picked up by the DOKS autoscaler if you have one enabled. If you do
not run an autoscaler, uncordoning is the right action.

### Which cordons are the optimizer's?

Cordons the nudger placed carry ownership annotations. To list them:

```bash
kubectl get nodes \
  -o custom-columns=NODE:.metadata.name,\
UNSCHEDULABLE:.spec.unschedulable,\
CORDONED_AT:.metadata.annotations.cluster-optimizer\\.io/cordoned-at,\
BY_RUN:.metadata.annotations.cluster-optimizer\\.io/cordoned-by-run
```

A node with `CORDONED_AT` set is ours. A node cordoned with no such annotation
was cordoned by a human or another controller, and the optimizer will never
reverse it.

### Stale cordons reverse themselves

You usually do not need to uncordon by hand. Each run reverses cordons the
optimizer placed that have outlived `CLUSTER_OPTIMIZER_CORDON_TTL` (default
`30m`) — this covers both a run that died mid-drain and a drain that completed
but that nothing ever acted on. The node's pre-cordon schedulability is
restored and an `uncordon_stale` row appears in the remediation feed.

Two cases where you should still intervene:

- **The halt switch is set.** Halt means the optimizer touches nothing,
  including the reaper. Uncordon by hand, or clear the halt.
- **`uncordon_stale` rows keep appearing for the same node.** Consolidation is
  working and nothing is removing the drained node. Either enable an
  autoscaler, remove the node yourself, or turn nudging off — as it stands the
  cluster is being drained and refilled for no saving.

To disable reaping entirely and restore the previous
cordon-and-leave-it behaviour, set `CLUSTER_OPTIMIZER_CORDON_TTL=0`.

## Suspend the CronJob

To stop the optimizer from running on its schedule without uninstalling:

```bash
kubectl -n cluster-optimizer patch cronjob cluster-optimizer \
  --type merge -p '{"spec":{"suspend":true}}'
```

In-flight Jobs continue to completion. Reverse with `"suspend":false`.

## Revoke applier RBAC

To strip the optimizer's ability to patch workloads even if both auto-apply
gates remain set:

```bash
kubectl delete -f manifests/rbac-applier.yaml
```

This removes the Role/RoleBinding for `patch` on Deployments/DaemonSets/
StatefulSets in `default`, and the read access to the halt ConfigMap. The
applier will then see RBAC-forbidden errors on its patch attempts (logged,
non-fatal) and fail-closed on the halt check (treated as halted).

## Read the recent run logs

```bash
# Most recent run:
kubectl -n cluster-optimizer get jobs --sort-by=.metadata.creationTimestamp \
  -o jsonpath='{.items[-1].metadata.name}{"\n"}' | xargs -I{} kubectl -n cluster-optimizer logs job/{}

# All runs in the retained history:
kubectl -n cluster-optimizer get jobs
```

Look for:

- `Applier DRY-RUN:` lines describe what *would* happen if both gates were on.
- `Applier LIVE:` lines describe applied changes (before → after).
- `Applier: halt configmap set (...), refusing to apply` confirms halt is active.
- `Active Nudger (DRY-RUN/LIVE):` describes consolidation plans.

## Go live (or back to dry-run) via the deploy workflow

The committed manifests always ship with the live gates set to `"false"`, so
a plain `kubectl apply -f` is advisory no matter which manifest is used.
Turning a gate on is a deploy-time decision, made through `Deploy Kubernetes`
(`workflow_dispatch`):

| Input | Effect when `true` |
|---|---|
| `enable_live_apply` | Sets `CLUSTER_OPTIMIZER_AUTOAPPLY=true`. Requires `enable_dynamodb=true` — the workflow fails fast otherwise, because the applier cannot establish the 3-consecutive-run history without persistence. |
| `enable_live_nudge` | Sets `CLUSTER_OPTIMIZER_NUDGE_LIVE=true`, allowing the nudger to cordon a node and evict its relocatable pods. |

Both default to `false`, so a re-deploy that does not set them explicitly
returns the cluster to dry-run. The `Show deployment` step prints the gate
values it actually deployed. The `--auto-apply` / `--nudge` args and the halt
ConfigMap are unaffected by these inputs; the halt switch still overrides both.

From a maintainer workstation, `scripts/deploy-kubernetes.sh` sets both inputs:

```bash
# Grant the applier its narrow patch permissions first, or every patch 403s.
scripts/apply-rbac.sh --applier

# Deploy live. Omitting the flags on the next deploy returns it to dry-run.
scripts/deploy-kubernetes.sh --live-apply --live-nudge --wait

# Confirm the posture that actually landed.
ENABLE_LIVE_APPLY=true ENABLE_LIVE_NUDGE=true scripts/verify-deployment.sh
```

The script refuses `--live-apply` / `--live-nudge` when `ENABLE_DYNAMODB=false`.
For apply that mirrors the workflow's own check; for nudge it is stricter,
because `manifests/cronjob.yaml` keeps both gates commented out and the
workflow only rewrites an env var that already exists — asking for a live
nudge there would deploy dry-run while reporting success.

`verify-deployment.sh` renders the expected CronJob with the same gate values,
so tell it which posture you expect (`ENABLE_LIVE_APPLY` / `ENABLE_LIVE_NUDGE`,
both defaulting to `false`) or it reports the live gates as config drift. It
also prints a `Remediation mode: how it is decided` block — the same five
checks the UI popover shows, read from the live CronJob — and the effective
mode they add up to. That block is informational; dry-run is a valid posture,
so it never fails the verification.

`CLUSTER_OPTIMIZER_GC_COMPLETED_PODS_LIVE` has no workflow input yet — it is
still enabled by editing the manifest or patching the CronJob directly.

## Return to dry-run mode

Fastest rollback is a re-deploy with both inputs `false`. To flip the running
CronJob immediately without waiting on a workflow run:

```bash
# Remove either gate; both must be true to mutate.
kubectl -n cluster-optimizer patch cronjob cluster-optimizer --type json \
  -p '[{"op":"remove","path":"/spec/jobTemplate/spec/template/spec/containers/0/env/<index-of-CLUSTER_OPTIMIZER_AUTOAPPLY>"}]'

# Or drop --auto-apply from args:
kubectl -n cluster-optimizer edit cronjob cluster-optimizer
# then remove "--auto-apply" from .spec.jobTemplate.spec.template.spec.containers[0].args
```

The next run will be dry-run only.

## Update the targets ConfigMap

The CronJob loads `config/remediation-targets.json` from a ConfigMap
(`cluster-optimizer-targets` in the `cluster-optimizer` namespace) that
is mounted at `/etc/cluster-optimizer/remediation-targets.json`. The
source file is gitignored, so the `Deploy Kubernetes` workflow does not
ship it; updates are pushed from a maintainer workstation with kubectl
pointed at the cluster.

**Preview without applying:**

```bash
scripts/deploy-remediation-targets.sh --dry-run
```

Prints the generated ConfigMap YAML to stdout and exits without touching
the cluster. Safe to run with any kubeconfig.

**Apply:**

```bash
scripts/deploy-remediation-targets.sh
# → configmap/cluster-optimizer-targets configured
```

The script is idempotent: it builds the ConfigMap client-side and pipes
through `kubectl apply -f -`, so removed entries are removed and changed
entries are replaced. The next CronJob firing picks up the new file.

**Apply and force a run immediately:**

```bash
scripts/deploy-remediation-targets.sh --trigger-job
```

Creates a one-off Job named `cluster-optimizer-manual-<unix-ts>` from
the CronJob template so you do not have to wait for the next scheduled
tick. Requires the `cluster-optimizer` CronJob to exist.

**Verify the cluster matches your local file:**

```bash
kubectl -n cluster-optimizer get configmap cluster-optimizer-targets \
  -o jsonpath='{.data.remediation-targets\.json}' \
  | diff - config/remediation-targets.json && echo "in sync"
```

A clean `diff` with `in sync` printed means the cluster is current.

**Roll back to the previous version:**

The script applies whatever your local file contains, so the rollback
path is: restore the previous file content locally (from git history of
a private vault, a backup, or by reverting your edits), then run the
script again.

If you do not have the previous content handy and a backup of the
ConfigMap exists, restore from that:

```bash
# When you captured a backup before the change:
#   kubectl -n cluster-optimizer get configmap cluster-optimizer-targets -o yaml > targets-backup.yaml
kubectl apply -f targets-backup.yaml
```

If neither is available, the safest fallback is to write a minimal
known-good `config/remediation-targets.json` (only `targets: []` is
valid — the optimizer treats zero targets as "advisory mode only" with
no remediable findings) and rerun the script.

**When changes take effect:**

- The in-cluster CronJob picks up the new ConfigMap on its next firing
  (the CronJob mounts the file fresh for each Job; no rolling restart
  needed). Default schedule is `*/30 * * * *`.
- The local UI loads the file at process start, so restart the UI
  process to pick up local edits.

## "Did the optimizer cause this incident?" checklist

1. `kubectl -n cluster-optimizer logs <recent job>` — did the applier touch
   the workload during the incident window?
2. `kubectl get deploy <name> -o yaml | grep -A 4 'managedFields:'` — does
   any entry list `manager: cluster-optimizer-applier` recently?
3. If yes, [activate the halt switch](#activate-the-halt-switch), then
   [roll back the patch](#roll-back-a-single-workload-patch).
4. File a regression: the trim shouldn't have happened. Note the workload,
   the trimmed value, and the observed pre-incident usage from your metrics.
   This usually means the analyzer's usage signal is missing burst data and
   the policy floor or max-trim cap needs to be raised for that workload.

## Things the optimizer will never do on its own

These are absolute, code-enforced limits. If one of these happens, treat it
as a bug:

- Patch a provider-managed workload (kube-proxy, cilium, csi-do-node,
  do-node-agent, doks-telemetry-config-reloader, konnectivity-agent,
  hubble-relay, hubble-ui, coredns, metrics-server, cpc-bridge-proxy, or
  anything in `kube-system` / `kube-public` / `kube-node-lease`).
- Patch a workload that is not in `config/remediation-targets.json` with
  the matching `rule_id` listed.
- Raise a request, replica count, or limit. The applier only trims.
- Trim more than 50% of the current value in a single pass.
- Trim below 10m CPU or 32Mi memory.
- Make more than one workload change per CronJob tick.
- Mutate without both `--auto-apply` AND `CLUSTER_OPTIMIZER_AUTOAPPLY=true`.
- Mutate when the halt ConfigMap reads `halt=true`, or when reading it
  fails for any non-NotFound reason.
- Delete or resize a node, call the DigitalOcean API, or write to DynamoDB
  beyond report and recommendation rows.
