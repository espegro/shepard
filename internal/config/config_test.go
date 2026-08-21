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
	if cfg.Server.Listen != ":8080" || cfg.Server.MaxRequestBytes == 0 || cfg.Server.MaxHeaderBytes == 0 ||
		cfg.Server.MaxConcurrentRequests == 0 || cfg.Server.ReadTimeout.Duration == 0 ||
		cfg.Server.WriteIdleTimeout.Duration == 0 || cfg.Server.ReadinessCacheTTL.Duration == 0 ||
		cfg.Server.Queue.Enabled == nil || !*cfg.Server.Queue.Enabled {
		t.Fatalf("defaults not applied: %+v", cfg.Server)
	}
	if !cfg.Server.BroadlyExposedWithoutAccessControls() {
		t.Fatal("default wildcard listener without auth or ACL should be reported as broadly exposed")
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

func TestBroadExposureWarningRequiresWildcardWithoutControls(t *testing.T) {
	tests := []struct {
		name   string
		server ServerConfig
		want   bool
	}{
		{name: "wildcard IPv4", server: ServerConfig{Listen: "0.0.0.0:8080"}, want: true},
		{name: "wildcard IPv6", server: ServerConfig{Listen: "[::]:8080"}, want: true},
		{name: "loopback", server: ServerConfig{Listen: "127.0.0.1:8080"}},
		{name: "authenticated", server: ServerConfig{Listen: ":8080", InboundAPIKeys: []string{"secret"}}},
		{name: "ACL protected", server: ServerConfig{Listen: ":8080", ClientNetworks: []string{"10.0.0.0/24"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.server.BroadlyExposedWithoutAccessControls(); got != test.want {
				t.Fatalf("BroadlyExposedWithoutAccessControls() = %v, want %v", got, test.want)
			}
		})
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
