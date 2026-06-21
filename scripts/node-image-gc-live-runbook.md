# Node Image GC Live Runbook

Use this when enabling live cleanup for stale container images on Kubernetes
nodes. The cleanup runs as a separate DaemonSet from the normal
`cluster-optimizer` CronJob.

## What It Does

- Runs one `cluster-optimizer-node-gc` pod per node.
- Talks to the node CRI socket, normally `/run/containerd/containerd.sock`.
- Measures host root-disk usage through `/host`.
- Removes only images no current or still-present container references.
- Skips cleanup while root disk is below `CLUSTER_OPTIMIZER_NODE_GC_DISK_THRESHOLD`.
- Defaults to dry-run unless `CLUSTER_OPTIMIZER_NODE_GC_LIVE=true`.

## Enable Live Cleanup

1. Confirm the CronJob image tag to reuse:

```bash
rtk kubectl -n cluster-optimizer get cronjob cluster-optimizer \
  -o jsonpath='{.spec.jobTemplate.spec.template.spec.containers[0].image}'
```

2. Apply the DaemonSet and its namespaced RBAC:

```bash
rtk kubectl apply -f manifests/daemonset-image-gc.yaml
```

3. If the image is private, attach the existing GHCR pull secret to the
   node-GC service account:

```bash
rtk kubectl -n cluster-optimizer patch serviceaccount cluster-optimizer-node-gc \
  -p '{"imagePullSecrets":[{"name":"ghcr-pull"}]}'
```

4. Pin the DaemonSet to the same immutable image as the CronJob:

```bash
rtk kubectl -n cluster-optimizer set image daemonset/cluster-optimizer-node-gc \
  node-image-gc=<image-from-step-1>
```

5. Enable live deletion with a bounded first-run cap:

```bash
rtk kubectl -n cluster-optimizer set env daemonset/cluster-optimizer-node-gc \
  CLUSTER_OPTIMIZER_NODE_GC_LIVE=true \
  CLUSTER_OPTIMIZER_NODE_GC_MAX_REMOVALS=20
```

6. Wait for rollout:

```bash
rtk kubectl -n cluster-optimizer rollout status daemonset/cluster-optimizer-node-gc --timeout=180s
```

## Verify It Is Running

```bash
rtk kubectl -n cluster-optimizer get daemonset cluster-optimizer-node-gc -o wide
rtk kubectl -n cluster-optimizer get pods -l app.kubernetes.io/name=cluster-optimizer-node-gc -o wide
rtk kubectl -n cluster-optimizer logs -l app.kubernetes.io/name=cluster-optimizer-node-gc --tail=200
```

Look for log lines like:

```text
node-image-gc: running every 30m (live=true, threshold=65%)
Node image GC: removed N of N image(s), reclaimed ~X MB
```

If logs say `disk X% below threshold 65%`, that node was skipped. If logs say
`none unreferenced`, the runtime did not expose any removable stale images.

## Confirm Storage Impact

Run the regular optimizer CronJob after cleanup and inspect the
`node-disk-utilization-high` finding:

```bash
rtk kubectl create job cluster-optimizer-manual-$(date +%s) \
  -n cluster-optimizer \
  --from=cronjob/cluster-optimizer

rtk kubectl -n cluster-optimizer logs -l job-name=<created-job-name>
```

Expected improvement is lower root disk used and/or lower image cache usage in
the finding evidence. Example field to compare:

```text
image cache uses X GB
```

## Pause Or Disable

Dry-run mode, no more deletions:

```bash
rtk kubectl -n cluster-optimizer set env daemonset/cluster-optimizer-node-gc \
  CLUSTER_OPTIMIZER_NODE_GC_LIVE=false
```

Remove the DaemonSet entirely:

```bash
rtk kubectl -n cluster-optimizer delete daemonset cluster-optimizer-node-gc
```

Emergency halt for live mutation paths that honor the shared halt switch:

```bash
rtk kubectl -n cluster-optimizer create configmap cluster-optimizer-halt \
  --from-literal=halt=true \
  --dry-run=client -o yaml | rtk kubectl apply -f -
```

Clear the halt:

```bash
rtk kubectl -n cluster-optimizer delete configmap cluster-optimizer-halt
```
