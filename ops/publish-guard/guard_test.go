// Package publishguard holds the publication guard (guard.sh) run by the
// pre-commit and pre-push hooks. This test runs its shell suite so
// `go test ./...` fails whenever the guard or the hooks regress.
package publishguard

import (
	"os/exec"
	"testing"
)

func TestPublishGuard(t *testing.T) {
	for _, tool := range []string{"bash", "git", "gitleaks"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available (make tools)", tool)
		}
	}
	output, err := exec.Command("bash", "guard_test.sh").CombinedOutput()
	t.Logf("%s", output)
	if err != nil {
		t.Fatalf("guard_test.sh failed: %v", err)
	}
}
