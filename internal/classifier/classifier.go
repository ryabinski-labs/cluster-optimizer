// Package classifier enriches analyzer findings with two safety-critical
// signals: provider_managed (the resource is owned by the cloud provider's
// control plane and must not be mutated by us) and remediable (a target is
// registered in remediation-targets.json that supports the rule).
//
// The classifier never mutates findings whose Workload/Namespace points at a
// provider-managed resource, and the live applier refuses to act on any
// finding that is not marked remediable. This is the single source of truth
// for what cluster-optimizer is allowed to touch.
package classifier

import (
	"encoding/json"
	"os"
	"strings"
)

// Target mirrors the entries in config/remediation-targets.json.
type Target struct {
	ClusterID        string   `json:"cluster_id"`
	Namespace        string   `json:"namespace"`
	Workload         string   `json:"workload"`
	Repository       string   `json:"repository,omitempty"`
	ManifestPath     string   `json:"manifest_path,omitempty"`
	InstructionsPath string   `json:"instructions_path,omitempty"`
	Container        string   `json:"container,omitempty"`
	SupportedRules   []string `json:"supported_rules,omitempty"`
}

type targetsFile struct {
	Targets []Target `json:"targets"`
}

// providerManagedNamespaces are namespaces whose contents are entirely owned
// by the DOKS control plane. We never propose live mutation in these.
var providerManagedNamespaces = map[string]bool{
	"kube-system":      true,
	"kube-public":      true,
	"kube-node-lease":  true,
	"cluster-optimizer": true, // our own namespace; out of scope for self-mutation
}

// providerManagedWorkloadNames lists DaemonSet/Deployment names the DOKS
// control plane reconciles. Even if they appear in a non-system namespace,
// editing them is futile (control plane reverts) and risky. This list is
// DOKS-tuned; extend it for other providers (EKS aws-node, ebs-csi-node,
// kube-proxy is already present; GKE: gke-metrics-agent, ip-masq-agent,
// fluentbit-gke; AKS: ama-logs, azure-cni-networkmonitor, etc.).
var providerManagedWorkloadNames = map[string]bool{
	"kube-proxy":                       true,
	"cilium":                           true,
	"cilium-operator":                  true,
	"csi-do-node":                      true,
	"csi-do-controller":                true,
	"do-node-agent":                    true,
	"doks-telemetry-config-reloader":   true,
	"doks-telemetry-fluent-bit":        true,
	"konnectivity-agent":               true,
	"hubble-relay":                     true,
	"hubble-ui":                        true,
	"coredns":                          true,
	"metrics-server":                   true,
	"cpc-bridge-proxy":                 true,
}

// Classifier evaluates whether a finding is provider-managed and/or
// remediable. Build once per run and reuse.
type Classifier struct {
	clusterID string
	byKey     map[string]Target // key: cluster/namespace/Kind/name
}

// New returns a Classifier initialised with the given remediation targets.
// Pass nil for none. Unknown clusterIDs are accepted: targets whose cluster_id
// matches are loaded and queryable; others are ignored.
func New(clusterID string, targets []Target) *Classifier {
	byKey := make(map[string]Target, len(targets))
	for _, target := range targets {
		if target.ClusterID != "" && target.ClusterID != clusterID {
			continue
		}
		byKey[targetKey(target.Namespace, target.Workload)] = target
	}
	return &Classifier{clusterID: clusterID, byKey: byKey}
}

// LoadTargets reads a targets file from disk and returns its entries. A
// missing file is not an error: it yields an empty slice. Malformed JSON is.
func LoadTargets(path string) ([]Target, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var file targetsFile
	if err := json.Unmarshal(payload, &file); err != nil {
		return nil, err
	}
	return file.Targets, nil
}

// IsProviderManaged reports whether the given namespace/workload pair points
// at a DOKS-controlled resource. workload is "Kind/name" as emitted by the
// analyzer; an empty workload (cluster-scoped finding) returns false.
func (c *Classifier) IsProviderManaged(namespace, workload string) bool {
	if providerManagedNamespaces[namespace] {
		return true
	}
	_, name, ok := splitWorkload(workload)
	if !ok {
		return false
	}
	return providerManagedWorkloadNames[name]
}

// IsRemediable reports whether a remediation target exists for the given
// workload that supports the given rule. Used by the planner to decide
// whether a PR-gated or live action is even applicable.
func (c *Classifier) IsRemediable(ruleID, namespace, workload string) bool {
	target, ok := c.TargetFor(namespace, workload)
	if !ok {
		return false
	}
	for _, supported := range target.SupportedRules {
		if supported == ruleID {
			return true
		}
	}
	return false
}

// TargetFor returns the remediation target entry for a workload, if one
// exists. Callers use this to find the container name and repo for PR
// generation.
func (c *Classifier) TargetFor(namespace, workload string) (Target, bool) {
	target, ok := c.byKey[targetKey(namespace, workload)]
	return target, ok
}

func targetKey(namespace, workload string) string {
	return namespace + "\x00" + workload
}

func splitWorkload(workload string) (kind, name string, ok bool) {
	if workload == "" {
		return "", "", false
	}
	idx := strings.IndexByte(workload, '/')
	if idx <= 0 || idx == len(workload)-1 {
		return "", "", false
	}
	return workload[:idx], workload[idx+1:], true
}
