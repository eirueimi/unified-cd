package k8sagent

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

// jsonFieldNames lists the JSON names of t's fields, descending into
// embedded (inline) structs, the way encoding/json sees them.
func jsonFieldNames(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if f.Anonymous && name == "" {
			out = append(out, jsonFieldNames(f.Type)...)
			continue
		}
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// TestJobPodPolicy_TablesClassifyEveryField is the fail-closed guard of the
// allowlist: a k8s.io/api bump that adds a field fails here, naming it, until
// someone classifies it in podpolicy.go (spec §5).
func TestJobPodPolicy_TablesClassifyEveryField(t *testing.T) {
	cases := []struct {
		typ   reflect.Type
		table map[string]fieldClass
	}{
		{reflect.TypeOf(corev1.PodSpec{}), podSpecFieldClass},
		{reflect.TypeOf(corev1.Container{}), containerFieldClass},
		{reflect.TypeOf(corev1.VolumeSource{}), volumeSourceClass},
		{reflect.TypeOf(corev1.VolumeProjection{}), projectionSourceClass},
		{reflect.TypeOf(corev1.PodSecurityContext{}), podSecurityContextFieldClass},
		{reflect.TypeOf(corev1.SecurityContext{}), securityContextFieldClass},
	}
	for _, tc := range cases {
		t.Run(tc.typ.Name(), func(t *testing.T) {
			fields := jsonFieldNames(tc.typ)
			var tableKeys []string
			for k := range tc.table {
				tableKeys = append(tableKeys, k)
			}
			sort.Strings(tableKeys)
			assert.Equal(t, fields, tableKeys,
				"every %s JSON field must be classified exactly once in podpolicy.go, and every table entry must exist on the struct", tc.typ.Name())
		})
	}
}

// inlineJob is a podTemplate with one "job" container plus extra pod-level keys.
func inlineJob(container map[string]any, pod map[string]any) *dsl.PodTemplate {
	c := map[string]any{"name": "job", "image": "alpine:3.20"}
	for k, v := range container {
		c[k] = v
	}
	spec := map[string]any{"containers": []any{c}}
	for k, v := range pod {
		spec[k] = v
	}
	return &dsl.PodTemplate{Spec: spec}
}

func policyErr(t *testing.T, p JobPodPolicy, agentTmpls map[string]AgentPodTemplate, tmpl *dsl.PodTemplate) *JobPodPolicyError {
	t.Helper()
	err := p.CheckJobPodTemplate(agentTmpls, tmpl)
	if err == nil {
		return nil
	}
	var pe *JobPodPolicyError
	require.ErrorAs(t, err, &pe)
	return pe
}

func TestJobPodPolicy_BenignInlinePasses(t *testing.T) {
	tmpl := inlineJob(map[string]any{
		"env":       []any{map[string]any{"name": "A", "value": "1"}},
		"resources": map[string]any{"limits": map[string]any{"cpu": "1"}},
		"securityContext": map[string]any{"runAsUser": 1000, "allowPrivilegeEscalation": false,
			"capabilities": map[string]any{"drop": []any{"ALL"}}},
	}, map[string]any{
		"securityContext": map[string]any{"fsGroup": 1000},
		"nodeSelector":    map[string]any{"pool": "ci"},
		"tolerations":     []any{map[string]any{"key": "ci", "operator": "Exists"}},
		"volumes":         []any{map[string]any{"name": "scratch", "emptyDir": map[string]any{}}},
	})
	assert.NoError(t, (&JobPodPolicy{}).CheckJobPodTemplate(nil, tmpl))
}

func TestJobPodPolicy_NilAndDefaultPodPass(t *testing.T) {
	p := &JobPodPolicy{}
	assert.NoError(t, p.CheckJobPodTemplate(nil, nil))
	assert.NoError(t, p.CheckJobPodTemplate(nil, &dsl.PodTemplate{Workspace: &dsl.WorkspaceConfig{MountPath: "/w"}}))
}

func TestJobPodPolicy_DecodeErrorIsLeftToBuildPod(t *testing.T) {
	tmpl := &dsl.PodTemplate{Spec: map[string]any{"containers": "not-a-list"}}
	assert.NoError(t, (&JobPodPolicy{}).CheckJobPodTemplate(nil, tmpl))
}

func TestJobPodPolicy_ErrorShape(t *testing.T) {
	tmpl := inlineJob(map[string]any{"securityContext": map[string]any{"privileged": true}},
		map[string]any{"volumes": []any{map[string]any{"name": "root", "hostPath": map[string]any{"path": "/"}}}})
	pe := policyErr(t, JobPodPolicy{}, nil, tmpl)
	require.NotNil(t, pe)
	require.Len(t, pe.Violations, 2)
	assert.Equal(t, "podTemplate.spec.containers[0].securityContext.privileged", pe.Violations[0].Path)
	assert.Equal(t, "allowPrivileged: true", pe.Violations[0].Allow)
	assert.Equal(t, "podTemplate.spec.volumes[0].hostPath", pe.Violations[1].Path)
	assert.Equal(t, "allowHostAccess: true", pe.Violations[1].Allow)
	msg := pe.Error()
	assert.True(t, strings.HasPrefix(msg, "k8s: job pod policy: the job's podTemplate sets 2 field(s)"), msg)
	assert.Contains(t, msg, `(container "job"): privileged: true (allow with jobPodPolicy.allowPrivileged: true in the k8s-agent config)`)
	assert.Contains(t, msg, `"Job Pod field policy" in docs/operator-manual/kubernetes-integration.md`)
	// Deterministic: same input, same message.
	assert.Equal(t, msg, policyErr(t, JobPodPolicy{}, nil, tmpl).Error())
}

func TestJobPodPolicy_NeverAllowedSaysSo(t *testing.T) {
	tmpl := inlineJob(map[string]any{"volumeMounts": []any{map[string]any{"name": sidecarS3CredentialsVolumeName, "mountPath": "/x"}}}, nil)
	pe := policyErr(t, JobPodPolicy{}, nil, tmpl)
	require.NotNil(t, pe)
	assert.Equal(t, "", pe.Violations[0].Allow)
	assert.Contains(t, pe.Error(), "(no k8s-agent setting allows this)")
}

// TestJobPodPolicy_SafeSysctlListIsPinned pins the baseline safe-sysctl set
// (spec §5.4) so an accidental edit to the table is caught.
func TestJobPodPolicy_SafeSysctlListIsPinned(t *testing.T) {
	var got []string
	for k := range safeSysctls {
		got = append(got, k)
	}
	sort.Strings(got)
	assert.Equal(t, []string{
		"kernel.shm_rmid_forced",
		"net.ipv4.ip_local_port_range",
		"net.ipv4.ip_local_reserved_ports",
		"net.ipv4.ip_unprivileged_port_start",
		"net.ipv4.ping_group_range",
		"net.ipv4.tcp_fin_timeout",
		"net.ipv4.tcp_keepalive_intvl",
		"net.ipv4.tcp_keepalive_probes",
		"net.ipv4.tcp_keepalive_time",
		"net.ipv4.tcp_syncookies",
	}, got)
}

func TestJobPodPolicy_Sysctls(t *testing.T) {
	sysctl := func(name string) *dsl.PodTemplate {
		return inlineJob(nil, map[string]any{"securityContext": map[string]any{
			"sysctls": []any{map[string]any{"name": name, "value": "1"}}}})
	}
	assert.NoError(t, (&JobPodPolicy{}).CheckJobPodTemplate(nil, sysctl("net.ipv4.tcp_syncookies")))
	pe := policyErr(t, JobPodPolicy{}, nil, sysctl("kernel.msgmax"))
	require.NotNil(t, pe)
	require.Len(t, pe.Violations, 1)
	assert.Equal(t, "podTemplate.spec.securityContext.sysctls[0]", pe.Violations[0].Path)
	assert.Equal(t, "allowPrivileged: true", pe.Violations[0].Allow)
	assert.NoError(t, (&JobPodPolicy{AllowPrivileged: true}).CheckJobPodTemplate(nil, sysctl("kernel.msgmax")))
}

// dropEntry removes key from table for the duration of the test, to drive the
// "unclassified key" deny branch without a production seam.
func dropEntry(t *testing.T, table map[string]fieldClass, key string) {
	t.Helper()
	old, ok := table[key]
	require.True(t, ok, "table has no %q to drop", key)
	delete(table, key)
	t.Cleanup(func() { table[key] = old })
}

func TestJobPodPolicy_UnclassifiedPodSpecKeyDenied(t *testing.T) {
	dropEntry(t, podSpecFieldClass, "nodeSelector")
	tmpl := inlineJob(nil, map[string]any{"nodeSelector": map[string]any{"pool": "ci"}})
	pe := policyErr(t, JobPodPolicy{}, nil, tmpl)
	require.NotNil(t, pe)
	require.Len(t, pe.Violations, 1)
	assert.Equal(t, "podTemplate.spec.nodeSelector", pe.Violations[0].Path)
	assert.Contains(t, pe.Violations[0].What, "not classified")
	assert.Equal(t, "", pe.Violations[0].Allow)
}

func TestJobPodPolicy_UnclassifiedVolumeSourceDenied(t *testing.T) {
	dropEntry(t, volumeSourceClass, "emptyDir")
	tmpl := inlineJob(nil, map[string]any{"volumes": []any{map[string]any{"name": "scratch", "emptyDir": map[string]any{}}}})
	pe := policyErr(t, JobPodPolicy{}, nil, tmpl)
	require.NotNil(t, pe)
	require.Len(t, pe.Violations, 1)
	assert.Equal(t, "podTemplate.spec.volumes[0].emptyDir", pe.Violations[0].Path)
	assert.Contains(t, pe.Violations[0].What, "does not classify")
}
