# Contributing

Cluster Optimizer is intended to become an open-source Kubernetes FinOps tool.
Contributions should preserve the default safety model:

- Do not add automatic mutation without an explicit feature gate, audit trail,
  and rollback story. Live mutation must require BOTH a CLI flag and an env
  var (today's applier uses `--auto-apply` AND `CLUSTER_OPTIMIZER_AUTOAPPLY=true`);
  never collapse to a single gate. Every new mutation path needs a rollback
  procedure documented in `docs/runbook.md`.
- Do not read Kubernetes Secret values. Secret metadata is enough for cost and
  reliability analysis.
- Prefer standard Kubernetes APIs. Provider-specific code belongs behind an
  optional integration boundary.
- Every recommendation should include evidence, risk, expected cost effect, and
  reliability impact.
- Add tests for new rules.

## Development

```bash
go test ./...
go run ./cmd/cluster-optimizer --output text
```

## Changelog

Every pull request must update `CHANGELOG.md` under `## Unreleased` with a
short user-visible summary of the change.

## Pull Request Checklist

- [ ] New rules include tests.
- [ ] CHANGELOG.md updated under `## Unreleased`.
- [ ] Kubernetes manifests still pass client-side dry-run.
- [ ] No Secret data is logged or persisted.
- [ ] User-facing output distinguishes observed facts from assumptions.
