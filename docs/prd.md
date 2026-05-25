# Product Requirements: Cluster Optimizer

## Problem

Small multi-tenant Kubernetes clusters often carry avoidable cost because
requested capacity, PDBs, HPAs, DaemonSet overhead, and node-pool shape drift
over time. Existing dashboards show utilization, but they rarely translate it
into staged, reliability-aware changes that an operator can safely review,
approve, and apply with a narrow blast radius.

## Target Users

- Platform operators running shared Kubernetes clusters.
- Small teams using managed Kubernetes without a dedicated FinOps function.
- App owners who need concrete request/HPA/PDB changes with evidence.

## Goals

- Identify cost-saving opportunities that are safe to review and phase in.
- Preserve reliability, security, performance, operational excellence, and
  sustainability trade-offs in every recommendation.
- Support local CLI use and in-cluster scheduled collection.
- Persist report history to DynamoDB when configured.
- Show recurring trends, remediation readiness, engine mode, halt status, and
  recent remediation activity in a local UI.
- Support opt-in remediation paths: live CPU/memory request trimming for
  allowlisted workloads, GitHub PRs for supported `api.yml` manifest changes,
  coding-agent instruction PRs for runtime modernization candidates, and dry-run
  or opt-in live node nudging for safe consolidation.
- Keep the core open-source and cloud-provider portable.

## Non-Goals

- Live workload mutation by default.
- Broad mutating-controller behavior, admission-webhook enforcement, or
  unbounded automatic patching.
- Direct cloud-provider node deletion or node-pool resizing.
- Cloud invoice ingestion.
- Replacing Prometheus, VPA, or cluster autoscaler.
- Reading Secret values.

## MVP User Stories

1. As a platform operator, I can run an advisory scan and see findings ordered
   by risk and cost impact.
2. As an app owner, I can see why a request, HPA, or PDB recommendation was
   made before changing manifests.
3. As a cluster owner, I can schedule scans and store report history in
   DynamoDB.
4. As a platform operator, I can see whether remediation is disabled, dry-run,
   live, or halted without reading logs.
5. As a platform operator, I can preview exactly which workload request would
   be changed before enabling live apply.
6. As a service owner, I can receive a GitHub pull request for supported
   manifest remediations instead of manually translating a finding.
7. As a platform operator, I can stop all future live mutations immediately
   through a halt ConfigMap.
8. As an open-source adopter, I can run the core without a specific cloud
   provider account.

## Acceptance Criteria

- The collector runs with read-oriented Kubernetes RBAC; live remediation verbs
  are isolated behind optional manifests and gates.
- Missing metrics API degrades gracefully.
- Reports include summary totals, findings, evidence, recommendation, risk,
  confidence, and Well-Architected pillar context.
- DynamoDB persistence is optional and off unless `DYNAMODB_TABLE` is set.
- The CronJob can run without persistent storage and still emit JSON to logs.
- The planner refuses live request-trim actions unless the finding is
  allowlisted, high confidence, recurrence-backed, provider-safe, above floors,
  within the max-trim cap, and inside the per-run action budget.
- The applier remains dry-run unless both `--auto-apply` and
  `CLUSTER_OPTIMIZER_AUTOAPPLY=true` are present.
- The nudger remains dry-run unless `--nudge` and
  `CLUSTER_OPTIMIZER_NUDGE_LIVE=true` are present.
- The halt ConfigMap stops both live mutation paths and fails closed on read
  errors.
- The UI exposes remediation readiness and disabled reasons before showing an
  actionable remediation control.

## Success Metrics

- Operator can identify at least one actionable cluster optimization in under
  five minutes.
- Operator can tell whether the engine is advisory, dry-run, live, or halted in
  under one minute from the UI.
- Remediation PRs include enough evidence for a service owner to review without
  opening the cluster report separately.
- Zero recommendations require Secret access.
- False-positive rate stays low enough that operators do not mute the tool;
  target fewer than 20% findings dismissed after initial tuning.
- For clusters using persistence, report history supports trend review across
  at least 30 days.
- Live remediation incidents remain rare and diagnosable: every live action has
  an audit event, log entry, and documented rollback path.
