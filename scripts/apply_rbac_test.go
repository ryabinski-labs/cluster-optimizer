package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func fakeKubectl(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kubectl")
	logPath := filepath.Join(dir, "kubectl.log")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$KUBECTL_LOG\"\n"
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	return path, logPath
}

func runRBACScript(t *testing.T, args ...string) (string, error) {
	t.Helper()
	kubectl, logPath := fakeKubectl(t)
	cmd := exec.Command("bash", append([]string{"apply-rbac.sh"}, args...)...)
	cmd.Env = append(os.Environ(), "KUBECTL="+kubectl, "KUBECTL_LOG="+logPath)
	output, err := cmd.CombinedOutput()
	logBody, readErr := os.ReadFile(logPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read fake kubectl log: %v", readErr)
	}
	return string(output) + string(logBody), err
}

func TestApplyRBACApplierMigratesLegacyObjects(t *testing.T) {
	output, err := runRBACScript(t, "--applier")
	if err != nil {
		t.Fatalf("apply script failed: %v\n%s", err, output)
	}
	for _, expected := range []string{
		"apply -f ",
		"rbac-applier.yaml",
		"delete rolebinding cluster-optimizer-applier -n default --ignore-not-found",
		"delete role cluster-optimizer-applier -n default --ignore-not-found",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("expected %q in output:\n%s", expected, output)
		}
	}
}

func TestApplyRBACRevokeRemovesCurrentAndLegacyObjects(t *testing.T) {
	output, err := runRBACScript(t, "--revoke-applier")
	if err != nil {
		t.Fatalf("revoke script failed: %v\n%s", err, output)
	}
	for _, expected := range []string{
		"delete -f ",
		"rbac-applier.yaml --ignore-not-found",
		"delete rolebinding cluster-optimizer-applier -n default --ignore-not-found",
		"delete role cluster-optimizer-applier -n default --ignore-not-found",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("expected %q in output:\n%s", expected, output)
		}
	}
}

func TestApplyRBACDryRunShowsMigrationCleanup(t *testing.T) {
	output, err := runRBACScript(t, "--dry-run", "--applier")
	if err != nil {
		t.Fatalf("dry-run script failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, "delete rolebinding cluster-optimizer-applier -n default --ignore-not-found --dry-run=client") {
		t.Fatalf("dry-run omitted legacy cleanup:\n%s", output)
	}
}

func TestApplyRBACRejectsConflictingModes(t *testing.T) {
	output, err := runRBACScript(t, "--applier", "--revoke-applier")
	if err == nil {
		t.Fatalf("expected conflicting modes to fail:\n%s", output)
	}
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 {
		t.Fatalf("expected exit code 2, got %v\n%s", err, output)
	}
}
