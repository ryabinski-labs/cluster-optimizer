package analyzer

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

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
}

type Report struct {
	ClusterID   string                 `json:"cluster_id"`
	GeneratedAt time.Time              `json:"generated_at"`
	Summary     map[string]interface{} `json:"summary"`
	Findings    []Finding              `json:"findings"`
}

func Analyze(snapshot model.Snapshot) Report {
	findings := append([]Finding{}, analyzePDBs(snapshot.Workloads, snapshot.PDBs)...)
	findings = append(findings, analyzeHPAs(snapshot.Workloads, snapshot.HPAs)...)
	findings = append(findings, analyzeUsage(snapshot.Workloads)...)
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

func analyzeUsage(workloads []model.Workload) []Finding {
	var findings []Finding
	for _, workload := range workloads {
		if workload.UsageMemoryMiB != nil {
			usageMem := *workload.UsageMemoryMiB
			if workload.RequestsMemoryMiB > 0 && usageMem > workload.RequestsMemoryMiB*12/10 && usageMem-workload.RequestsMemoryMiB > 64 {
				findings = append(findings, finding("memory-request-below-usage", "high", workload,
					fmt.Sprintf("Observed memory %s exceeds request %s.", quantity.FormatMiB(usageMem), quantity.FormatMiB(workload.RequestsMemoryMiB)),
					"Raise memory request to at least observed p95 plus headroom before consolidation.",
					"Prevents false bin-packing that causes evictions; may increase requested capacity.",
					"Medium: request increases can delay node reduction but improve reliability.", "medium"))
			}
			if workload.RequestsMemoryMiB > 0 && usageMem < workload.RequestsMemoryMiB/2 && workload.RequestsMemoryMiB-usageMem > 128 {
				findings = append(findings, finding("memory-request-over-provisioned", "medium", workload,
					fmt.Sprintf("Observed memory %s is less than half of request %s.", quantity.FormatMiB(usageMem), quantity.FormatMiB(workload.RequestsMemoryMiB)),
					"Review multi-day p95/p99 memory and lower request only if peaks support it.",
					"May unlock bin-packing and node scale-down.",
					"Medium: memory reductions need peak and OOM evidence.", "low"))
			}
		}
		if workload.UsageCPUm != nil {
			usageCPU := *workload.UsageCPUm
			if usageCPU > 0 && workload.RequestsCPUm > 0 && usageCPU < workload.RequestsCPUm/5 && workload.RequestsCPUm-usageCPU > 100 {
				findings = append(findings, finding("cpu-request-over-provisioned", "medium", workload,
					fmt.Sprintf("Observed CPU %s is materially below request %s.", quantity.FormatCPU(usageCPU), quantity.FormatCPU(workload.RequestsCPUm)),
					"Lower CPU request after checking latency and throttling metrics.",
					"Improves schedulable CPU headroom and HPA sensitivity.",
					"Low/medium: CPU is compressible, but latency-sensitive paths need throttling checks.", "low"))
			}
		}
	}
	return findings
}

func analyzePDBs(workloads []model.Workload, pdbs []model.PDB) []Finding {
	var findings []Finding
	for _, pdb := range pdbs {
		for _, workload := range workloads {
			if pdb.Namespace != workload.Namespace || !selectorMatches(pdb.Selector, workload.Labels) {
				continue
			}
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

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
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
