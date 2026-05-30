package analyzer

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GipsyChef/cluster-optimizer/internal/classifier"
	"github.com/GipsyChef/cluster-optimizer/internal/model"
	"github.com/GipsyChef/cluster-optimizer/internal/quantity"
)

type Finding struct {
	RuleID             string            `json:"rule_id"`
	Severity           string            `json:"severity"`
	Namespace          string            `json:"namespace,omitempty"`
	Workload           string            `json:"workload,omitempty"`
	Evidence           string            `json:"evidence"`
	Recommendation     string            `json:"recommendation"`
	ExpectedCostEffect string            `json:"expected_cost_effect"`
	Risk               string            `json:"risk"`
	Confidence         string            `json:"confidence"`
	Pillars            map[string]string `json:"pillars"`
	// ProviderManaged is true when the underlying resource is reconciled by
	// the cloud provider's control plane (e.g. DOKS-managed DaemonSets).
	// Remediators must never mutate these.
	ProviderManaged bool `json:"provider_managed,omitempty"`
	// Remediable is true when a target in remediation-targets.json supports
	// this rule for this workload. Live applier consumes only remediable
	// findings.
	Remediable bool `json:"remediable,omitempty"`
}

type Report struct {
	ClusterID   string                 `json:"cluster_id"`
	GeneratedAt time.Time              `json:"generated_at"`
	Summary     map[string]interface{} `json:"summary"`
	Findings    []Finding              `json:"findings"`
}

// Analyze runs the rule pipelines and returns a Report. It does not tag
// findings with provider_managed / remediable; callers that want those
// classifications should call AnalyzeWith with a configured Classifier.
func Analyze(snapshot model.Snapshot) Report {
	return AnalyzeWith(snapshot, nil)
}

// AnalyzeWith runs the rule pipelines and tags each finding using the
// supplied Classifier. If classifier is nil, findings are returned untagged
// (preserving prior behaviour for tests and callers that haven't migrated).
func AnalyzeWith(snapshot model.Snapshot, c *classifier.Classifier) Report {
	findings := append([]Finding{}, analyzePDBs(snapshot.Workloads, snapshot.PDBs)...)
	findings = append(findings, analyzeHPAs(snapshot.Workloads, snapshot.HPAs)...)
	findings = append(findings, analyzeHPASensitivity(snapshot.Workloads, snapshot.HPAs)...)
	findings = append(findings, analyzeUsage(snapshot.Workloads, snapshot.HPAs)...)
	findings = append(findings, analyzeScaling(snapshot.Workloads, snapshot.HPAs)...)
	findings = append(findings, analyzeRuntimeModernization(snapshot.Workloads)...)
	findings = append(findings, analyzeDaemonSetOverhead(snapshot)...)
	findings = append(findings, analyzeClusterHygiene(snapshot.Pods)...)
	if c != nil {
		for i := range findings {
			findings[i].ProviderManaged = c.IsProviderManaged(findings[i].Namespace, findings[i].Workload)
			findings[i].Remediable = !findings[i].ProviderManaged &&
				c.IsRemediable(findings[i].RuleID, findings[i].Namespace, findings[i].Workload)
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		return findingRank(findings[i]) < findingRank(findings[j])
	})
	return Report{
		ClusterID:   snapshot.ClusterID,
		GeneratedAt: time.Now().UTC(),
		Summary:     summary(snapshot),
		Findings:    findings,
	}
}

