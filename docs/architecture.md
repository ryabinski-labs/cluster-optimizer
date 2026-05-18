# Architecture

## Decision

Use a read-only CronJob plus optional DynamoDB persistence for the first
release. A CronJob is cheaper and simpler than a DaemonSet for cluster-scoped
analysis, and read-only recommendations preserve operator control.

## Alternatives Considered

| Option | Pros | Cons | Decision |
|---|---|---|---|
| CronJob collector | Low cost, simple RBAC, easy logs, natural snapshots | Not real-time | Chosen for MVP |
| Long-running Deployment | Can expose API/UI later, easier watch loops | More operational surface | Later |
| DaemonSet | Node-local visibility, per-node diagnostics | Higher cost, duplicate API reads, broader footprint | Not needed for MVP |
| Mutating controller | Can apply savings automatically | High reliability/security risk | Explicitly out of scope |

## Components

- CLI entrypoint: runs locally or in cluster.
- Kubernetes collector: reads nodes, pods, workloads, HPAs, PDBs, and metrics.
- Analyzer: produces findings with evidence and pillar trade-offs.
- Persistence adapter: writes reports to DynamoDB when configured.
- Manifests: RBAC and CronJob example.

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
| One short metrics sample can mislead sizing | High | Mark confidence, recommend multi-day p95/p99 validation |
| Provider-specific node pricing is absent | Medium | Keep cost effect qualitative until provider adapters ship |
| PDB percentage/matchExpression edge cases | Medium | Support percentages now; add matchExpression support next |
| DynamoDB table misconfiguration | Low | Persistence is optional; stdout remains source of truth |

## Roadmap

1. Add provider adapters for DOKS/EKS/GKE/AKS node pricing and node-pool min/max.
2. Add Prometheus/VPA adapters for historical p95/p99 usage.
3. Add GitOps output: suggested patches/PR templates instead of live mutation.
4. Add a small UI/API Deployment mode backed by DynamoDB.
5. Add policy packs for tenant-specific availability rules.

