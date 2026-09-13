package nudger

import (
	"context"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/GipsyChef/cluster-optimizer/internal/applier"
	"github.com/GipsyChef/cluster-optimizer/internal/capacity"
	"github.com/GipsyChef/cluster-optimizer/internal/collector"
	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Options gates and configures the nudger. The zero value is dry-run with
// a single cordon+evict pass — safe to default to.
type Options struct {
	// Live, when true, actually cordons the node and evicts pods. Default
	// false: dry-run only prints the consolidation plan.
	Live bool
	// HaltNamespace / HaltConfigMap / HaltKey identify the kill switch
	// (same one the applier uses). An operator can stop both mutation
	// paths by writing halt=true into the configmap.
	HaltNamespace string
	HaltConfigMap string
	HaltKey       string
	// SelfNamespace is the namespace this tool runs in. Pods found there are
	// the optimizer's own: they are never part of the relocatable set (the
	// process would evict itself), their capacity is still charged to their
	// host, and no node hosting one may be cordoned — cordoning the node out
	// from under the running job kills the run mid-pass and strands the
	// cordon with nothing in the audit log to explain it. Empty disables the
	// check; NewOptions defaults it to the halt-switch namespace.
	SelfNamespace string
	// CordonTTL is how long a cordon this tool placed may stand before the
	// reaper reverses it. Zero disables reaping entirely, which restores the
	// pre-reaper behaviour of leaving cordons in place indefinitely.
	CordonTTL time.Duration
	// RecordonCooldown keeps a reaped node out of the candidate set for a
	// while, so reaping cannot become a cordon/evict/uncordon loop.
	RecordonCooldown time.Duration
	// Capacity is this run's pool-level verdict from the capacity engine.
	// Consolidation is gated on it: a node is only a candidate when its pool
	// has an actionable verdict and at least one usable node to spare, so the
	// mutating path can never be less conservative than the plan the same run
	// reports. Nil means no verdict was supplied; the pass then refuses to
	// consolidate rather than trusting the nudger's own, request-only model.
	Capacity *capacity.Result
	// RunID identifies this invocation on the cordons it places. Left empty,
	// NudgePodsWithResult generates one.
	RunID string
	// now is a test seam. Nil means time.Now.
	now func() time.Time
}

// NewOptions returns Options with the same safe defaults as the applier
// (dry-run, halt switch at cluster-optimizer/cluster-optimizer-halt) plus
// stale-cordon reaping enabled.
func NewOptions() Options {
	return Options{
		HaltNamespace:    applier.DefaultHaltNamespace,
		HaltConfigMap:    applier.DefaultHaltConfigMap,
		HaltKey:          applier.DefaultHaltKey,
		SelfNamespace:    applier.DefaultHaltNamespace,
		CordonTTL:        DefaultCordonTTL,
		RecordonCooldown: DefaultRecordonCooldown,
	}
}

// clock returns the time source for this run.
func (o Options) clock() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

// Result summarises one NudgePods run for downstream persistence (the
// remediation audit log). It is deliberately compact — the verbose per-pod
// detail stays in logs. Mode is "live" or "dry-run"; an empty TargetNode
// means no consolidation was feasible.
type Result struct {
	Mode              string
	Halted            bool
	HaltReason        string
	TargetNode        string
	RelocatablePods   int
	Evicted           int
	EvictionErrors    int
	NotFeasibleReason string
	// Reap records what the stale-cordon pass did before this run looked for
	// a new consolidation target.
	Reap ReapResult
}

// NudgePods scans the cluster nodes and active pods, determines if any node's
// workloads can be fully consolidated/packed onto the remaining schedulable nodes,
// and if so, cordons the candidate node and evicts its pods.
//
// In dry-run mode (Options.Live=false, the default) it logs the plan and
// returns without mutating anything.
func NudgePods(ctx context.Context, clientset kubernetes.Interface, opts Options) error {
	_, err := NudgePodsWithResult(ctx, clientset, opts)
	return err
}

// NudgePodsWithResult is the same as NudgePods but also returns a Result
// describing what happened (or would have happened in dry-run). The
// remediation audit log consumes this so the UI can show recent activity.
func NudgePodsWithResult(ctx context.Context, clientset kubernetes.Interface, opts Options) (Result, error) {
	result := Result{Mode: "dry-run"}
	if opts.Live {
		result.Mode = "live"
	}
	mode := "DRY-RUN"
	if opts.Live {
		mode = "LIVE"
	}
	log.Printf("Active Nudger (%s): Starting cluster consolidation analysis...", mode)

	if opts.Live {
		if halted, reason := nudgerHaltCheck(ctx, clientset, opts); halted {
			// The reaper stays behind this gate on purpose. Halt means "this
			// tool touches nothing"; an operator who needs a stale cordon
			// reversed while halted can find it by its annotations and
			// uncordon it by hand.
			log.Printf("Active Nudger: halt switch active (%s), refusing to cordon", reason)
			result.Halted = true
			result.HaltReason = reason
			return result, nil
		}
	}

	runID := opts.RunID
	if runID == "" {
		runID = newRunID()
	}
	now := opts.clock()

	// 0. Repair before extending. A previous run that died between cordoning
	// and evicting leaves a node unschedulable with nothing to ever undo it,
	// and a drain nothing acted on is lost capacity rather than work in
	// progress. Reverse those first so this run sees the cluster's real
	// schedulable capacity instead of the residue of past failures.
	result.Reap = ReapStaleCordons(ctx, clientset, opts.Live, runID, opts.CordonTTL, now)
	for _, msg := range result.Reap.Errors {
		log.Printf("Active Nudger: stale-cordon reap failed: %s", msg)
	}
	if len(result.Reap.Uncordoned) > 0 && !opts.Live {
		log.Printf("Active Nudger DRY-RUN: would reverse %d stale cordon(s): %v", len(result.Reap.Uncordoned), result.Reap.Uncordoned)
	}

	// The capacity engine's verdict is the enforcement gate for this pass.
	// The nudger's own simulation answers only "would these pods fit
	// elsewhere?"; it deliberately ignores the configured floor, the
	// survive-one-loss rule, pinned pods and weak usage evidence, so on its
	// own it will happily drain a node the engine has already refused to give
	// back. Refusing to run without the verdict keeps the mutating path from
	// ever being looser than the plan the same run reports.
	if opts.Capacity == nil {
		log.Println("Active Nudger: no capacity engine verdict supplied for this run; refusing to consolidate")
		result.NotFeasibleReason = "capacity engine verdict not supplied"
		return result, nil
	}

	// 1. Fetch all nodes. This runs after the reap so any node just returned
	// to service counts toward the packing simulation below.
	nodeList, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return result, fmt.Errorf("failed to list nodes: %w", err)
	}
	if len(nodeList.Items) < 2 {
		log.Println("Active Nudger: Cluster has fewer than 2 nodes. Consolidation not possible.")
		result.NotFeasibleReason = "cluster has fewer than 2 nodes"
		return result, nil
	}

	// 2. Fetch all pods across all namespaces
	podList, err := clientset.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return result, fmt.Errorf("failed to list pods: %w", err)
	}

	// 3. Define helper structures to model node capacity and resource request tracking
	type nodeState struct {
		node           corev1.Node
		name           string
		pool           string
		allocatableCPU int64 // millicores
		allocatableMem int64 // MiB
		requestedCPU   int64 // millicores
		requestedMem   int64 // MiB
		freeCPU        int64 // millicores
		freeMem        int64 // MiB
		isSchedulable  bool
		hostsSelf      bool
		activePods     []corev1.Pod
	}

	nodesMap := make(map[string]*nodeState)
	for _, node := range nodeList.Items {
		cpu := node.Status.Allocatable.Cpu().MilliValue()
		mem := node.Status.Allocatable.Memory().Value() / 1024 / 1024 // bytes to MiB
		isSchedulable := !node.Spec.Unschedulable && collector.NodeReady(node)

		nodesMap[node.Name] = &nodeState{
			node:           node,
			name:           node.Name,
			pool:           collector.NodePoolName(node.Labels),
			allocatableCPU: cpu,
			allocatableMem: mem,
			isSchedulable:  isSchedulable,
			activePods:     []corev1.Pod{},
		}
	}

	// 3b. Aggregate live per-pool health. The capacity engine reasons about
	// pools, so the gate that consumes its verdict must too: a node inherits
	// its pool's constraints, including a cordon the reaper recently returned.
	type poolState struct {
		schedulable    int
		unusable       int
		reapedRecently bool
	}
	pools := make(map[string]*poolState)
	for _, ns := range nodesMap {
		ps := pools[ns.pool]
		if ps == nil {
			ps = &poolState{}
			pools[ns.pool] = ps
		}
		if ns.isSchedulable {
			ps.schedulable++
		} else {
			ps.unusable++
		}
		if !ps.reapedRecently && inRecordonCooldown(ns.node, opts.RecordonCooldown, now) {
			// One reaped cordon in a pool is enough to bench the whole pool:
			// the failure it detects — a drained node that nothing removed —
			// says nothing about which node the next run would pick, so
			// allowing another node in the same pool is how the documented
			// drain/refill ping-pong starts.
			ps.reapedRecently = true
		}
	}

	// 4. Map active pods to their respective nodes and accumulate requests
	for _, pod := range podList.Items {
		// Skip succeeded, failed, or terminating pods
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		nodeName := pod.Spec.NodeName
		if nodeName == "" {
			continue // pod is not scheduled yet
		}

		ns, exists := nodesMap[nodeName]
		if !exists {
			continue // scheduled on unknown node
		}

		// Calculate pod resource requests
		var podCPU, podMem int64
		for _, container := range pod.Spec.Containers {
			if rCPU, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
				podCPU += rCPU.MilliValue()
			}
			if rMem, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
				podMem += rMem.Value() / 1024 / 1024
			}
		}

		ns.requestedCPU += podCPU
		ns.requestedMem += podMem

		// The optimizer's own pods are never part of the relocatable set: a
		// drain that evicts the process running it dies before it can record
		// anything, leaving the cordon behind. They are still charged for the
		// capacity they occupy, because their host may need to absorb pods
		// from a different node.
		if opts.SelfNamespace != "" && pod.Namespace == opts.SelfNamespace {
			ns.hostsSelf = true
			continue
		}

		ns.activePods = append(ns.activePods, pod)
	}

	// Compute initial free capacities
	for _, ns := range nodesMap {
		ns.freeCPU = ns.allocatableCPU - ns.requestedCPU
		ns.freeMem = ns.allocatableMem - ns.requestedMem
	}

	// Helper to determine if a pod is relocatable
	isRelocatable := func(pod corev1.Pod) bool {
		// 1. Must have at least one owner reference that is a controller
		hasControllerOwner := false
		for _, owner := range pod.OwnerReferences {
			if owner.Controller != nil && *owner.Controller {
				// Avoid DaemonSets
				if owner.Kind == "DaemonSet" {
					return false
				}
				hasControllerOwner = true
			}
		}
		if !hasControllerOwner {
			return false // bare pods are not safe to evict/reschedule
		}

		// 2. Mirror pods (static pods) cannot be evicted
		if _, isMirror := pod.Annotations["kubernetes.io/config.mirror"]; isMirror {
			return false
		}

		return true
	}

	// 5. Filter nodes that are candidates for emptying.
	// We want to find a node whose relocatable pods can be completely rescheduled onto the other *schedulable* nodes.
	// Two distinct questions, deliberately separated: which nodes can receive
	// pods, and which nodes may be emptied. Nodes in a benched pool are still
	// perfectly good capacity — they just must not be drained again yet.
	//
	// A candidate must clear the capacity engine's verdict for its pool, not
	// just the packing simulation below. The simulation is a placement check;
	// the verdict is the enforcement decision, and it is the stricter of the
	// two by construction: it honours the configured floor, the
	// survive-one-loss rule, pinned workloads and usage-evidence quality.
	verdicts := make(map[string]capacity.PoolVerdict, len(opts.Capacity.Pools))
	for _, verdict := range opts.Capacity.Pools {
		verdicts[verdict.Pool] = verdict
	}

	var schedulableCount int
	var candidateNodes []*nodeState
	blockedPools := map[string]string{}
	for _, ns := range nodesMap {
		if !ns.isSchedulable {
			continue // Node is cordoned or not Ready
		}
		schedulableCount++

		// Never cordon the node this process is running on. Even with the
		// self namespace excluded from eviction, cordoning it would stop the
		// next attempt from scheduling there and the drained node would have
		// no follow-through; the run that did it would also die mid-pass
		// before its audit row was written.
		if ns.hostsSelf {
			log.Printf("Active Nudger: node %q runs the optimizer's own pod; refusing to cordon the node out from under this run", ns.name)
			continue
		}

		verdict, hasVerdict := verdicts[ns.pool]
		ps := pools[ns.pool]
		switch {
		case !hasVerdict:
			blockedPools[ns.pool] = "the capacity engine reported no verdict for this pool"
		case !verdict.Actionable:
			blockedPools[ns.pool] = fmt.Sprintf("the capacity verdict is not actionable (status %s, usage evidence %s)", verdict.Status, verdict.UsageFidelity)
		case ps.unusable > 0:
			blockedPools[ns.pool] = fmt.Sprintf("%d node(s) in the pool are cordoned or not Ready; the pool must return to health before another node is drained", ps.unusable)
		case ps.schedulable <= verdict.MinimumSafeNodes:
			blockedPools[ns.pool] = fmt.Sprintf("the pool needs at least %d of its %d usable node(s); there is no spare to give back", verdict.MinimumSafeNodes, ps.schedulable)
		case ps.reapedRecently:
			blockedPools[ns.pool] = fmt.Sprintf("a cordon in the pool was reaped within the last %s; nothing is removing drained nodes", opts.RecordonCooldown)
		default:
			candidateNodes = append(candidateNodes, ns)
		}
	}

	blockedNames := make([]string, 0, len(blockedPools))
	for pool := range blockedPools {
		blockedNames = append(blockedNames, pool)
	}
	sort.Strings(blockedNames)
	for _, pool := range blockedNames {
		log.Printf("Active Nudger: pool %q is not a drain source this pass: %s", pool, blockedPools[pool])
	}

	if schedulableCount < 2 {
		log.Println("Active Nudger: Less than 2 schedulable nodes. Consolidation not possible.")
		result.NotFeasibleReason = "fewer than 2 schedulable nodes"
		return result, nil
	}
	if len(candidateNodes) == 0 {
		log.Println("Active Nudger: No pool currently has a spare node to give back. Nothing to consolidate this pass.")
		result.NotFeasibleReason = noDrainablePoolReason(blockedPools)
		return result, nil
	}

	// Sort candidate nodes by total requested resources (ascending) so we try to empty the least-loaded nodes first.
	sort.Slice(candidateNodes, func(i, j int) bool {
		// Compare CPU request first, then Memory request
		if candidateNodes[i].requestedCPU != candidateNodes[j].requestedCPU {
			return candidateNodes[i].requestedCPU < candidateNodes[j].requestedCPU
		}
		return candidateNodes[i].requestedMem < candidateNodes[j].requestedMem
	})

	// 6. Iterate through sorted nodes and check packing feasibility
	var targetNodeToEmpty *nodeState
	var podsToEvict []corev1.Pod

	for _, candidate := range candidateNodes {
		// Collect relocatable pods on this candidate
		var relocatable []corev1.Pod
		var nonRelocatableActiveCount int
		for _, pod := range candidate.activePods {
			if isRelocatable(pod) {
				relocatable = append(relocatable, pod)
			} else {
				// DaemonSets, static pods, bare pods etc.
				nonRelocatableActiveCount++
			}
		}

		// If there are no relocatable pods to nudge, there is nothing to do for this node.
		if len(relocatable) == 0 {
			continue
		}

		// Simulate packing these relocatable pods onto OTHER schedulable nodes
		// Copy free capacities of all other schedulable nodes
		simulatedCapacities := make(map[string]struct{ cpu, mem int64 })
		for name, ns := range nodesMap {
			if name == candidate.name || !ns.isSchedulable {
				continue
			}
			simulatedCapacities[name] = struct{ cpu, mem int64 }{
				cpu: ns.freeCPU,
				mem: ns.freeMem,
			}
		}

		allPodsFit := true
		for _, pod := range relocatable {
			// Calculate pod resource requests
			var podCPU, podMem int64
			for _, container := range pod.Spec.Containers {
				if rCPU, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
					podCPU += rCPU.MilliValue()
				}
				if rMem, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
					podMem += rMem.Value() / 1024 / 1024
				}
			}

			// Find a simulated node that can host this pod
			placed := false
			for nodeName, capState := range simulatedCapacities {
				if capState.cpu >= podCPU && capState.mem >= podMem {
					// Simulate placement
					simulatedCapacities[nodeName] = struct{ cpu, mem int64 }{
						cpu: capState.cpu - podCPU,
						mem: capState.mem - podMem,
					}
					placed = true
					break
				}
			}

			if !placed {
				allPodsFit = false
				break
			}
		}

		if allPodsFit {
			// Check PDB constraints: every relocatable pod must currently
			// have disruption budget headroom. If a matching PDB shows
			// DisruptionsAllowed=0, evicting the pod would block and we
			// would have cordoned a node for no gain.
			if blocker := pdbBlocker(ctx, clientset, relocatable); blocker != "" {
				log.Printf("Active Nudger: candidate node %q passes capacity check but PDB %q would block eviction; skipping.", candidate.name, blocker)
				continue
			}
			targetNodeToEmpty = candidate
			podsToEvict = relocatable
			break
		}
	}

	// 7. If no node can be consolidated, log and return
	if targetNodeToEmpty == nil {
		log.Println("Active Nudger: No node consolidation is currently feasible. All nodes are packed or have non-relocatable workloads.")
		result.NotFeasibleReason = "no node consolidation is currently feasible"
		return result, nil
	}

	result.TargetNode = targetNodeToEmpty.name
	result.RelocatablePods = len(podsToEvict)

	log.Printf("Active Nudger (%s): Found consolidation opportunity. Node %q can be emptied. Relocatable pods to nudge: %d",
		mode, targetNodeToEmpty.name, len(podsToEvict))

	if !opts.Live {
		for _, pod := range podsToEvict {
			log.Printf("Active Nudger DRY-RUN: would evict pod %s/%s from node %q", pod.Namespace, pod.Name, targetNodeToEmpty.name)
		}
		log.Printf("Active Nudger DRY-RUN: would cordon node %q. Set CLUSTER_OPTIMIZER_NUDGE_LIVE=true to actually cordon and evict.", targetNodeToEmpty.name)
		return result, nil
	}

	// 8. Cordon the node to prevent new pods from scheduling on it
	nodeObj, err := clientset.CoreV1().Nodes().Get(ctx, targetNodeToEmpty.name, metav1.GetOptions{})
	if err != nil {
		return result, fmt.Errorf("failed to get node %q for cordoning: %w", targetNodeToEmpty.name, err)
	}

	if !nodeObj.Spec.Unschedulable {
		log.Printf("Active Nudger: Cordoning node %q...\n", targetNodeToEmpty.name)
		// claimCordon writes the ownership annotations and Unschedulable in
		// one update, so this run cannot leave behind a cordon that no later
		// run can recognise as its own.
		if err := claimCordon(ctx, clientset, nodeObj, runID, now); err != nil {
			return result, err
		}
		log.Printf("Active Nudger: Node %q cordoned successfully (run %s).\n", targetNodeToEmpty.name, runID)
	} else {
		log.Printf("Active Nudger: Node %q is already cordoned.\n", targetNodeToEmpty.name)
	}

	// 9. Evict (nudge) the pods
	for _, pod := range podsToEvict {
		log.Printf("Active Nudger: Evicting pod %s/%s from node %q...\n", pod.Namespace, pod.Name, targetNodeToEmpty.name)
		eviction := &policyv1beta1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			},
		}
		err := clientset.CoreV1().Pods(pod.Namespace).Evict(ctx, eviction)
		if err != nil {
			log.Printf("Active Nudger: WARNING: Failed to evict pod %s/%s: %v\n", pod.Namespace, pod.Name, err)
			result.EvictionErrors++
		} else {
			log.Printf("Active Nudger: Pod %s/%s evicted successfully.\n", pod.Namespace, pod.Name)
			result.Evicted++
		}
	}

	log.Printf("Active Nudger: Consolidation of node %q initiated successfully.\n", targetNodeToEmpty.name)
	return result, nil
}

