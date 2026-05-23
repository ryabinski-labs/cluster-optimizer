package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const hpaWithoutBehavior = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: nightlamp-api
spec:
  replicas: 2
  template:
    spec:
      containers:
        - name: nightlamp-api
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: nightlamp-api
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: nightlamp-api
  minReplicas: 2
  maxReplicas: 10
  metrics:
    - type: Resource
      resource:
        name: cpu
        target:
          type: Utilization
          averageUtilization: 70
`

const hpaWithLowScaleUp = `apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: nightlamp-api
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: nightlamp-api
  minReplicas: 2
  maxReplicas: 10
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 30
`

const hpaWithLargeWindows = `apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: nightlamp-api
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: nightlamp-api
  minReplicas: 2
  maxReplicas: 10
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 120
    scaleDown:
      stabilizationWindowSeconds: 600
`

const hpaForOtherWorkload = `apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: other-api
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: other-api
  minReplicas: 2
  maxReplicas: 10
`

func writeTempManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp manifest: %v", err)
	}
	return path
}

func readHPAStabilization(t *testing.T, path string) (scaleUp, scaleDown int) {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read patched manifest: %v", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(payload))
	for {
		var doc struct {
			Kind string `yaml:"kind"`
			Spec struct {
				Behavior struct {
					ScaleUp struct {
						StabilizationWindowSeconds int `yaml:"stabilizationWindowSeconds"`
					} `yaml:"scaleUp"`
					ScaleDown struct {
						StabilizationWindowSeconds int `yaml:"stabilizationWindowSeconds"`
					} `yaml:"scaleDown"`
				} `yaml:"behavior"`
			} `yaml:"spec"`
		}
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode patched manifest: %v", err)
		}
		if doc.Kind == "HorizontalPodAutoscaler" {
			return doc.Spec.Behavior.ScaleUp.StabilizationWindowSeconds, doc.Spec.Behavior.ScaleDown.StabilizationWindowSeconds
		}
	}
	t.Fatalf("no HorizontalPodAutoscaler found in %s", path)
	return 0, 0
}

func TestRunAddsHPAStabilizationWindows(t *testing.T) {
	path := writeTempManifest(t, hpaWithoutBehavior)
	if err := run([]string{
		"--file", path,
		"--workload", "Deployment/nightlamp-api",
		"--rule-id", "cpu-hpa-low-request-sensitive",
	}); err != nil {
		t.Fatalf("run() error: %v", err)
	}
	scaleUp, scaleDown := readHPAStabilization(t, path)
	if scaleUp != hpaScaleUpStabilizationSeconds {
		t.Fatalf("scaleUp stabilization = %d, want %d", scaleUp, hpaScaleUpStabilizationSeconds)
	}
	if scaleDown != hpaScaleDownStabilizationSeconds {
		t.Fatalf("scaleDown stabilization = %d, want %d", scaleDown, hpaScaleDownStabilizationSeconds)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read patched manifest: %v", err)
	}
	if !strings.Contains(string(payload), "stabilizationWindowSeconds: 60") {
		t.Fatalf("expected an unquoted integer stabilization window, got:\n%s", payload)
	}
}

func TestRunRaisesLowHPAScaleUpStabilization(t *testing.T) {
	path := writeTempManifest(t, hpaWithLowScaleUp)
	if err := run([]string{
		"--file", path,
		"--workload", "Deployment/nightlamp-api",
		"--rule-id", "cpu-hpa-low-request-sensitive",
	}); err != nil {
		t.Fatalf("run() error: %v", err)
	}
	scaleUp, scaleDown := readHPAStabilization(t, path)
	if scaleUp != hpaScaleUpStabilizationSeconds {
		t.Fatalf("scaleUp stabilization = %d, want %d", scaleUp, hpaScaleUpStabilizationSeconds)
	}
	if scaleDown != hpaScaleDownStabilizationSeconds {
		t.Fatalf("scaleDown stabilization = %d, want %d", scaleDown, hpaScaleDownStabilizationSeconds)
	}
}

func TestRunPreservesLargerHPAStabilization(t *testing.T) {
	path := writeTempManifest(t, hpaWithLargeWindows)
	err := run([]string{
		"--file", path,
		"--workload", "Deployment/nightlamp-api",
		"--rule-id", "cpu-hpa-low-request-sensitive",
	})
	if err == nil {
		t.Fatal("run() should fail when stabilization is already at or above the safe minimum")
	}
	if !strings.Contains(err.Error(), "already has scale-up and scale-down stabilization") {
		t.Fatalf("unexpected error: %v", err)
	}
	scaleUp, scaleDown := readHPAStabilization(t, path)
	if scaleUp != 120 || scaleDown != 600 {
		t.Fatalf("operator stabilization windows changed: scaleUp=%d scaleDown=%d", scaleUp, scaleDown)
	}
}

func TestRunFailsWhenHPATargetMissing(t *testing.T) {
	path := writeTempManifest(t, hpaForOtherWorkload)
	err := run([]string{
		"--file", path,
		"--workload", "Deployment/nightlamp-api",
		"--rule-id", "cpu-hpa-low-request-sensitive",
	})
	if err == nil {
		t.Fatal("run() should fail when no HPA targets the workload")
	}
	if !strings.Contains(err.Error(), "no matching api.yml object was changed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

const daemonSetWithRequests = `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: fluent-bit
spec:
  template:
    spec:
      containers:
        - name: fluent-bit
          resources:
            requests:
              cpu: 100m
              memory: 256Mi
