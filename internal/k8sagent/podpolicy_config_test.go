package k8sagent

import (
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/config"
)

func policyBaseConfig() Config {
	return Config{Server: "https://c.example", EnrollmentPolicy: "p"}
}

func TestJobPodPolicyConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string // empty means Validate must succeed
	}{
		{"zero policy", func(c *Config) {}, ""},
		{"wildcard secret", func(c *Config) { c.JobPodPolicy.AllowSecrets = []string{"*"} },
			`jobPodPolicy: allowSecrets: "*": wildcards are not supported; list each name`},
		{"bad service account name", func(c *Config) { c.JobPodPolicy.AllowServiceAccounts = []string{"Bad_Name"} },
			`is not a valid Kubernetes object name`},
		{"sidecar secret allowed", func(c *Config) {
			c.SidecarS3SecretName = "ucd-s3"
			c.JobPodPolicy.AllowSecrets = []string{"ucd-s3"}
		}, `is sidecarS3SecretName`},
		{"own named volume PVC", func(c *Config) {
			c.AgentName = "ci-main"
			c.Volumes = []config.VolumeConfig{{Name: "go-cache", Mode: "exclusive", Size: "1Gi"}}
			c.JobPodPolicy.AllowPersistentVolumeClaims = []string{"ci-main-go-cache"}
		}, `is this agent's named volume "go-cache"; jobs reach it only through spec.volumes`},
		{"opt-in PodSpec field", func(c *Config) { c.JobPodPolicy.AllowPodSpecFields = []string{"runtimeClassName"} }, ""},
		{"always-allowed PodSpec field", func(c *Config) { c.JobPodPolicy.AllowPodSpecFields = []string{"containers"} },
			`"containers" is always allowed; remove it`},
		{"category PodSpec field", func(c *Config) { c.JobPodPolicy.AllowPodSpecFields = []string{"hostNetwork"} },
			`"hostNetwork" is governed by jobPodPolicy.allowHostAccess; use that instead`},
		{"never PodSpec field", func(c *Config) { c.JobPodPolicy.AllowPodSpecFields = []string{"ephemeralContainers"} },
			`cannot be allowed`},
		{"unknown PodSpec field", func(c *Config) { c.JobPodPolicy.AllowPodSpecFields = []string{"bogus"} },
			`is not an opt-in PodSpec field`},
		{"opt-in volume type", func(c *Config) { c.JobPodPolicy.AllowVolumeTypes = []string{"nfs"} }, ""},
		{"category volume type", func(c *Config) { c.JobPodPolicy.AllowVolumeTypes = []string{"secret"} },
			`governed by jobPodPolicy.allowSecrets`},
		{"always-allowed volume type", func(c *Config) { c.JobPodPolicy.AllowVolumeTypes = []string{"emptyDir"} },
			`is always allowed`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := policyBaseConfig()
			tc.mutate(&c)
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestJobPodPolicyConfigUnknownPodSpecFieldListsValidEntries(t *testing.T) {
	c := policyBaseConfig()
	c.JobPodPolicy.AllowPodSpecFields = []string{"bogus"}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "runtimeClassName") {
		t.Fatalf("Validate() = %v, want error listing runtimeClassName", err)
	}
}

func TestJobPodPolicyConfigLoadRejectsUnknownKey(t *testing.T) {
	path := writeTempYAML(t, "jobPodPolicy:\n  allowHostAcess: true\n")
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "jobPodPolicy") || !strings.Contains(err.Error(), "allowHostAcess") {
		t.Fatalf("LoadConfig() = %v, want error naming jobPodPolicy and allowHostAcess", err)
	}
}

func TestJobPodPolicyConfigLoadDecodes(t *testing.T) {
	path := writeTempYAML(t, "jobPodPolicy:\n  allowHostAccess: true\n  allowSecrets: [db]\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}
	if !cfg.JobPodPolicy.AllowHostAccess || len(cfg.JobPodPolicy.AllowSecrets) != 1 || cfg.JobPodPolicy.AllowSecrets[0] != "db" {
		t.Fatalf("decoded policy = %+v", cfg.JobPodPolicy)
	}
}

func TestJobPodPolicyConfigLoadWithoutBlock(t *testing.T) {
	path := writeTempYAML(t, "server: https://c.example\n")
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig() = %v", err)
	}
}

func TestJobPodPolicyConfigSummary(t *testing.T) {
	if got := (JobPodPolicy{}).Summary(); got != "none" {
		t.Fatalf("zero Summary() = %q, want none", got)
	}
	p := JobPodPolicy{AllowHostAccess: true, AllowSecrets: []string{"a", "b"}}
	if got, want := p.Summary(), "allowSecrets=a,b allowHostAccess"; got != want {
		t.Fatalf("Summary() = %q, want %q", got, want)
	}
}

func TestJobPodPolicyConfigEveryCategoryHasKey(t *testing.T) {
	for name, cls := range podSpecFieldClass {
		if cls == classCategory && podSpecCategoryKey[name] == "" {
			t.Errorf("PodSpec category field %q has no podSpecCategoryKey entry", name)
		}
	}
	for name, cls := range volumeSourceClass {
		if cls == classCategory && volumeCategoryKey[name] == "" {
			t.Errorf("volume category type %q has no volumeCategoryKey entry", name)
		}
	}
}
