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
	// RequiresNamespaceOptIn is derived when a namespace wildcard authorizes
	// a rule. It is never read from or written to target files.
	RequiresNamespaceOptIn bool `json:"-"`
}

const (
	NamespaceRemediationLabel = "cluster-optimizer.io/remediation"
	NamespaceRemediationValue = "enabled"
)

type targetsFile struct {
	Targets []Target `json:"targets"`
}

// providerManagedNamespaces are namespaces whose contents are owned by the
// Kubernetes platform or its cluster add-ons. We never propose live mutation
// in these, including through wildcard remediation targets.
var providerManagedNamespaces = map[string]bool{
	"cert-manager":      true,
	"ingress-nginx":     true,
	"kube-system":       true,
	"kube-public":       true,
	"kube-node-lease":   true,
	"cluster-optimizer": true, // our own namespace; out of scope for self-mutation
}

// providerManagedWorkloadNames lists DaemonSet/Deployment names the DOKS
// control plane reconciles. Even if they appear in a non-system namespace,
// editing them is futile (control plane reverts) and risky. This list is
// DOKS-tuned; extend it for other providers (EKS aws-node, ebs-csi-node,
// kube-proxy is already present; GKE: gke-metrics-agent, ip-masq-agent,
// fluentbit-gke; AKS: ama-logs, azure-cni-networkmonitor, etc.).
var providerManagedWorkloadNames = map[string]bool{
	"kube-proxy":                     true,
	"cilium":                         true,
	"cilium-operator":                true,
	"csi-do-node":                    true,
	"csi-do-controller":              true,
	"do-node-agent":                  true,
	"doks-telemetry-config-reloader": true,
	"doks-telemetry-fluent-bit":      true,
	"konnectivity-agent":             true,
	"hubble-relay":                   true,
	"hubble-ui":                      true,
	"coredns":                        true,
	"metrics-server":                 true,
	"cpc-bridge-proxy":               true,
}

// Classifier evaluates whether a finding is provider-managed and/or
// remediable. Build once per run and reuse.
type Classifier struct {
	clusterID          string
	byKey              map[string]Target // key: cluster/namespace/Kind/name
	wildcardNamespaces map[string]bool
}

// New returns a Classifier initialised with the given remediation targets.
// Pass nil for none. Unknown clusterIDs are accepted: targets whose cluster_id
// matches are loaded and queryable; others are ignored.
func New(clusterID string, targets []Target) *Classifier {
	return NewWithNamespaceLabels(clusterID, targets, nil)
}

// NewWithNamespaceLabels enables namespace wildcard targets only for
// namespaces carrying the explicit remediation opt-in label. Exact namespace
// targets remain explicit authorization and do not require the label.
func NewWithNamespaceLabels(clusterID string, targets []Target, namespaceLabels map[string]map[string]string) *Classifier {
	byKey := make(map[string]Target, len(targets))
	for _, target := range targets {
		if target.ClusterID != "" && target.ClusterID != clusterID {
			continue
		}
		byKey[targetKey(target.Namespace, target.Workload)] = target
	}
	wildcardNamespaces := make(map[string]bool, len(namespaceLabels))
	for namespace, labels := range namespaceLabels {
		wildcardNamespaces[namespace] = labels[NamespaceRemediationLabel] == NamespaceRemediationValue
	}
	return &Classifier{clusterID: clusterID, byKey: byKey, wildcardNamespaces: wildcardNamespaces}
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
// at a platform-controlled resource. workload is "Kind/name" as emitted by
// the analyzer.
func (c *Classifier) IsProviderManaged(namespace, workload string) bool {
	return IsProviderManaged(namespace, workload)
}

// IsProviderManaged is the package-level safety check used by both planning
// and execution. Keeping the applier on the same deny rules provides a second
// guard if a malformed or externally constructed plan bypasses classification.
func IsProviderManaged(namespace, workload string) bool {
	if providerManagedNamespaces[namespace] ||
		strings.HasPrefix(namespace, "kube-") ||
		strings.HasSuffix(namespace, "-system") ||
		strings.HasSuffix(namespace, "-operator") {
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
	_, ok := c.TargetForRule(ruleID, namespace, workload)
	return ok
}

// TargetFor returns the most specific remediation target for a workload.
// Exact mappings win over namespace or workload wildcards, and the global
// */* target is the final fallback.
func (c *Classifier) TargetFor(namespace, workload string) (Target, bool) {
	return c.effectiveTarget("", namespace, workload)
}

// TargetForRule resolves one effective target. More-specific metadata wins,
// while less-specific wildcard entries provide defaults and supported rules.
func (c *Classifier) TargetForRule(ruleID, namespace, workload string) (Target, bool) {
	return c.effectiveTarget(ruleID, namespace, workload)
}

func (c *Classifier) effectiveTarget(ruleID, namespace, workload string) (Target, bool) {
	keys := []string{
		targetKey(namespace, workload),
		targetKey(namespace, "*"),
	}
	if c.wildcardNamespaces[namespace] {
		keys = append(keys, targetKey("*", workload), targetKey("*", "*"))
	}
	var effective Target
	found := false
	ruleSupported := ruleID == ""
	for _, key := range keys {
		target, ok := c.byKey[key]
		if !ok {
			continue
		}
		if !found {
			effective = target
			found = true
		} else {
			effective = mergeTargetDefaults(effective, target)
		}
		for _, supported := range target.SupportedRules {
			if supported == ruleID && !ruleSupported {
				ruleSupported = true
				if target.Namespace == "*" {
					effective.RequiresNamespaceOptIn = true
				}
			}
		}
	}
	return effective, found && ruleSupported
}

func mergeTargetDefaults(target, defaults Target) Target {
	if target.Repository == "" {
		target.Repository = defaults.Repository
	}
	if target.ManifestPath == "" {
		target.ManifestPath = defaults.ManifestPath
	}
	if target.InstructionsPath == "" {
		target.InstructionsPath = defaults.InstructionsPath
	}
	if target.Container == "" {
		target.Container = defaults.Container
	}
	return target
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
