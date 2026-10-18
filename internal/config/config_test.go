package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gitMFQ/gopm2/internal/types"
)

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	content := `
version = 1

[defaults]
restart_policy = "on-failure"
max_restarts = 10

[[apps]]
name = "api"
command = "node server.js"
cwd = "./services/api"
restart_policy = "always"
health_cmd = "curl -fsS http://127.0.0.1:3000/health"
health_interval = "15s"

[apps.env]
NODE_ENV = "production"

[[apps]]
name = "worker"
command = "python worker.py"
`
	path := filepath.Join(dir, "gopm2.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	specs, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("want 2 specs, got %d", len(specs))
	}
	api := specs[0]
	if api.Name != "api" || api.Restart != types.RestartAlways {
		t.Fatalf("api spec wrong: %+v", api)
	}
	if api.Cwd != filepath.Join(dir, "services/api") {
		t.Fatalf("cwd should be resolved relative to config file: %q", api.Cwd)
	}
	if api.Env["NODE_ENV"] != "production" {
		t.Fatalf("env not loaded: %+v", api.Env)
	}
	if api.Health.Type != types.HealthExec || api.Health.Target == "" {
		t.Fatalf("health wrong: %+v", api.Health)
	}
	if api.Health.Interval.Seconds() != 15 {
		t.Fatalf("health interval wrong: %v", api.Health.Interval)
	}
	worker := specs[1]
	if worker.Restart != types.RestartOnFailure || worker.MaxRestarts != 10 {
		t.Fatalf("defaults not applied: %+v", worker)
	}
}

func TestLoadFileInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")
	os.WriteFile(path, []byte("[[apps]]\nname = \"x\"\n"), 0o644) // 缺 command
	if _, err := LoadFile(path); err == nil {
		t.Fatal("expected error for missing command")
	}
}
