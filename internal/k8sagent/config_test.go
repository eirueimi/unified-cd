package k8sagent

import (
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// digestPinPattern requires a well-formed, complete 64-hex-character sha256
// digest anchored at the end of the string. A plain `strings.Contains(img,
// "@sha256:")` check would still pass for a truncated or malformed digest
// (e.g. "@sha256:" with nothing after it) — which is exactly the kind of pin
// that looks correct at a glance but silently resolves to the wrong (or no)
// image at pull time.
var digestPinPattern = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

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
labels:
  - kind:k8s
namespace: ci
maxConcurrent: 3
podImage: alpine:3.20
`)
	got, err := LoadConfig(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Server != "http://localhost:8080" {
		t.Errorf("Server = %q, want %q", got.Server, "http://localhost:8080")
	}
	if got.MaxConcurrent != 3 {
		t.Errorf("MaxConcurrent = %d, want 3", got.MaxConcurrent)
	}
}

func TestLoadConfig_SidecarS3SecretName(t *testing.T) {
	cfg := writeTempYAML(t, `
server: http://localhost:8080
namespace: ci
sidecarS3SecretName: my-s3-secret
`)
	got, err := LoadConfig(cfg)
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
`)
	got, err := LoadConfig(cfg)
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
	assert.Regexp(t, digestPinPattern, defaultPodImage,
		"default pod image must be pinned to a well-formed, complete sha256 digest — "+
			"a mutable tag (or a truncated/malformed digest) lets whoever controls the "+
			"registry repository force-push it and execute code in the primary container "+
			"of every isolated job lacking its own podTemplate job container")
}

// TestDefaultSidecarImageIsDigestPinned guards against the k8s-agent's
// fleet-wide default artifact-sidecar image regressing to a mutable tag.
// Unlike PodImage, this is auto-injected into every k8s-agent pod regardless
// of job-author-supplied podTemplate — it is never job-author-controlled —
// and it holds long-lived, bucket-scoped static S3 credentials (see
// SidecarS3SecretName / podbuilder.go's buildArtifactSidecarContainer and
// cmd/unified-sidecar/main.go). A mutable tag here is therefore not just a
// code-exec path but a credential-exfiltration path: a registry compromise
// of this image gets read/write over the whole artifact bucket.
func TestDefaultSidecarImageIsDigestPinned(t *testing.T) {
	assert.Regexp(t, digestPinPattern, defaultSidecarImage,
		"default sidecar image must be pinned to a well-formed, complete sha256 digest — "+
			"a mutable tag (or a truncated/malformed digest) lets whoever controls the "+
			"registry repository force-push it and both execute code in, and exfiltrate the "+
			"long-lived S3 credentials of, every k8s-agent pod's auto-injected sidecar fleet-wide")
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
		Server:           "http://localhost:8080",
		EnrollmentPolicy: "p",
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
		Server:           "http://localhost:8080",
		EnrollmentPolicy: "p",
		ShimImage:        "registry.internal/mirror/ucd-k8s-agent:v1",
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
		return Config{Server: "https://s", EnrollmentPolicy: "p"}
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
	c := Config{Server: "https://s", EnrollmentPolicy: "p"}
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
	bad := Config{Server: "https://s", EnrollmentPolicy: "p", PodStartTimeout: "not-a-duration"}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected Validate to reject an unparseable podStartTimeout")
	}
	badDrain := Config{Server: "https://s", EnrollmentPolicy: "p", DrainTimeout: "not-a-duration"}
	if err := badDrain.Validate(); err == nil {
		t.Fatal("expected Validate to reject an unparseable drainTimeout")
	}
}

func TestKubernetesCredentialConfigAllowsAutomaticEnrollmentWithoutLegacyTokenOrAgentID(t *testing.T) {
	c := Config{Server: "https://controller", EnrollmentPolicy: "cluster-agents"}
	require.NoError(t, c.Validate())
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
