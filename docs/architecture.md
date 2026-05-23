# Architecture

## Decision

Use a read-only CronJob plus optional DynamoDB persistence for the first
release. A CronJob is cheaper and simpler than a DaemonSet for cluster-scoped
analysis, and read-only recommendations preserve operator control.

Live in-cluster remediation is available as an opt-in capability layered on
top of the read-only core. The default behaviour remains advisory: live
mutation requires two independent gates AND a halt-ConfigMap check.

## Alternatives Considered

| Option | Pros | Cons | Decision |
|---|---|---|---|
| CronJob collector | Low cost, simple RBAC, easy logs, natural snapshots | Not real-time | Chosen for MVP |
| Long-running Deployment | Can expose API/UI later, easier watch loops | More operational surface | Later |
| DaemonSet | Node-local visibility, per-node diagnostics | Higher cost, duplicate API reads, broader footprint | Not needed for MVP |
| Mutating controller | Can apply savings automatically | High reliability/security risk | Explicitly out of scope |

## Components

- CLI entrypoint (`cmd/cluster-optimizer`): runs locally or in cluster.
- Kubernetes collector (`internal/collector`): reads nodes, pods, workloads,
  HPAs, PDBs, and metrics.
- Analyzer (`internal/analyzer`): produces findings with evidence and pillar
  trade-offs.
- Classifier (`internal/classifier`): tags each finding with
  `provider_managed` (DOKS-controlled DaemonSets and system namespaces) and
  `remediable` (a target in `config/remediation-targets.json` supports the
  rule for this workload). Single source of truth for what the optimizer is
  *allowed* to touch.
- Planner (`internal/plan`): turns findings into a `Plan` of concrete
  `PlannedAction`s. Pure function; enforces all safety gates (confidence,
  occurrence count, max-trim, floors, per-tick budget). Always emitted, so
  dry-run output shows exactly what *would* run.
- Applier (`internal/applier`): executes a `Plan` against the Kubernetes
  API. Defaults to dry-run; live mutation requires `--auto-apply` AND
  `CLUSTER_OPTIMIZER_AUTOAPPLY=true`, plus an unread or absent
  `cluster-optimizer-halt` ConfigMap. Fail-closed on halt read errors.
- Nudger (`internal/nudger`): cordons + evicts to consolidate onto fewer
  nodes. Dry-run by default; live mode requires `CLUSTER_OPTIMIZER_NUDGE`
  and `CLUSTER_OPTIMIZER_NUDGE_LIVE`. Respects the same halt switch and
  pre-flights PDBs.
- PR-gated remediator (`cmd/api-yml-remediator`): patches workload
  manifests in user-owned application repositories. Now supports
  Deployment, DaemonSet, and StatefulSet kinds, and refuses any name in
  the provider-managed list.
- Persistence adapter (`internal/store`): writes reports and recommendation
  occurrence counts to DynamoDB when configured. Occurrence counts are the
  evidence source the planner uses to verify multi-run agreement before
  live mutation.
- Manifests: base read-only RBAC + CronJob, plus optional
  `manifests/rbac-applier.yaml` for the patch verb when auto-apply is
  enabled.

## Well-Architected Review

- Operational excellence: reports are explicit, scheduled, and testable; no
  hidden mutation.
- Security: read-only RBAC, non-root container, no Secret value access.
- Reliability: recommendations do not reduce replicas or requests without
  evidence and risk context.
- Performance efficiency: analyzes scheduling requests, actual metrics, and
  DaemonSet overhead.
- Cost optimization: targets bin-packing, blocked drains, over-requests, and
  ineffective autoscaling.
- Sustainability: reducing idle nodes and over-provisioned compute lowers
  wasted capacity.

## Risk Register

| Risk | Severity | Mitigation |
|---|---|---|
| Live trim causes OOMKill or throttling under burst | High | Live applier requires `confidence=high` + ≥3-run agreement + DynamoDB persistence + 50% max-trim cap per pass + 10m/32Mi floor + 1-workload-per-tick budget; dry-run is default |
| Operator mistakenly patches a DOKS-managed resource | High | Hardcoded provider-managed namespace + workload-name list in classifier; planner refuses to emit actions for them; PR remediator also refuses |
| One short metrics sample can mislead sizing | High | Mark confidence, recommend multi-day p95/p99 validation |
| Operator forgets the kill switch exists | Medium | Documented in README + runbook; applier logs reference the halt ConfigMap path on every run |
| Nudger cordons a node that would violate a PDB | Medium | Pre-flight: lists matching PDBs and aborts if `DisruptionsAllowed=0`; PDB list errors are also treated as blockers |
| RBAC drift adds patch verbs to wrong role | Medium | Applier RBAC split into separate `rbac-applier.yaml`; base `rbac.yaml` has read-only + node-update + pods/eviction only |
| Provider-specific node pricing is absent | Medium | Keep cost effect qualitative until provider adapters ship |
| PDB percentage/matchExpression edge cases | Medium | Support percentages now; add matchExpression support next |
| DynamoDB unavailable → no occurrence count, no rollback log | Medium | Planner refuses live action without persistence; advisory output continues to work via stdout |
| DynamoDB table misconfiguration | Low | Persistence is optional; stdout remains source of truth |

## Roadmap

1. Add provider adapters for DOKS/EKS/GKE/AKS node pricing and node-pool min/max.
2. Add Prometheus/VPA adapters for historical p95/p99 usage.
3. Add GitOps output: suggested patches/PR templates instead of live mutation.
4. Add a small UI/API Deployment mode backed by DynamoDB.
5. Add policy packs for tenant-specific availability rules.