func summary(snapshot model.Snapshot) map[string]interface{} {
	var allocCPU, allocMem, reqCPU, reqMem, usageCPU, usageMem, dsCPU, dsMem int64
	var sawUsageCPU, sawUsageMem bool
	activePods := 0
	instanceTypes := map[string]bool{}
	for _, node := range snapshot.Nodes {
		allocCPU += node.AllocatableCPUm
		allocMem += node.AllocatableMemoryMiB
		if node.InstanceType != "" {
			instanceTypes[node.InstanceType] = true
		}
	}
	for _, pod := range snapshot.Pods {
		if pod.Phase == "Succeeded" || pod.Phase == "Failed" {
			continue
		}
		activePods++
		reqCPU += pod.RequestsCPUm
		reqMem += pod.RequestsMemoryMiB
		if pod.UsageCPUm != nil {
			usageCPU += *pod.UsageCPUm
			sawUsageCPU = true
		}
		if pod.UsageMemoryMiB != nil {
			usageMem += *pod.UsageMemoryMiB
			sawUsageMem = true
		}
		if pod.OwnerKind == "DaemonSet" {
			dsCPU += pod.RequestsCPUm
			dsMem += pod.RequestsMemoryMiB
		}
	}
	types := make([]string, 0, len(instanceTypes))
	for instanceType := range instanceTypes {
		types = append(types, instanceType)
	}
	sort.Strings(types)
	return map[string]interface{}{
		"node_count":                     len(snapshot.Nodes),
		"instance_types":                 types,
		"active_pods":                    activePods,
		"allocatable_cpu_m":              allocCPU,
		"allocatable_memory_mib":         allocMem,
		"requested_cpu_m":                reqCPU,
		"requested_memory_mib":           reqMem,
		"observed_cpu_m":                 optionalMetric(usageCPU, sawUsageCPU),
		"observed_memory_mib":            optionalMetric(usageMem, sawUsageMem),
		"daemonset_requested_cpu_m":      dsCPU,
		"daemonset_requested_memory_mib": dsMem,
		"two_node_estimate":              twoNodeEstimate(snapshot, reqCPU, reqMem, dsCPU, dsMem),
	}
}

func twoNodeEstimate(snapshot model.Snapshot, reqCPU, reqMem, dsCPU, dsMem int64) map[string]interface{} {
	nodeCount := int64(len(snapshot.Nodes))
	if nodeCount < 2 {
		return map[string]interface{}{"feasible": false, "reason": "cluster has fewer than two nodes"}
	}
	var allocCPU, allocMem int64
	for _, node := range snapshot.Nodes {
		allocCPU += node.AllocatableCPUm
		allocMem += node.AllocatableMemoryMiB
	}
	avgCPU := allocCPU / nodeCount
	avgMem := allocMem / nodeCount
	avgDSCPU := dsCPU / nodeCount
	avgDSMem := dsMem / nodeCount
	projectedCPU := reqCPU - max(nodeCount-2, 0)*avgDSCPU
	projectedMem := reqMem - max(nodeCount-2, 0)*avgDSMem
	targetCPU := 2 * avgCPU
	targetMem := 2 * avgMem
	minCPUHeadroom := max(250, targetCPU/10)
	minMemHeadroom := max(512, targetMem/10)
	cpuHeadroom := targetCPU - projectedCPU
	memHeadroom := targetMem - projectedMem
	return map[string]interface{}{
		"feasible":                       cpuHeadroom >= minCPUHeadroom && memHeadroom >= minMemHeadroom,
		"projected_requested_cpu_m":      projectedCPU,
		"projected_requested_memory_mib": projectedMem,
		"target_allocatable_cpu_m":       targetCPU,
		"target_allocatable_memory_mib":  targetMem,
		"cpu_headroom_m":                 cpuHeadroom,
		"memory_headroom_mib":            memHeadroom,
		"minimum_cpu_headroom_m":         minCPUHeadroom,
		"minimum_memory_headroom_mib":    minMemHeadroom,
	}
}

