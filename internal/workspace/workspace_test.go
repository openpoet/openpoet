package workspace

import (
	"context"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"openpoet/internal/database"
)

const exampleManifest = `version: 1
setup:
  - npm ci
services:
  web:
    command: npm run dev
    port: "{{PORT}}"
    health: "http://localhost:{{PORT}}/"
    scope: per-sandbox
env:
  NODE_ENV: development
`

// TestManifestParseAndSHA256: the example manifest parses, the {{PORT}} template
// renders, and the SHA-256 is stable + drift-sensitive.
func TestManifestParseAndSHA256(t *testing.T) {
	m, err := ParseManifest([]byte(exampleManifest))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Version != 1 || len(m.Setup) != 1 || m.Setup[0] != "npm ci" {
		t.Fatalf("bad parse: %+v", m)
	}
	svc, ok := m.Services["web"]
	if !ok || svc.Command != "npm run dev" || !PortIsTemplated(svc.Port) || svc.Scope != "per-sandbox" {
		t.Fatalf("service parse wrong: %+v", svc)
	}
	if got := RenderTemplate(svc.Health, 8712); got != "http://localhost:8712/" {
		t.Fatalf("render: %q", got)
	}
	env := m.RenderedEnv(8712)
	if env["PORT"] != "8712" || env["NODE_ENV"] != "development" {
		t.Fatalf("rendered env wrong: %v", env)
	}
	sha := ManifestSHA256([]byte(exampleManifest))
	if len(sha) != 64 {
		t.Fatalf("sha length %d", len(sha))
	}
	if ManifestSHA256([]byte(exampleManifest)) != sha {
		t.Fatal("sha not stable")
	}
	if ManifestSHA256([]byte(exampleManifest+"# drift\n")) == sha {
		t.Fatal("sha not drift-sensitive")
	}
}

// TestPortAllocatorSkipsReserved: the DB-backed allocator never returns a
// reserved port (8080/8081/8090) and an explicit request for a reserved port
// fails closed.
func TestPortAllocatorSkipsReserved(t *testing.T) {
	db, err := database.New(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	// Force the range to straddle the reserved band so skipping is exercised.
	_ = db.SetSetting(ctx, "environment_port_min", "8080")
	_ = db.SetSetting(ctx, "environment_port_max", "8082")
	port, err := db.AllocatePort(ctx, 1, "", "web")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if port == 8080 || port == 8081 {
		t.Fatalf("allocator returned a reserved port %d", port)
	}
	if port != 8082 {
		t.Fatalf("expected the only free non-reserved port 8082, got %d", port)
	}
	// Explicit reserved port → fails closed.
	if err := db.AllocateSpecificPort(ctx, 1, "", "web", 8081); err == nil {
		t.Fatal("AllocateSpecificPort(8081) must fail (reserved)")
	}
}

// TestProcessDriverSpawnAndKill: the process driver spawns a child, exposes its
// pid, and Kill terminates it.
func TestProcessDriverSpawnAndKill(t *testing.T) {
	d := &ProcessDriver{}
	pid, err := d.Spawn("sleep 30", t.TempDir(), map[string]string{"FOO": "bar"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if pid <= 0 {
		t.Fatalf("bad pid %d", pid)
	}
	if !pidAlive(pid) {
		t.Fatal("spawned process not alive")
	}
	d.Kill()
	// give the OS a moment to reap
	dead := false
	for i := 0; i < 20; i++ {
		if !pidAlive(pid) {
			dead = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !dead {
		t.Fatalf("process %d still alive after Kill", pid)
	}
	// KillPID on a bogus pid is a safe no-op.
	KillPID(-1)
}

func pidAlive(pid int) bool {
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}
