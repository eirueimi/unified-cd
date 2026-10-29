package k8sagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

// trustedTemplates holds an operator template that uses every denied field,
// to prove agent templates are not filtered (spec R3).
func trustedTemplates() map[string]AgentPodTemplate {
	return map[string]AgentPodTemplate{
		"dind": {Spec: map[string]any{
			"serviceAccountName": "builder",
			"hostNetwork":        true,
			"containers": []any{
				map[string]any{"name": "job", "image": "alpine:3.20"},
				map[string]any{"name": "docker", "image": "docker:24-dind",
					"securityContext": map[string]any{"privileged": true},
					"volumeMounts":    []any{map[string]any{"name": "host-root", "mountPath": "/host"}}},
			},
			"volumes": []any{
				map[string]any{"name": "host-root", "hostPath": map[string]any{"path": "/"}},
				map[string]any{"name": "creds", "secret": map[string]any{"secretName": "registry"}},
				map[string]any{"name": "cache", "emptyDir": map[string]any{}},
			},
		}},
	}
}

func TestJobPodPolicy_NamedTemplateUnaffected(t *testing.T) {
	p := &JobPodPolicy{}
	assert.NoError(t, p.CheckJobPodTemplate(trustedTemplates(), &dsl.PodTemplate{Name: "dind"}))
	assert.NoError(t, p.CheckJobPodTemplate(trustedTemplates(), &dsl.PodTemplate{Name: "dind", Override: &dsl.PodSpecPatch{
		Containers: []map[string]any{{"name": "trivy", "image": "aquasec/trivy", "volumeMounts": []any{map[string]any{"name": "cache", "mountPath": "/c"}}}},
	}}))
	// Unknown template: left to BuildPod.
	assert.NoError(t, p.CheckJobPodTemplate(trustedTemplates(), &dsl.PodTemplate{Name: "missing", Override: &dsl.PodSpecPatch{
		Containers: []map[string]any{{"name": "x", "image": "x", "securityContext": map[string]any{"privileged": true}}},
	}}))
}

// TestJobPodPolicy_OverrideContainerRules re-expresses every container-level
// inline case as an override container.
func TestJobPodPolicy_OverrideContainerRules(t *testing.T) {
	for _, tc := range inlinePolicyCases {
		if tc.container == nil {
			continue // PodSpec-level fields cannot be expressed in an override.
		}
		t.Run(tc.name, func(t *testing.T) {
			c := map[string]any{"name": "extra", "image": "alpine:3.20"}
			for k, v := range tc.container {
				c[k] = v
			}
			tmpl := &dsl.PodTemplate{Name: "dind", Override: &dsl.PodSpecPatch{Containers: []map[string]any{c}}}
			pe := policyErr(t, JobPodPolicy{}, trustedTemplates(), tmpl)
			require.NotNil(t, pe)
			require.Len(t, pe.Violations, 1, "%v", pe.Violations)
			assert.Equal(t, "podTemplate.override.containers[0]"+tc.path[len("podTemplate.spec.containers[0]"):], pe.Violations[0].Path)
			assert.Equal(t, tc.allow, pe.Violations[0].Allow)
			if tc.allow != "" {
				fix := tc.fix
				assert.NoError(t, fix.CheckJobPodTemplate(trustedTemplates(), tmpl))
			}
		})
	}
}

func TestJobPodPolicy_OverrideVolumes(t *testing.T) {
	tmpl := &dsl.PodTemplate{Name: "dind", Override: &dsl.PodSpecPatch{
		Volumes: []map[string]any{{"name": "cache", "hostPath": map[string]any{"path": "/var/lib"}}}, // replaces the template's emptyDir
	}}
	pe := policyErr(t, JobPodPolicy{}, trustedTemplates(), tmpl)
	require.NotNil(t, pe)
	require.Len(t, pe.Violations, 1)
	assert.Equal(t, "podTemplate.override.volumes[0].hostPath", pe.Violations[0].Path)
	assert.NoError(t, (&JobPodPolicy{AllowHostAccess: true}).CheckJobPodTemplate(trustedTemplates(), tmpl))
}