func analyzeUsage(workloads []model.Workload, hpas []model.HPA) []Finding {
	var findings []Finding
	for _, workload := range workloads {
		// Requests and usage in the snapshot are fleet totals summed across
		// every replica (see collector.aggregatePods). Compare on a per-replica
		// basis so a workload whose per-pod request is already reasonable is not
		// flagged as over-provisioned purely because it runs several replicas
		// (e.g. 2 replicas x 128Mi reads as a 256Mi request). The remediation
		// target derived from this evidence and the patch the planner applies
		// are both per container, so the evidence must be per replica too; this
		// also mirrors the per-replica math already used by
		// analyzeRuntimeModernization, analyzeHPASensitivity, and
		// plan.planRequestTrim. Without a known replica count the per-pod values
		// are undefined, so skip the workload.
		replicas := int64(workload.Replicas)
		if replicas < 1 {
			continue
		}
		if workload.UsageMemoryMiB != nil {
			usageMem := *workload.UsageMemoryMiB / replicas
			requestMem := workload.RequestsMemoryMiB / replicas
			if requestMem > 0 && usageMem > requestMem*12/10 && usageMem-requestMem > 64 {
				findings = append(findings, finding("memory-request-below-usage", "high", workload,
					fmt.Sprintf("Observed memory %s exceeds request %s.", quantity.FormatMiB(usageMem), quantity.FormatMiB(requestMem)),
					"Raise memory request to at least observed p95 plus headroom before consolidation.",
					"Prevents false bin-packing that causes evictions; may increase requested capacity.",
					"Medium: request increases can delay node reduction but improve reliability.", "medium"))
			}
			if requestMem > 0 && usageMem < requestMem/2 && requestMem-usageMem > 128 {
				findings = append(findings, finding("memory-request-over-provisioned", "medium", workload,
					fmt.Sprintf("Observed memory %s is less than half of request %s.", quantity.FormatMiB(usageMem), quantity.FormatMiB(requestMem)),
					"Review multi-day p95/p99 memory and lower request only if peaks support it.",
					"May unlock bin-packing and node scale-down.",
					"Medium: memory reductions need peak and OOM evidence.", "low"))
			}
		}
		if workload.UsageCPUm != nil {
			usageCPU := *workload.UsageCPUm / replicas
			requestCPU := workload.RequestsCPUm / replicas
			if usageCPU > 0 && requestCPU > 0 && usageCPU < requestCPU/5 && requestCPU-usageCPU > 100 {
				recommendation := "Lower CPU request after checking latency and throttling metrics."
				if hpa := cpuUtilizationHPAForWorkload(workload, hpas); hpa != nil {
					recommendation = "Lower CPU request only with matching HPA retuning; preserve the intended scale-up point with an absolute averageValue target or a higher CPU request, then validate latency and throttling metrics."
				}
				findings = append(findings, finding("cpu-request-over-provisioned", "medium", workload,
					fmt.Sprintf("Observed CPU %s is materially below request %s.", quantity.FormatCPU(usageCPU), quantity.FormatCPU(requestCPU)),
					recommendation,
					"Improves schedulable CPU headroom and HPA sensitivity.",
					"Low/medium: CPU is compressible, but latency-sensitive paths need throttling checks.", "low"))
			}
		}
	}
	return findings
}

func analyzeHPASensitivity(workloads []model.Workload, hpas []model.HPA) []Finding {
	byKey := map[string]model.Workload{}
	for _, workload := range workloads {
		byKey[workloadKey(workload)] = workload
	}
	var findings []Finding
	for _, hpa := range hpas {
		if hpa.CPUUtilizationTarget == nil || hpa.CPUAverageValueTargetm != nil || hpa.MaxReplicas <= hpa.MinReplicas {
			continue
		}
		workload := byKey[hpa.Namespace+"/"+hpa.TargetKind+"/"+hpa.TargetName]
		if workload.Name == "" || workload.Replicas <= 0 || workload.RequestsCPUm <= 0 {
			continue
		}
		perReplicaRequest := workload.RequestsCPUm / int64(workload.Replicas)
		scaleThreshold := perReplicaRequest * int64(*hpa.CPUUtilizationTarget) / 100
		if scaleThreshold >= 100 && hasScaleUpStabilization(hpa, 60) {
			continue
		}
		evidence := fmt.Sprintf("HPA %s targets %d%% CPU on %s per-replica request, so scale-up can start around %s CPU per pod.",
			hpa.Name, *hpa.CPUUtilizationTarget, quantity.FormatCPU(perReplicaRequest), quantity.FormatCPU(scaleThreshold))
		if hpa.ScaleUpStabilizationSeconds == nil {
			evidence += " No scale-up stabilization window is configured."
		} else {
			evidence += fmt.Sprintf(" Scale-up stabilization is %ds.", *hpa.ScaleUpStabilizationSeconds)
		}
		findings = append(findings, finding("cpu-hpa-low-request-sensitive", "medium", workload, evidence,
			"Use an absolute CPU averageValue target or raise the CPU request so the scale-up point reflects sustained load; add HPA scale-up and scale-down stabilization before lowering requests further.",
			"Prevents request tuning from causing replica churn or avoidable node scale-ups.",
			"Medium: HPA changes alter burst handling and must be checked against latency/error SLOs.", "medium"))
	}
	return findings
}