`

func TestRunPatchesDaemonSetMemoryRequest(t *testing.T) {
	path := writeTempManifest(t, daemonSetWithRequests)
	if err := run([]string{
		"--file", path,
		"--workload", "DaemonSet/fluent-bit",
		"--container", "fluent-bit",
		"--rule-id", "memory-request-over-provisioned",
		"--memory-request", "96Mi",
	}); err != nil {
		t.Fatalf("run() error: %v", err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read patched manifest: %v", err)
	}
	if !strings.Contains(string(payload), "memory: 96Mi") {
		t.Fatalf("expected memory request to be 96Mi, got:\n%s", payload)
	}
	if !strings.Contains(string(payload), "cpu: 100m") {
		t.Fatalf("expected cpu request to be preserved, got:\n%s", payload)
	}
}

func TestRunRefusesProviderManagedWorkload(t *testing.T) {
	manifest := strings.Replace(daemonSetWithRequests, "fluent-bit", "kube-proxy", -1)
	path := writeTempManifest(t, manifest)
	err := run([]string{
		"--file", path,
		"--workload", "DaemonSet/kube-proxy",
		"--container", "kube-proxy",
		"--rule-id", "memory-request-over-provisioned",
		"--memory-request", "96Mi",
	})
	if err == nil {
		t.Fatal("run() should refuse to patch provider-managed workload")
	}
	if !strings.Contains(err.Error(), "reconciled by the cloud provider") {
		t.Fatalf("unexpected error: %v", err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if !strings.Contains(string(payload), "memory: 256Mi") {
		t.Fatalf("manifest must be unchanged on refusal, got:\n%s", payload)
	}
}

func TestRunRejectsUnsupportedKind(t *testing.T) {
	path := writeTempManifest(t, daemonSetWithRequests)
	err := run([]string{
		"--file", path,
		"--workload", "Job/fluent-bit",
		"--container", "fluent-bit",
		"--rule-id", "memory-request-over-provisioned",
		"--memory-request", "96Mi",
	})
	if err == nil {
		t.Fatal("run() should refuse unsupported kind")
	}
	if !strings.Contains(err.Error(), "not supported for request patching") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPatchHPASensitivityReportsChangeForLowWindow(t *testing.T) {
	docs, err := decodeYAMLDocuments([]byte(hpaWithLowScaleUp))
	if err != nil {
		t.Fatalf("decode documents: %v", err)
	}
	changed, err := patchHPASensitivity(docs, "Deployment/nightlamp-api")
	if err != nil {
		t.Fatalf("patchHPASensitivity error: %v", err)
	}
	if !changed {
		t.Fatal("patchHPASensitivity should report a change when it raises a low window")
	}
}
