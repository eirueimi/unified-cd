package k8sagent

import (
	"os"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
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

// TestLoadConfig_RejectsWorkspacePVC pins that an agent podTemplate still
// carrying the removed workspace.pvc block stops the agent from starting. This
// loader is lenient about unknown keys, so without the check the block would be
// dropped silently and a persistent workspace would quietly become ephemeral.
func TestLoadConfig_RejectsWorkspacePVC(t *testing.T) {
	cfg := writeTempYAML(t, `
server: http://localhost:8080
namespace: ci
podTemplates:
  golang:
    workspace:
      pvc:
        claimName: go-build-cache
    spec:
      containers:
        - name: job
          image: golang:1.24
`)
	_, err := LoadConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `podTemplates["golang"].workspace.pvc`)
	assert.Contains(t, err.Error(), "docs/operator-manual/migrations/2026-10-20-named-volumes.md")
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
	if got.PodImage != defaultPodImage() {
		t.Errorf("PodImage = %q, want %q", got.PodImage, defaultPodImage())
	}
	if got.ShimImage != defaultShimImage() {
		t.Errorf("ShimImage = %q, want %q", got.ShimImage, defaultShimImage())
	}
}

// setAgentVersion stamps agentlib.Version for one test. A test binary always
// reports "dev", which maps to ":latest" -- a test reading a default image
// without stamping a version could not tell "derived from the version" from
// "hard-coded :latest".
func setAgentVersion(t *testing.T, v string) {
	t.Helper()
	old := agentlib.Version
	agentlib.Version = v
	t.Cleanup(func() { agentlib.Version = old })
}

// TestDefaultImagesCarryNoDigest guards the invariant that replaced digest
// pinning: every unified-cd default image is named by a tag derived from the
// agent's own version, never by a digest.
//
// A digest of an image published from this repository can only be recorded in
// a commit AFTER the image is pushed, so a digest pin can only ever name the
// PREVIOUS release. Operators who need immutability set the image explicitly in
// the agent configuration; see docs/operator-manual/kubernetes-integration.md.
func TestDefaultImagesCarryNoDigest(t *testing.T) {
	for name, img := range map[string]string{
		"pod":     defaultPodImage(),
		"sidecar": defaultSidecarImage(),
		"shim":    defaultShimImage(),
	} {
		assert.NotContains(t, img, "@sha256:",
			"default %s image must be version-derived, not digest-pinned: a digest for an "+
				"image built from this tree can only name the previous release", name)
	}
}

// TestDefaultImagesResolveThroughVersionedImage pins that each default is
// computed from the stamped version rather than holding a literal tag. Without
// this a literal ":v0.0.3" would satisfy the no-digest test above and silently
// reintroduce the version skew.
func TestDefaultImagesResolveThroughVersionedImage(t *testing.T) {
	const (
		runner  = "ghcr.io/eirueimi/unified-cd-runner"
		sidecar = "ghcr.io/eirueimi/unified-cd-artifact-sidecar"
		shim    = "ghcr.io/eirueimi/unified-cd-k8s-agent"
	)
	setAgentVersion(t, "v9.9.9")
	assert.Equal(t, agentlib.VersionedImage(runner, "v9.9.9"), defaultPodImage())
	assert.Equal(t, agentlib.VersionedImage(sidecar, "v9.9.9"), defaultSidecarImage())
	assert.Equal(t, agentlib.VersionedImage(shim, "v9.9.9"), defaultShimImage())
	assert.Equal(t, runner+":v9.9.9", defaultPodImage())
	assert.Equal(t, sidecar+":v9.9.9", defaultSidecarImage())
	assert.Equal(t, shim+":v9.9.9", defaultShimImage())

	setAgentVersion(t, "dev")
	assert.Equal(t, runner+":latest", defaultPodImage())
	assert.Equal(t, sidecar+":latest", defaultSidecarImage())
	assert.Equal(t, shim+":latest", defaultShimImage())
}

// TestDefaultsReachEveryCallSite confirms DefaultConfig, Validate and
// defaultPodSpec all take their image from the version-derived defaults. Each
// of the three used to read a constant; one left holding a literal string
// would keep working and silently pin the old image.
func TestDefaultsReachEveryCallSite(t *testing.T) {
	setAgentVersion(t, "v9.9.9")

	dc := DefaultConfig()
	assert.Equal(t, defaultPodImage(), dc.PodImage)
	assert.Equal(t, defaultSidecarImage(), dc.SidecarImage)
	assert.Equal(t, defaultShimImage(), dc.ShimImage)

	cfg := Config{Server: "http://localhost:8080", EnrollmentPolicy: "p"}
	require.NoError(t, cfg.Validate())
	assert.Equal(t, defaultPodImage(), cfg.PodImage)
	assert.Equal(t, defaultSidecarImage(), cfg.SidecarImage)
	assert.Equal(t, defaultShimImage(), cfg.ShimImage)

	spec := defaultPodSpec("")
	require.NotEmpty(t, spec.Containers)
	assert.Equal(t, defaultPodImage(), spec.Containers[0].Image)
	assert.Contains(t, spec.Containers[0].Image, ":v9.9.9")
}

// TestDefaultConfig_ShimImage confirms DefaultConfig() itself (not just
// LoadConfig, which starts from it) sets ShimImage to the k8s-agent's own
// image.
func TestDefaultConfig_ShimImage(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ShimImage != defaultShimImage() {
		t.Errorf("ShimImage = %q, want %q", cfg.ShimImage, defaultShimImage())
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
	if cfg.ShimImage != defaultShimImage() {
		t.Errorf("ShimImage = %q, want %q", cfg.ShimImage, defaultShimImage())
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
