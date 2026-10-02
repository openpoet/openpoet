// Package deploygate holds the mandatory pre-deploy gate (gate.sh). This test
// runs its shell suites so `go test ./...` fails whenever the gate regresses.
package deploygate

import (
	"os/exec"
	"testing"
)

func TestDeployGate(t *testing.T) {
	runSuite(t, "gate_test.sh")
}

func TestDeployScriptRunsTheGate(t *testing.T) {
	runSuite(t, "deploy_sh_test.sh")
}

func runSuite(t *testing.T, script string) {
	t.Helper()
	for _, tool := range []string{"bash", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	output, err := exec.Command("bash", script).CombinedOutput()
	t.Logf("%s", output)
	if err != nil {
		t.Fatalf("%s failed: %v", script, err)
	}
}
