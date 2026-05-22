package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Safe stabilization minimums applied for the cpu-hpa-low-request-sensitive
// rule. scaleUp clears the analyzer's 60s scale-up stabilization threshold;
// scaleDown matches the Kubernetes default scale-down window. The patch only
// raises these windows, so it never weakens an operator's existing setting.
const (
	hpaScaleUpStabilizationSeconds   = 60
	hpaScaleDownStabilizationSeconds = 300
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "api-yml-remediator: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	var file string
	var workload string
	var container string
	var ruleID string
	var cpuRequest string
	var memoryRequest string
	flags := flag.NewFlagSet("api-yml-remediator", flag.ContinueOnError)
	flags.StringVar(&file, "file", "", "api.yml path")
	flags.StringVar(&workload, "workload", "", "workload in Kind/name form, for example Deployment/echothread-api")
	flags.StringVar(&container, "container", "", "container name")
	flags.StringVar(&ruleID, "rule-id", "", "recommendation rule id")
	flags.StringVar(&cpuRequest, "cpu-request", "", "new CPU request, for example 100m")
	flags.StringVar(&memoryRequest, "memory-request", "", "new memory request, for example 256Mi")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if file == "" || workload == "" || ruleID == "" {
		return errors.New("--file, --workload, and --rule-id are required")
	}
	payload, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	docs, err := decodeYAMLDocuments(payload)
	if err != nil {
		return err
	}
	changed := false
	switch ruleID {
	case "cpu-request-over-provisioned", "memory-request-over-provisioned", "memory-request-below-usage":
		if container == "" {
			return errors.New("--container is required for resource request changes")
		}
		if cpuRequest == "" && memoryRequest == "" {
			return errors.New("at least one of --cpu-request or --memory-request is required")
		}
		changed, err = patchContainerRequests(docs, workload, container, cpuRequest, memoryRequest)
	case "single-replica-pdb-blocks-drain":
		changed, err = patchSingleReplicaPDB(docs, workload)
	case "cpu-hpa-low-request-sensitive":
		changed, err = patchHPASensitivity(docs, workload)
	default:
		return fmt.Errorf("unsupported rule id %q", ruleID)
	}
	if err != nil {
		return err
	}
	if !changed {
		return errors.New("no matching api.yml object was changed")
	}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	for _, doc := range docs {
		if isEmptyDocument(doc) {
			continue
		}
		if err := encoder.Encode(doc); err != nil {
			return err
		}
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	return os.WriteFile(file, out.Bytes(), 0o644)
}

func decodeYAMLDocuments(payload []byte) ([]*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(payload))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(doc.Content) == 0 {
			break
		}
		docs = append(docs, &doc)
	}
	return docs, nil
}

func patchContainerRequests(docs []*yaml.Node, workload, container, cpuRequest, memoryRequest string) (bool, error) {
	kind, name, ok := strings.Cut(workload, "/")
	if !ok {
		return false, fmt.Errorf("workload must be Kind/name, got %q", workload)
	}
	for _, doc := range docs {
		root := documentRoot(doc)
		if root == nil || scalarAt(root, "kind") != kind || scalarAt(mappingAt(root, "metadata"), "name") != name {
			continue
		}
		containers := sequenceAt(mappingAt(mappingAt(mappingAt(root, "spec"), "template"), "spec"), "containers")
		if containers == nil {
			return false, fmt.Errorf("%s has no spec.template.spec.containers", workload)
		}
		for _, item := range containers.Content {
			if scalarAt(item, "name") != container {
				continue
			}
			resources := ensureMapping(item, "resources")
			requests := ensureMapping(resources, "requests")
			if cpuRequest != "" {
				setScalar(requests, "cpu", cpuRequest)
			}
			if memoryRequest != "" {
				setScalar(requests, "memory", memoryRequest)
			}
			return true, nil
		}
		return false, fmt.Errorf("%s does not have container %q", workload, container)
	}
	return false, nil
}

