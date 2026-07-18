package k8sagent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	if got.MaxConcurrent != 100 {
		t.Errorf("MaxConcurrent = %d, want 100", got.MaxConcurrent)
	}
	if got.PodImage != defaultPodImage {
		t.Errorf("PodImage = %q, want %q", got.PodImage, defaultPodImage)
	}
	if got.ShimImage != defaultShimImage {
		t.Errorf("ShimImage = %q, want %q", got.ShimImage, defaultShimImage)
	}
}

// TestDefaultPodImageIsDigestPinned guards against the k8s-agent's
// fleet-wide default pod image regressing to a mutable tag. A mutable tag
// lets whoever controls the registry repository force-push it and execute
// code in the primary container of every isolated job lacking its own
// podTemplate job container.
func TestDefaultPodImageIsDigestPinned(t *testing.T) {
	assert.Contains(t, defaultPodImage, "@sha256:", "default pod image must be digest-pinned")
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

func TestPodStartTimeoutDuration_DefaultAndParse(t *testing.T) {
	c := DefaultConfig()
	if got := c.PodStartTimeoutDuration(); got != 5*time.Minute {
		t.Fatalf("unset podStartTimeout: want 5m, got %v", got)
	}
	c.PodStartTimeout = "90s"
	if got := c.PodStartTimeoutDuration(); got != 90*time.Second {
		t.Fatalf("parsed podStartTimeout: want 90s, got %v", got)
	}
	c.PodStartTimeout = "0s" // non-positive -> default
	if got := c.PodStartTimeoutDuration(); got != 5*time.Minute {
		t.Fatalf("non-positive podStartTimeout: want 5m, got %v", got)
	}
}

func TestDrainTimeoutDuration_DefaultAndParse(t *testing.T) {
	c := DefaultConfig()
	if got := c.DrainTimeoutDuration(); got != 0 {
		t.Fatalf("unset drainTimeout: want 0, got %v", got)
	}
	c.DrainTimeout = "30s"
	if got := c.DrainTimeoutDuration(); got != 30*time.Second {
		t.Fatalf("parsed drainTimeout: want 30s, got %v", got)
	}
}

func TestValidate_MaxConcurrentDefaultAndUnlimited(t *testing.T) {
	base := func() Config {
		return Config{Server: "s", Token: "t", AgentID: "a"}
	}
	// 0 -> 100
	c := base()
	c.MaxConcurrent = 0
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.MaxConcurrent != 100 {
		t.Fatalf("zero maxConcurrent: want 100, got %d", c.MaxConcurrent)
	}
	// negative -> unchanged (unlimited sentinel)
	c = base()
	c.MaxConcurrent = -1
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.MaxConcurrent != -1 {
		t.Fatalf("negative maxConcurrent must be preserved as unlimited, got %d", c.MaxConcurrent)
	}
	// positive -> unchanged
	c = base()
	c.MaxConcurrent = 3
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.MaxConcurrent != 3 {
		t.Fatalf("positive maxConcurrent: want 3, got %d", c.MaxConcurrent)
	}
}

func TestValidate_DurationEnvOverridesAndParseError(t *testing.T) {
	t.Setenv("UNIFIED_K8S_POD_START_TIMEOUT", "42s")
	t.Setenv("UNIFIED_K8S_DRAIN_TIMEOUT", "7s")
	c := Config{Server: "s", Token: "t", AgentID: "a"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.PodStartTimeoutDuration() != 42*time.Second {
		t.Fatalf("env override podStartTimeout: got %v", c.PodStartTimeoutDuration())
	}
	if c.DrainTimeoutDuration() != 7*time.Second {
		t.Fatalf("env override drainTimeout: got %v", c.DrainTimeoutDuration())
	}
}

func TestValidate_DurationParseError(t *testing.T) {
	bad := Config{Server: "s", Token: "t", AgentID: "a", PodStartTimeout: "not-a-duration"}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected Validate to reject an unparseable podStartTimeout")
	}
	badDrain := Config{Server: "s", Token: "t", AgentID: "a", DrainTimeout: "not-a-duration"}
	if err := badDrain.Validate(); err == nil {
		t.Fatal("expected Validate to reject an unparseable drainTimeout")
	}
}

func TestKubernetesCredentialConfigAllowsAutomaticEnrollmentWithoutLegacyTokenOrAgentID(t *testing.T) {
	c := Config{Server: "https://controller", EnrollmentPolicy: "cluster-agents"}
	require.NoError(t, c.Validate())
	assert.Empty(t, c.Token)
	assert.Empty(t, c.AgentID)
	assert.Equal(t, defaultServiceAccountTokenFile, c.ServiceAccountTokenFile)
}

func TestKubernetesCredentialConfigRejectsRemoteHTTPEnrollment(t *testing.T) {
	c := Config{Server: "http://controller.example", EnrollmentPolicy: "cluster-agents"}
	require.ErrorContains(t, c.Validate(), "https")

	local := Config{Server: "http://localhost:8080", EnrollmentPolicy: "cluster-agents"}
	require.NoError(t, local.Validate())

	explicitDev := Config{Server: "http://controller.dev.svc", EnrollmentPolicy: "cluster-agents", AllowInsecureHTTP: true}
	require.NoError(t, explicitDev.Validate())
}