func analyzePDBs(workloads []model.Workload, pdbs []model.PDB) []Finding {
	var findings []Finding
	matchedPDB := map[string]bool{}
	for _, pdb := range pdbs {
		for _, workload := range workloads {
			if pdb.Namespace != workload.Namespace || !selectorMatches(pdb.Selector, workload.Labels) {
				continue
			}
			matchedPDB[workloadKey(workload)] = true
			minAvailable := availabilityCount(pdb.MinAvailable, workload.Replicas)
			maxUnavailable := availabilityCount(pdb.MaxUnavailable, workload.Replicas)
			if workload.Replicas <= 1 && minAvailable != nil && *minAvailable >= 1 {
				findings = append(findings, finding("single-replica-pdb-blocks-drain", "high", workload,
					fmt.Sprintf("PDB %s has minAvailable=%s for %d ready replica.", pdb.Name, pdb.MinAvailable, workload.Replicas),
					"Use maxUnavailable: 1 if voluntary downtime is acceptable, or run two replicas.",
					"Unblocks autoscaler scale-down and node maintenance.",
					"Medium/high: this accepts singleton voluntary downtime unless replicas increase.", "high"))
			}
			if maxUnavailable != nil && *maxUnavailable == 0 {
				findings = append(findings, finding("pdb-max-unavailable-zero", "high", workload,
					fmt.Sprintf("PDB %s has maxUnavailable=0.", pdb.Name),
					"Allow at least one voluntary disruption or increase replicas to preserve availability.",
					"Unblocks drains that currently cannot evict matching pods.",
					"Medium: must align with workload availability requirements.", "high"))
			}
			if workload.Replicas > 1 && minAvailable != nil && *minAvailable == 0 {
				findings = append(findings, finding("pdb-allows-full-disruption", "medium", workload,
					fmt.Sprintf("PDB %s has minAvailable=0 for %d replicas.", pdb.Name, workload.Replicas),
					"Prefer maxUnavailable: 1 so drains cannot voluntarily evict every replica together.",
					"Maintains consolidation while improving reliability during maintenance.",
					"Low: stricter PDB can slow drains but protects service availability.", "high"))
			}
		}
	}
	for _, workload := range workloads {
		if workload.Kind == "DaemonSet" || workload.Replicas < 2 || matchedPDB[workloadKey(workload)] {
			continue
		}
		findings = append(findings, finding("missing-pdb-for-multi-replica-workload", "medium", workload,
			fmt.Sprintf("%s has %d ready replicas and no matching PDB.", workload.Name, workload.Replicas),
			"Add a PDB such as maxUnavailable: 1 so maintenance and consolidation preserve at least one healthy replica.",
			"Supports safe drains and autoscaler consolidation without full voluntary disruption.",
			"Low/medium: stricter disruption policy can slow drains if replicas are unhealthy.", "medium"))
	}
	return findings
}

func analyzeHPAs(workloads []model.Workload, hpas []model.HPA) []Finding {
	byKey := map[string]model.Workload{}
	for _, workload := range workloads {
		byKey[workload.Namespace+"/"+workload.Kind+"/"+workload.Name] = workload
	}
	var findings []Finding
	for _, hpa := range hpas {
		if hpa.MinReplicas == hpa.MaxReplicas {
			findings = append(findings, Finding{
				RuleID: "hpa-min-equals-max", Severity: "low", Namespace: hpa.Namespace, Workload: "HPA/" + hpa.Name,
				Evidence:           fmt.Sprintf("HPA %s has minReplicas=maxReplicas=%d.", hpa.Name, hpa.MinReplicas),
				Recommendation:     "Remove the HPA or widen the replica range so it can influence capacity.",
				ExpectedCostEffect: "Reduces control-plane noise; no direct node saving unless range changes.",
				Risk:               "Low.", Confidence: "high", Pillars: pillars(),
			})
		}
		workload := byKey[hpa.Namespace+"/"+hpa.TargetKind+"/"+hpa.TargetName]
		if contains(hpa.Metrics, "cpu") && workload.Name != "" && workload.RequestsCPUm == 0 {
			findings = append(findings, finding("cpu-hpa-without-cpu-request", "high", workload,
				fmt.Sprintf("HPA %s targets CPU but matching workload has no CPU request.", hpa.Name),
				"Set CPU requests or use a metric that reflects the workload bottleneck.",
				"Makes autoscaling behavior predictable and avoids hidden capacity risk.",
				"Medium: changing requests affects HPA utilization math.", "high"))
		}
	}
	return findings
}

func analyzeScaling(workloads []model.Workload, hpas []model.HPA) []Finding {
	hpaTargets := map[string]bool{}
	for _, hpa := range hpas {
		hpaTargets[hpa.Namespace+"/"+hpa.TargetKind+"/"+hpa.TargetName] = true
	}
	var findings []Finding
	for _, workload := range workloads {
		if workload.Kind == "DaemonSet" || workload.Replicas < 2 || systemNamespace(workload.Namespace) || hpaTargets[workloadKey(workload)] {
			continue
		}
		findings = append(findings, finding("fixed-replica-capacity-without-autoscaler", "medium", workload,
			fmt.Sprintf("%s has %d ready replicas and no HPA.", workload.Name, workload.Replicas),
			"Validate whether demand is steady; otherwise add HPA/KEDA or lower the minimum replica count after SLO review.",
			"Can reduce idle replicas or make capacity elastic during quiet periods.",
			"Medium: lowering replica floors can affect availability, cold starts, and burst handling.", "low"))
	}
	return findings
}