// TestJobPodPolicy_OverrideMountOfTemplateVolume is spec §5.6 step 3.
func TestJobPodPolicy_OverrideMountOfTemplateVolume(t *testing.T) {
	for _, tc := range []struct {
		volume string
		allow  string
		fix    JobPodPolicy
	}{
		{"host-root", "allowHostAccess: true", JobPodPolicy{AllowHostAccess: true}},
		{"creds", `allowSecrets: ["registry"]`, JobPodPolicy{AllowSecrets: []string{"registry"}}},
	} {
		t.Run(tc.volume, func(t *testing.T) {
			tmpl := &dsl.PodTemplate{Name: "dind", Override: &dsl.PodSpecPatch{Containers: []map[string]any{
				{"name": "sneaky", "image": "alpine:3.20", "volumeMounts": []any{map[string]any{"name": tc.volume, "mountPath": "/m"}}},
			}}}
			pe := policyErr(t, JobPodPolicy{}, trustedTemplates(), tmpl)
			require.NotNil(t, pe)
			require.Len(t, pe.Violations, 1)
			assert.Equal(t, "podTemplate.override.containers[0].volumeMounts[0]", pe.Violations[0].Path)
			assert.Contains(t, pe.Violations[0].What, `mounts agent template volume "`+tc.volume+`", which is `)
			assert.Equal(t, tc.allow, pe.Violations[0].Allow)
			fix := tc.fix
			assert.NoError(t, fix.CheckJobPodTemplate(trustedTemplates(), tmpl))
		})
	}
}

// A patch volume that replaces a template volume is the one checked; the
// template's original no longer exists in the Pod.
func TestJobPodPolicy_OverrideReplacedTemplateVolumeIsNotLookedUp(t *testing.T) {
	tmpl := &dsl.PodTemplate{Name: "dind", Override: &dsl.PodSpecPatch{
		Volumes:    []map[string]any{{"name": "host-root", "emptyDir": map[string]any{}}},
		Containers: []map[string]any{{"name": "c", "image": "alpine:3.20", "volumeMounts": []any{map[string]any{"name": "host-root", "mountPath": "/m"}}}},
	}}
	assert.NoError(t, (&JobPodPolicy{}).CheckJobPodTemplate(trustedTemplates(), tmpl))
}

// The effective ServiceAccount of an override is the template's (spec §5.5).
func TestJobPodPolicy_OverrideUsesTemplateServiceAccount(t *testing.T) {
	tmpl := &dsl.PodTemplate{Name: "dind", Override: &dsl.PodSpecPatch{
		Volumes: []map[string]any{{"name": "tok", "projected": map[string]any{"sources": []any{map[string]any{"serviceAccountToken": map[string]any{"path": "t"}}}}}},
	}}
	pe := policyErr(t, JobPodPolicy{}, trustedTemplates(), tmpl)
	require.NotNil(t, pe)
	assert.Equal(t, `allowServiceAccounts: ["builder"]`, pe.Violations[0].Allow)
	assert.NoError(t, (&JobPodPolicy{AllowServiceAccounts: []string{"builder"}}).CheckJobPodTemplate(trustedTemplates(), tmpl))
}

// TestJobPodPolicy_OverrideR7NothingAllows: an override container cannot mount
// the artifact sidecar's credential volumes under the most permissive policy
// expressible (spec R7), for mounts and for block devices.
func TestJobPodPolicy_OverrideR7NothingAllows(t *testing.T) {
	all := JobPodPolicy{AllowHostAccess: true, AllowPrivileged: true, AllowUnconfinedProfiles: true, AllowNodeName: true,
		AllowSecrets: []string{"x"}, AllowServiceAccounts: []string{"default", "builder"}}
	for _, name := range []string{sidecarS3CredentialsVolumeName, sidecarBrokerTokenVolumeName} {
		for _, field := range []string{"volumeMounts", "volumeDevices"} {
			tmpl := &dsl.PodTemplate{Name: "dind", Override: &dsl.PodSpecPatch{Containers: []map[string]any{
				{"name": "thief", "image": "alpine:3.20", field: []any{map[string]any{"name": name, "mountPath": "/x", "devicePath": "/dev/x"}}},
			}}}
			pe := policyErr(t, all, trustedTemplates(), tmpl)
			require.NotNil(t, pe, "%s %s", field, name)
			require.Len(t, pe.Violations, 1)
			assert.Equal(t, "podTemplate.override.containers[0]."+field+"[0]", pe.Violations[0].Path)
			assert.Equal(t, "", pe.Violations[0].Allow)
		}
	}
}
