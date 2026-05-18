# Product Requirements: Cluster Optimizer

## Problem

Small multi-tenant Kubernetes clusters often carry avoidable cost because
requested capacity, PDBs, HPAs, DaemonSet overhead, and node-pool shape drift
over time. Existing dashboards show utilization, but they rarely translate it
into staged, reliability-aware changes that an operator can safely review.

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
- Keep the core open-source and cloud-provider portable.

## Non-Goals

- Automatic workload mutation in the initial release.
- Cloud invoice ingestion in the initial release.
- Replacing Prometheus, VPA, or cluster autoscaler.
- Reading Secret values.

## MVP User Stories

1. As a platform operator, I can run a read-only scan and see findings ordered
   by risk and cost impact.
2. As an app owner, I can see why a request, HPA, or PDB recommendation was
   made before changing manifests.
3. As a cluster owner, I can schedule scans and store report history in
   DynamoDB.
4. As an open-source adopter, I can run the core without a specific cloud
   provider account.

## Acceptance Criteria

- The collector runs with read-only Kubernetes RBAC.
- Missing metrics API degrades gracefully.
- Reports include summary totals, findings, evidence, recommendation, risk,
  confidence, and Well-Architected pillar context.
- DynamoDB persistence is optional and off unless `DYNAMODB_TABLE` is set.
- The CronJob can run without persistent storage and still emit JSON to logs.

## Success Metrics

- Operator can identify at least one actionable cluster optimization in under
  five minutes.
- Zero recommendations require Secret access.
- False-positive rate stays low enough that operators do not mute the tool;
  target fewer than 20% findings dismissed after initial tuning.
- For clusters using persistence, report history supports trend review across
  at least 30 days.

