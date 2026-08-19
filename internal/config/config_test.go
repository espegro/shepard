package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaultsAndRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.yaml")
	if err := os.WriteFile(valid, []byte(`
providers:
  test:
    base_url: http://localhost:9999/v1
models:
  stable:
    provider: test
    model: real-model
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(valid)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != ":8080" || cfg.Server.MaxRequestBytes == 0 || cfg.Server.ReadTimeout.Duration == 0 || cfg.Server.Queue.Enabled == nil || !*cfg.Server.Queue.Enabled {
		t.Fatalf("defaults not applied: %+v", cfg.Server)
	}

	invalid := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(invalid, []byte(`
server:
  lissten: :1234
providers:
  test:
    base_url: http://localhost:9999/v1
models:
  stable:
    provider: test
    model: real-model
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(invalid)
	if err == nil || !strings.Contains(err.Error(), "lissten") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestRejectsStructuralModelOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "invalid-override.yaml")
	if err := os.WriteFile(path, []byte(`
providers:
  test:
    base_url: http://localhost:9999/v1
models:
  stable:
    provider: test
    model: real-model
    overrides:
      messages: []
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "override cannot set") {
		t.Fatalf("expected structural override error, got %v", err)
	}
}
