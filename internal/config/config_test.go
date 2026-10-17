package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadProxyURLFromEnvironmentOverridesYAML(t *testing.T) {
	t.Setenv("HARVESTER_PROXY_URL", " http://user:pass@proxy.example:1080 ")
	path := writeConfig(t, "proxies:\n  - http://yaml-proxy.example:8080\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Proxies) != 1 || cfg.Proxies[0] != "http://user:pass@proxy.example:1080" {
		t.Fatalf("Proxies = %#v, want the trimmed environment proxy URL", cfg.Proxies)
	}
}

func TestLoadKeepsYAMLProxiesWhenEnvironmentUnset(t *testing.T) {
	t.Setenv("HARVESTER_PROXY_URL", "")
	path := writeConfig(t, "proxies:\n  - http://yaml-proxy.example:8080\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(cfg.Proxies) != 1 || cfg.Proxies[0] != "http://yaml-proxy.example:8080" {
		t.Fatalf("Proxies = %#v, want YAML proxy list unchanged", cfg.Proxies)
	}
}
