package nudger

import (
	"context"
	"fmt"
	"log"
	"sort"

	"github.com/GipsyChef/cluster-optimizer/internal/applier"
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
}

// NewOptions returns Options with the same safe defaults as the applier
// (dry-run, halt switch at cluster-optimizer/cluster-optimizer-halt).
func NewOptions() Options {
	return Options{
		HaltNamespace: applier.DefaultHaltNamespace,
		HaltConfigMap: applier.DefaultHaltConfigMap,
		HaltKey:       applier.DefaultHaltKey,
	}
}

// NudgePods scans the cluster nodes and active pods, determines if any node's
// workloads can be fully consolidated/packed onto the remaining schedulable nodes,
// and if so, cordons the candidate node and evicts its pods.
//
// In dry-run mode (Options.Live=false, the default) it logs the plan and
// returns without mutating anything.
func NudgePods(ctx context.Context, clientset kubernetes.Interface, opts Options) error {
	mode := "DRY-RUN"
	if opts.Live {
		mode = "LIVE"
	}
	log.Printf("Active Nudger (%s): Starting cluster consolidation analysis...", mode)

	if opts.Live {
		if halted, reason := nudgerHaltCheck(ctx, clientset, opts); halted {
			log.Printf("Active Nudger: halt switch active (%s), refusing to cordon", reason)
			return nil
		}
	}

	// 1. Fetch all nodes
	nodeList, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list nodes: %w", err)
	}
	if len(nodeList.Items) < 2 {
		log.Println("Active Nudger: Cluster has fewer than 2 nodes. Consolidation not possible.")
		return nil
	}

	// 2. Fetch all pods across all namespaces
	podList, err := clientset.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	// 3. Define helper structures to model node capacity and resource request tracking
	type nodeState struct {
		node           corev1.Node
		name           string
		allocatableCPU int64 // millicores
		allocatableMem int64 // MiB
		requestedCPU   int64 // millicores
		requestedMem   int64 // MiB
		freeCPU        int64 // millicores
		freeMem        int64 // MiB
		isSchedulable  bool
		activePods     []corev1.Pod
	}

	nodesMap := make(map[string]*nodeState)
	for _, node := range nodeList.Items {
		cpu := node.Status.Allocatable.Cpu().MilliValue()
		mem := node.Status.Allocatable.Memory().Value() / 1024 / 1024 // bytes to MiB
		isSchedulable := !node.Spec.Unschedulable

		nodesMap[node.Name] = &nodeState{
			node:           node,
			name:           node.Name,
			allocatableCPU: cpu,
			allocatableMem: mem,
			isSchedulable:  isSchedulable,
			activePods:     []corev1.Pod{},
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
	var candidateNodes []*nodeState
	for _, ns := range nodesMap {
		if !ns.isSchedulable {
			continue // Node is already cordoned
		}
		candidateNodes = append(candidateNodes, ns)
	}

	if len(candidateNodes) < 2 {
		log.Println("Active Nudger: Less than 2 schedulable nodes. Consolidation not possible.")
		return nil
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
		return nil
	}

	log.Printf("Active Nudger (%s): Found consolidation opportunity. Node %q can be emptied. Relocatable pods to nudge: %d",
		mode, targetNodeToEmpty.name, len(podsToEvict))

	if !opts.Live {
		for _, pod := range podsToEvict {
			log.Printf("Active Nudger DRY-RUN: would evict pod %s/%s from node %q", pod.Namespace, pod.Name, targetNodeToEmpty.name)
		}
		log.Printf("Active Nudger DRY-RUN: would cordon node %q. Set CLUSTER_OPTIMIZER_NUDGE_LIVE=true to actually cordon and evict.", targetNodeToEmpty.name)
		return nil
	}

	// 8. Cordon the node to prevent new pods from scheduling on it
	nodeObj, err := clientset.CoreV1().Nodes().Get(ctx, targetNodeToEmpty.name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get node %q for cordoning: %w", targetNodeToEmpty.name, err)
	}

	if !nodeObj.Spec.Unschedulable {
		log.Printf("Active Nudger: Cordoning node %q...\n", targetNodeToEmpty.name)
		nodeObj.Spec.Unschedulable = true
		_, err = clientset.CoreV1().Nodes().Update(ctx, nodeObj, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to cordon node %q: %w", targetNodeToEmpty.name, err)
		}
		log.Printf("Active Nudger: Node %q cordoned successfully.\n", targetNodeToEmpty.name)
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
		} else {
			log.Printf("Active Nudger: Pod %s/%s evicted successfully.\n", pod.Namespace, pod.Name)
		}
	}

	log.Printf("Active Nudger: Consolidation of node %q initiated successfully.\n", targetNodeToEmpty.name)
	return nil
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