// noDrainablePoolReason is the audit-log reason for a pass in which the
// capacity gate refused every pool. It names the first blocked pool
// alphabetically (not map-order) so successive runs produce a stable string
// and the remediation feed does not look like random noise.
func noDrainablePoolReason(blocked map[string]string) string {
	if len(blocked) == 0 {
		return "no node consolidation is currently feasible"
	}
	names := make([]string, 0, len(blocked))
	for name := range blocked {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Sprintf("no drainable pool (%d pool(s) blocked, including %q: %s)",
		len(blocked), names[0], blocked[names[0]])
}

// nudgerHaltCheck consults the same configmap the applier uses. Fail
// closed: if we can't read it, refuse to cordon.
func nudgerHaltCheck(ctx context.Context, clientset kubernetes.Interface, opts Options) (bool, string) {
	cm, err := clientset.CoreV1().ConfigMaps(opts.HaltNamespace).Get(ctx, opts.HaltConfigMap, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, ""
		}
		return true, fmt.Sprintf("unreadable halt configmap: %v", err)
	}
	if cm.Data[opts.HaltKey] == "true" {
		return true, "halt=true"
	}
	return false, ""
}

// pdbBlocker checks whether any pod-in-eviction-set would be blocked by a
// matching PDB whose DisruptionsAllowed is currently 0. Returns the name of
// the first blocking PDB, or "" if none would block.
func pdbBlocker(ctx context.Context, clientset kubernetes.Interface, pods []corev1.Pod) string {
	// Group pods by namespace so we don't list PDBs in namespaces we don't
	// touch.
	byNamespace := map[string][]corev1.Pod{}
	for _, pod := range pods {
		byNamespace[pod.Namespace] = append(byNamespace[pod.Namespace], pod)
	}
	for namespace, nsPods := range byNamespace {
		pdbs, err := clientset.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			// Be conservative: an error reading PDBs is treated as a
			// blocker so we don't proceed without disruption-budget data.
			return fmt.Sprintf("error listing PDBs in %s: %v", namespace, err)
		}
		for _, pdb := range pdbs.Items {
			if pdb.Status.DisruptionsAllowed > 0 {
				continue
			}
			selector := pdb.Spec.Selector
			if selector == nil {
				continue
			}
			for _, pod := range nsPods {
				if labelsMatchSelector(pod.Labels, selector.MatchLabels) {
					return pdb.Namespace + "/" + pdb.Name
				}
			}
		}
	}
	return ""
}

func labelsMatchSelector(podLabels, selector map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if podLabels[k] != v {
			return false
		}
	}
	return true
}