func analyzeRuntimeModernization(workloads []model.Workload) []Finding {
	var findings []Finding
	for _, workload := range workloads {
		if workload.Kind == "DaemonSet" || workload.Replicas == 0 || systemNamespace(workload.Namespace) {
			continue
		}
		perReplicaRequestMem := workload.RequestsMemoryMiB / int64(workload.Replicas)
		perReplicaUsageMem := int64(0)
		if workload.UsageMemoryMiB != nil {
			perReplicaUsageMem = *workload.UsageMemoryMiB / int64(workload.Replicas)
		}
		if len(workload.RuntimeHints) == 0 {
			if perReplicaRequestMem < 384 && perReplicaUsageMem < 256 {
				continue
			}
			findings = append(findings, finding("runtime-modernization-candidate", "low", workload,
				fmt.Sprintf("Runtime was not detected; per-replica request is %s and observed memory is %s.", quantity.FormatMiB(perReplicaRequestMem), quantity.FormatMiB(perReplicaUsageMem)),
				"Identify the implementation runtime and profile memory-heavy always-on paths; if they are interpreted or framework-heavy, evaluate a Go or Rust rewrite for the hot path before making it a cost project.",
				"Can reduce per-pod memory footprint and unlock bin-packing when request tuning alone is not enough.",
				"High: rewrites are product work; require profiling, load testing, canary rollout, and rollback to the existing implementation.", "low"))
			continue
		}
		if contains(workload.RuntimeHints, "go") || contains(workload.RuntimeHints, "rust") || !runtimeRewriteCandidate(workload.RuntimeHints, perReplicaRequestMem, perReplicaUsageMem) {
			continue
		}
		target := "Go or Rust"
		if contains(workload.RuntimeHints, "browser/chromium") {
			target = "an on-demand browser worker model, pool right-sizing, or a smaller specialized service"
		}
		findings = append(findings, finding("runtime-modernization-candidate", "low", workload,
			fmt.Sprintf("Detected runtime hints %v with per-replica request %s and observed memory %s.", workload.RuntimeHints, quantity.FormatMiB(perReplicaRequestMem), quantity.FormatMiB(perReplicaUsageMem)),
			fmt.Sprintf("Profile the workload and evaluate moving memory-heavy always-on paths to %s before treating a rewrite as a cost project.", target),
			"Can reduce per-pod memory footprint and unlock bin-packing when request tuning alone is not enough.",
			"High: rewrites are product work; require profiling, load testing, canary rollout, and rollback to the existing implementation.", "low"))
	}
	return findings
}

func analyzeDaemonSetOverhead(snapshot model.Snapshot) []Finding {
	nodeCount := int64(len(snapshot.Nodes))
	if nodeCount == 0 {
		return nil
	}
	var allocCPU, allocMem, dsCPU, dsMem int64
	for _, node := range snapshot.Nodes {
		allocCPU += node.AllocatableCPUm
		allocMem += node.AllocatableMemoryMiB
	}
	for _, pod := range snapshot.Pods {
		if pod.Phase == "Succeeded" || pod.Phase == "Failed" || pod.OwnerKind != "DaemonSet" {
			continue
		}
		dsCPU += pod.RequestsCPUm
		dsMem += pod.RequestsMemoryMiB
	}
	if allocCPU == 0 || allocMem == 0 {
		return nil
	}
	var findings []Finding
	if dsMem*100/allocMem >= 15 || dsCPU*100/allocCPU >= 20 {
		findings = append(findings, Finding{
			RuleID:             "daemonset-overhead-limits-small-node-efficiency",
			Severity:           "medium",
			Evidence:           fmt.Sprintf("DaemonSets request %s CPU and %s memory across %d nodes.", quantity.FormatCPU(dsCPU), quantity.FormatMiB(dsMem), nodeCount),
			Recommendation:     "Review provider and observability DaemonSet requests before moving to smaller node counts or smaller node shapes.",
			ExpectedCostEffect: "May improve bin-packing or show that fewer larger nodes are more efficient than many small nodes.",
			Risk:               "Medium: system DaemonSets affect networking, logging, and node health; prefer provider-supported tuning only.",
			Confidence:         "medium",
			Pillars:            pillars(),
		})
	}
	return findings
}

