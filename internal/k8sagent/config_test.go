package k8sagent

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "*.yaml")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	f.Close()
	return f.Name()
}

func TestLoadConfig_ConfigOnly(t *testing.T) {
	cfg := writeTempYAML(t, `
server: http://localhost:8080
agentId: agent-1
labels:
  - kind:k8s
namespace: ci
maxConcurrent: 3
podImage: alpine:3.20
token: from-config
`)
	got, err := LoadConfig(cfg, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Server != "http://localhost:8080" {
		t.Errorf("Server = %q, want %q", got.Server, "http://localhost:8080")
	}
	if got.Token != "from-config" {
		t.Errorf("Token = %q, want %q", got.Token, "from-config")
	}
	if got.MaxConcurrent != 3 {
		t.Errorf("MaxConcurrent = %d, want 3", got.MaxConcurrent)
	}
}

func TestLoadConfig_SecretOverridesToken(t *testing.T) {
	cfg := writeTempYAML(t, `
server: http://localhost:8080
agentId: agent-1
namespace: ci
`)
	secret := writeTempYAML(t, `
token: secret-token
`)
	got, err := LoadConfig(cfg, secret)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Token != "secret-token" {
		t.Errorf("Token = %q, want %q", got.Token, "secret-token")
	}
}

func TestLoadConfig_MissingSecretFileIsSkipped(t *testing.T) {
	cfg := writeTempYAML(t, `
server: http://localhost:8080
agentId: agent-1
namespace: ci
token: from-config
`)
	nonExistent := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	got, err := LoadConfig(cfg, nonExistent)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Token != "from-config" {
		t.Errorf("Token = %q, want %q", got.Token, "from-config")
	}
}

func TestLoadConfig_EmptySecretPathIsSkipped(t *testing.T) {
	cfg := writeTempYAML(t, `
server: http://localhost:8080
agentId: agent-1
namespace: ci
token: from-config
`)
	got, err := LoadConfig(cfg, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Token != "from-config" {
		t.Errorf("Token = %q, want %q", got.Token, "from-config")
	}
}

func TestLoadConfig_SidecarS3SecretName(t *testing.T) {
	cfg := writeTempYAML(t, `
server: http://localhost:8080
agentId: agent-1
namespace: ci
token: from-config
sidecarS3SecretName: my-s3-secret
`)
	got, err := LoadConfig(cfg, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.SidecarS3SecretName != "my-s3-secret" {
		t.Errorf("SidecarS3SecretName = %q, want %q", got.SidecarS3SecretName, "my-s3-secret")
	}
}

func TestLoadConfig_DefaultsApplied(t *testing.T) {
	cfg := writeTempYAML(t, `
server: http://localhost:8080
agentId: agent-1
`)
	got, err := LoadConfig(cfg, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Namespace != "default" {
		t.Errorf("Namespace = %q, want %q", got.Namespace, "default")
	}
	if got.MaxConcurrent != 5 {
		t.Errorf("MaxConcurrent = %d, want 5", got.MaxConcurrent)
	}
	if got.PodImage != "ghcr.io/eirueimi/unified-cd-runner:v0.0.3" {
		t.Errorf("PodImage = %q, want %q", got.PodImage, "ghcr.io/eirueimi/unified-cd-runner:v0.0.3")
	}
	if got.ShimImage != defaultShimImage {
		t.Errorf("ShimImage = %q, want %q", got.ShimImage, defaultShimImage)
	}
}

// TestDefaultConfig_ShimImage confirms DefaultConfig() itself (not just
// LoadConfig, which starts from it) sets ShimImage to the k8s-agent's own
// image.
func TestDefaultConfig_ShimImage(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ShimImage != defaultShimImage {
		t.Errorf("ShimImage = %q, want %q", cfg.ShimImage, defaultShimImage)
	}
}

// TestValidate_ShimImageFallback confirms Config.Validate() fills in the
// default ShimImage when a config file was loaded without one set (e.g. an
// older config predating the step-shell-shim feature), mirroring the
// fallback Validate() already does for PodImage/SidecarImage.
func TestValidate_ShimImageFallback(t *testing.T) {
	cfg := Config{
		Server:  "http://localhost:8080",
		Token:   "t",
		AgentID: "agent-1",
		// ShimImage intentionally left zero-value.
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ShimImage != defaultShimImage {
		t.Errorf("ShimImage = %q, want %q", cfg.ShimImage, defaultShimImage)
	}
}

// TestValidate_ShimImagePreserved confirms Validate() does not clobber an
// explicitly configured ShimImage (e.g. an air-gapped registry's mirrored
// copy) with the default.
func TestValidate_ShimImagePreserved(t *testing.T) {
	cfg := Config{
		Server:    "http://localhost:8080",
		Token:     "t",
		AgentID:   "agent-1",
		ShimImage: "registry.internal/mirror/ucd-k8s-agent:v1",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ShimImage != "registry.internal/mirror/ucd-k8s-agent:v1" {
		t.Errorf("ShimImage = %q, want it preserved", cfg.ShimImage)
	}
}