func patchSingleReplicaPDB(docs []*yaml.Node, workload string) (bool, error) {
	_, name, ok := strings.Cut(workload, "/")
	if !ok {
		return false, fmt.Errorf("workload must be Kind/name, got %q", workload)
	}
	changed := false
	for _, doc := range docs {
		root := documentRoot(doc)
		if root == nil || scalarAt(root, "kind") != "PodDisruptionBudget" {
			continue
		}
		pdbName := scalarAt(mappingAt(root, "metadata"), "name")
		if !strings.Contains(pdbName, name) {
			continue
		}
		spec := ensureMapping(root, "spec")
		removeKey(spec, "minAvailable")
		setScalar(spec, "maxUnavailable", "1")
		changed = true
	}
	return changed, nil
}

// patchHPASensitivity stabilizes the HorizontalPodAutoscaler that targets the
// given workload by adding or raising its scale-up and scale-down
// stabilization windows. It only raises windows, so an operator's larger
// existing value is preserved. It does not change the CPU metric type or the
// workload's CPU request; those remain human-reviewed because they alter
// burst handling and must be checked against latency and error SLOs.
func patchHPASensitivity(docs []*yaml.Node, workload string) (bool, error) {
	kind, name, ok := strings.Cut(workload, "/")
	if !ok {
		return false, fmt.Errorf("workload must be Kind/name, got %q", workload)
	}
	for _, doc := range docs {
		root := documentRoot(doc)
		if root == nil || scalarAt(root, "kind") != "HorizontalPodAutoscaler" {
			continue
		}
		spec := mappingAt(root, "spec")
		if spec == nil {
			continue
		}
		scaleTargetRef := mappingAt(spec, "scaleTargetRef")
		if scaleTargetRef == nil || scalarAt(scaleTargetRef, "kind") != kind || scalarAt(scaleTargetRef, "name") != name {
			continue
		}
		behavior := ensureMapping(spec, "behavior")
		changedUp := raiseStabilization(ensureMapping(behavior, "scaleUp"), hpaScaleUpStabilizationSeconds)
		changedDown := raiseStabilization(ensureMapping(behavior, "scaleDown"), hpaScaleDownStabilizationSeconds)
		if !changedUp && !changedDown {
			return false, fmt.Errorf("%s autoscaler already has scale-up and scale-down stabilization at or above the safe minimum", workload)
		}
		return true, nil
	}
	return false, nil
}

// raiseStabilization sets stabilizationWindowSeconds to minimumSeconds unless
// it is already present and at least that large. It reports whether it changed
// the node.
func raiseStabilization(node *yaml.Node, minimumSeconds int) bool {
	if current, ok := intScalarAt(node, "stabilizationWindowSeconds"); ok && current >= minimumSeconds {
		return false
	}
	setIntScalar(node, "stabilizationWindowSeconds", minimumSeconds)
	return true
}

func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc == nil || len(doc.Content) == 0 {
		return nil
	}
	return doc.Content[0]
}

func isEmptyDocument(doc *yaml.Node) bool {
	return documentRoot(doc) == nil
}

func mappingAt(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(node.Content)-1; i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func sequenceAt(node *yaml.Node, key string) *yaml.Node {
	value := mappingAt(node, key)
	if value == nil || value.Kind != yaml.SequenceNode {
		return nil
	}
	return value
}

func scalarAt(node *yaml.Node, key string) string {
	value := mappingAt(node, key)
	if value == nil || value.Kind != yaml.ScalarNode {
		return ""
	}
	return value.Value
}

func ensureMapping(node *yaml.Node, key string) *yaml.Node {
	if existing := mappingAt(node, key); existing != nil {
		if existing.Kind != yaml.MappingNode {
			existing.Kind = yaml.MappingNode
			existing.Tag = "!!map"
			existing.Value = ""
			existing.Content = nil
		}
		return existing
	}
	child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		child,
	)
	return child
}

func setScalar(node *yaml.Node, key, value string) {
	for i := 0; i < len(node.Content)-1; i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
			return
		}
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value},
	)
}

func intScalarAt(node *yaml.Node, key string) (int, bool) {
	value := mappingAt(node, key)
	if value == nil || value.Kind != yaml.ScalarNode {
		return 0, false
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value.Value))
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func setIntScalar(node *yaml.Node, key string, value int) {
	scalar := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(value)}
	for i := 0; i < len(node.Content)-1; i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = scalar
			return
		}
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		scalar,
	)
}

func removeKey(node *yaml.Node, key string) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i < len(node.Content)-1; i += 2 {
		if node.Content[i].Value == key {
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
			return
		}
	}
}
