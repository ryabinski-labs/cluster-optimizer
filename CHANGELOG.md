# Changelog

All notable changes to Cluster Optimizer will be documented in this file.

## Unreleased

- Surfaced active remediation in the local UI: the CLI now persists each applier `Outcome` and a per-run nudger summary as `REMEDIATION#` items, plus an `ENGINE_STATUS` sentinel (auto-apply/nudge mode, halt switch, last-run counts). The UI adds an engine-status strip (Mode pill with two-gate popover, Halt switch, Last run summary), a "Recent Remediation Activity" panel (filterable by All / Live only / Errors / Dry-run; rule chip jumps to the matching finding), a halt banner when the kill switch is set, and a new `GET /api/remediations/history` endpoint feeding the panel.
- Reduced DynamoDB call volume and fixed a class of "context deadline exceeded / failed to get rate limit token" errors by sharing a tuned `store.NewDynamoDBClient` (no-op retry rate limiter, explicit HTTP transport timeouts), projecting only the attributes needed for `Occurrences`, paginating `REC#` queries (previously silently truncated at the 1 MB Query page), batching finding writes via `BatchWriteItem` (25 per request) instead of per-finding `PutItem`, and dropping the second `Query` inside `PutReport` by accepting the caller's existing-recommendation map.
- Added a short-TTL in-memory cache for `cluster-optimizer-ui` reports + rollups (configurable via `--cache-ttl-seconds` / `REPORTS_CACHE_TTL_SECONDS`, default 30s) and a `--trend-report-cap` / `TREND_REPORT_CAP` cap (default 50) so dashboard polls do not repeatedly fan out to DynamoDB.
- Clarified `scripts/verify-deployment.sh` output so it directly answers whether the latest published or requested image is deployed.
- Added `provider_managed` and `remediable` fields to every analyzer finding so remediators can refuse to touch DOKS-reconciled DaemonSets (kube-proxy, cilium, csi-do-node, do-node-agent, doks-telemetry-config-reloader, konnectivity-agent, hubble-relay/ui, coredns, metrics-server, cpc-bridge-proxy) and so callers can tell at a glance which findings have a remediation target configured.
- Added DaemonSet and StatefulSet support to `api-yml-remediator` for `memory-request-over-provisioned` and `cpu-request-over-provisioned` rules, with an explicit refusal to patch provider-managed workload names.
- Added a `plan` package that turns findings into an auditable list of `PlannedAction`s with safety defaults (confidence=high, ≥3 occurrences, 50% max trim per pass, 10m/32Mi floors, 1 action per run), and skips with a recorded reason when any gate fails.
- Added an `applier` package and `--auto-apply` flag that can patch workload resource requests live via the Kubernetes API. Defaults to dry-run; live mutation requires BOTH `--auto-apply` and `CLUSTER_OPTIMIZER_AUTOAPPLY=true`. Reads a halt ConfigMap (`cluster-optimizer/cluster-optimizer-halt`, key `halt=true`) before any mutation and fails closed if it can't be read.
- Extended `nudger` with a dry-run default (`CLUSTER_OPTIMIZER_NUDGE_LIVE=true` required to actually cordon/evict), a shared halt-switch check, and a PDB pre-flight that aborts when the eviction would be blocked.
- Added optional `manifests/rbac-applier.yaml` granting the applier the minimum `patch` verb on Deployments, DaemonSets, and StatefulSets in the `default` namespace, plus a single-resource `get` on the halt ConfigMap.
- Added `docs/runbook.md` with halt-switch activation, single-workload rollback, uncordon, CronJob suspend, applier RBAC revoke, and a "did the optimizer cause this incident?" checklist.
- Updated `docs/architecture.md` to document the classifier, planner, applier, nudger options, and the expanded risk register.
- Added an `api.yml` remediation for the `cpu-hpa-low-request-sensitive` rule that adds or raises HPA scale-up and scale-down stabilization windows, reusing the existing remediator, workflow, UI, and remediation-target interfaces.
- Updated local Kubernetes deployment and verification scripts to resolve the latest successfully published image tag when no explicit tag is provided.
- Added local deployment helper scripts that trigger the CI/CD deploy workflow and verify the live CronJob image tag.
- Added HPA sensitivity analysis for percentage-based CPU autoscalers whose low CPU requests can cause replica churn after request tuning.
- Replaced remediation persistence fractions such as `4/3 days` with clearer review-readiness labels in the UI.
- Added active pod-nudging capabilities to safely pack and consolidate cluster workloads onto as few nodes as possible.
- Added a local UI optimization overview for node-fit posture, headroom guardrails, observed/requested memory, and ready actions.
- Added a `--nudge` CLI flag and `CLUSTER_OPTIMIZER_NUDGE` environment variable to trigger active consolidation.
- Updated RBAC permissions in `manifests/rbac.yaml` to authorize node cordoning and pod evictions.
- Unified both rewrite and resource-remediation actions to display a beautifully-styled, local-first instructions preview modal with clipboard-copy and markdown-download capabilities.
- Implemented direct browser markdown file downloads for rewrite modernization plans.
- Replaced standard browser confirm popups with a beautiful, modern native in-app confirmation modal.
- Fixed UI observed-day calculation by implementing paginated DynamoDB queries and stateful recommendation tracking to prevent sliding-window resets.
- Fixed UI observed-day counts to advance on UTC calendar-day boundaries instead of waiting for a full 24 hours.
- Fixed Kubernetes deploy workflow authentication to use a DigitalOcean token and short-lived kubeconfig instead of storing kubeconfig in repository secrets.
- Fixed Kubernetes deploy workflow rendering when the requested image tag already matches the manifest image.
- Reduced the optimizer CronJob scheduling request so report collection does not trigger node scale-ups on tightly packed clusters.
- Updated Kubernetes patch dependencies while staying on the supported 0.35 module line.
- Updated GitHub Actions workflow dependencies.
- Stabilized Dependabot policy for supported Kubernetes module versions and generated files.
- Added pull request validation to require `CHANGELOG.md` updates on every PR.
- Prepared repository controls, documentation, and workflows for public open-source release.