func analyzeClusterHygiene(pods []model.Pod) []Finding {
	var findings []Finding
	for _, pod := range pods {
		if pod.Phase == "Succeeded" || pod.Phase == "Failed" {
			continue
		}
		if strings.HasPrefix(pod.Name, "cm-acme-http-solver-") || pod.Labels["acme.cert-manager.io/http01-solver"] == "true" {
			findings = append(findings, Finding{
				RuleID:             "cert-manager-http01-solver-running",
				Severity:           "low",
				Namespace:          pod.Namespace,
				Workload:           "Pod/" + pod.Name,
				Evidence:           "Active cert-manager HTTP-01 solver pod found.",
				Recommendation:     "Verify the matching Challenge completes quickly; investigate stuck solver pods or fragmented certificate maintenance if these persist.",
				ExpectedCostEffect: "Usually small, but reduces transient scheduling pressure and operational noise.",
				Risk:               "Low: inspect Certificate, Order, and Challenge state before changing cert-manager resources.",
				Confidence:         "medium",
				Pillars:            pillars(),
			})
		}
	}
	return findings
}

func finding(ruleID, severity string, workload model.Workload, evidence, recommendation, effect, risk, confidence string) Finding {
	return Finding{
		RuleID: ruleID, Severity: severity, Namespace: workload.Namespace, Workload: workload.Kind + "/" + workload.Name,
		Evidence: evidence, Recommendation: recommendation, ExpectedCostEffect: effect, Risk: risk, Confidence: confidence, Pillars: pillars(),
	}
}

func pillars() map[string]string {
	return map[string]string{
		"operational_excellence": "Recommendation includes validation context and avoids hidden automation.",
		"security":               "No mutation or secret value access required.",
		"reliability":            "Avoids changes that hide real capacity needs or block safe drains.",
		"performance_efficiency": "Uses requests and observed usage to improve scheduling fit.",
		"cost_optimization":      "Targets wasted requests, blocked consolidation, and scale inefficiency.",
		"sustainability":         "Reduces idle compute where reliability evidence supports it.",
	}
}

func selectorMatches(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func availabilityCount(value string, replicas int32) *int32 {
	if value == "" {
		return nil
	}
	if strings.HasSuffix(value, "%") {
		n, err := strconv.Atoi(strings.TrimSuffix(value, "%"))
		if err != nil {
			return nil
		}
		result := int32(math.Ceil(float64(replicas) * float64(n) / 100.0))
		return &result
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return nil
	}
	result := int32(n)
	return &result
}

func cpuUtilizationHPAForWorkload(workload model.Workload, hpas []model.HPA) *model.HPA {
	key := workloadKey(workload)
	for i := range hpas {
		hpa := &hpas[i]
		if hpa.Namespace+"/"+hpa.TargetKind+"/"+hpa.TargetName == key && hpa.CPUUtilizationTarget != nil {
			return hpa
		}
	}
	return nil
}

func hasScaleUpStabilization(hpa model.HPA, minimumSeconds int32) bool {
	return hpa.ScaleUpStabilizationSeconds != nil && *hpa.ScaleUpStabilizationSeconds >= minimumSeconds
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func runtimeRewriteCandidate(hints []string, perReplicaRequestMem, perReplicaUsageMem int64) bool {
	if contains(hints, "browser/chromium") {
		return perReplicaRequestMem >= 512 || perReplicaUsageMem >= 384
	}
	if contains(hints, "jvm") {
		return perReplicaRequestMem >= 512 || perReplicaUsageMem >= 384
	}
	if contains(hints, "nodejs") || contains(hints, "python") || contains(hints, "ruby") || contains(hints, "php") {
		return perReplicaRequestMem >= 256 || perReplicaUsageMem >= 192
	}
	return false
}

func systemNamespace(namespace string) bool {
	return namespace == "kube-system" || namespace == "kube-public" || namespace == "kube-node-lease" || namespace == "ingress-nginx"
}

func workloadKey(workload model.Workload) string {
	return workload.Namespace + "/" + workload.Kind + "/" + workload.Name
}

func findingRank(f Finding) int {
	rank := map[string]int{"high": 0, "medium": 1000, "low": 2000}
	return rank[f.Severity]
}

func optionalMetric(value int64, present bool) interface{} {
	if !present {
		return nil
	}
	return value
}

func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
