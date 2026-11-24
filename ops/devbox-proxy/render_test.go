// Package devboxproxy holds the templates of the devbox reverse proxy that
// OpenPoet publishes devbox ports through (see README.md). This test runs the
// shell suite so `go test ./...` fails whenever render.sh regresses.
package devboxproxy

import (
	"os/exec"
	"testing"
)

func TestRender(t *testing.T) {
	for _, tool := range []string{"bash", "sed", "awk", "diff"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	output, err := exec.Command("bash", "render_test.sh").CombinedOutput()
	t.Logf("%s", output)
	if err != nil {
		t.Fatalf("render_test.sh failed: %v", err)
	}
}
