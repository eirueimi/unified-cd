package k8sagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

type policyCase struct {
	name      string
	container map[string]any // merged into the "job" container
	pod       map[string]any // merged into the PodSpec
	path      string         // expected single violation path
	allow     string         // expected Allow ("" = nothing allows it)
	fix       JobPodPolicy   // a policy under which the case passes (zero when allow == "")
}

// projectedSources builds a pod-level volumes list holding one projected volume.
func projectedSources(sources ...any) map[string]any {
	return map[string]any{"volumes": []any{map[string]any{"name": "pr", "projected": map[string]any{"sources": sources}}}}
}

var inlinePolicyCases = []policyCase{
	// §5.1 PodSpec
	{name: "serviceAccountName", pod: map[string]any{"serviceAccountName": "deployer"},
		path: "podTemplate.spec.serviceAccountName", allow: `allowServiceAccounts: ["deployer"]`,
		fix: JobPodPolicy{AllowServiceAccounts: []string{"deployer"}}},
	{name: "deprecated serviceAccount", pod: map[string]any{"serviceAccount": "deployer"},
		path: "podTemplate.spec.serviceAccount", allow: `allowServiceAccounts: ["deployer"]`,
		fix: JobPodPolicy{AllowServiceAccounts: []string{"deployer"}}},
	{name: "automount true on default", pod: map[string]any{"automountServiceAccountToken": true},
		path: "podTemplate.spec.automountServiceAccountToken", allow: `allowServiceAccounts: ["default"]`,
		fix: JobPodPolicy{AllowServiceAccounts: []string{"default"}}},
	{name: "nodeName", pod: map[string]any{"nodeName": "node-1"},
		path: "podTemplate.spec.nodeName", allow: "allowNodeName: true", fix: JobPodPolicy{AllowNodeName: true}},
	{name: "hostNetwork", pod: map[string]any{"hostNetwork": true},
		path: "podTemplate.spec.hostNetwork", allow: "allowHostAccess: true", fix: JobPodPolicy{AllowHostAccess: true}},
	{name: "hostPID", pod: map[string]any{"hostPID": true},
		path: "podTemplate.spec.hostPID", allow: "allowHostAccess: true", fix: JobPodPolicy{AllowHostAccess: true}},
	{name: "hostIPC", pod: map[string]any{"hostIPC": true},
		path: "podTemplate.spec.hostIPC", allow: "allowHostAccess: true", fix: JobPodPolicy{AllowHostAccess: true}},
	{name: "imagePullSecrets", pod: map[string]any{"imagePullSecrets": []any{map[string]any{"name": "regcred"}}},
		path: "podTemplate.spec.imagePullSecrets[0]", allow: `allowImagePullSecrets: ["regcred"]`,
		fix: JobPodPolicy{AllowImagePullSecrets: []string{"regcred"}}},
	{name: "opt-in runtimeClassName", pod: map[string]any{"runtimeClassName": "gvisor"},
		path: "podTemplate.spec.runtimeClassName", allow: "allowPodSpecFields: [runtimeClassName]",
		fix: JobPodPolicy{AllowPodSpecFields: []string{"runtimeClassName"}}},
	{name: "opt-in shareProcessNamespace", pod: map[string]any{"shareProcessNamespace": true},
		path: "podTemplate.spec.shareProcessNamespace", allow: "allowPodSpecFields: [shareProcessNamespace]",
		fix: JobPodPolicy{AllowPodSpecFields: []string{"shareProcessNamespace"}}},
	{name: "opt-in priorityClassName", pod: map[string]any{"priorityClassName": "high"},
		path: "podTemplate.spec.priorityClassName", allow: "allowPodSpecFields: [priorityClassName]",
		fix: JobPodPolicy{AllowPodSpecFields: []string{"priorityClassName"}}},
	{name: "never ephemeralContainers", pod: map[string]any{"ephemeralContainers": []any{map[string]any{"name": "dbg", "image": "busybox"}}},
		path: "podTemplate.spec.ephemeralContainers", allow: ""},

	// §5.4 pod securityContext
	{name: "pod seccomp Unconfined", pod: map[string]any{"securityContext": map[string]any{"seccompProfile": map[string]any{"type": "Unconfined"}}},
		path: "podTemplate.spec.securityContext.seccompProfile.type", allow: "allowUnconfinedProfiles: true",
		fix: JobPodPolicy{AllowUnconfinedProfiles: true}},
	{name: "pod apparmor Unconfined", pod: map[string]any{"securityContext": map[string]any{"appArmorProfile": map[string]any{"type": "Unconfined"}}},
		path: "podTemplate.spec.securityContext.appArmorProfile.type", allow: "allowUnconfinedProfiles: true",
		fix: JobPodPolicy{AllowUnconfinedProfiles: true}},
	{name: "pod unsafe sysctl", pod: map[string]any{"securityContext": map[string]any{"sysctls": []any{map[string]any{"name": "kernel.msgmax", "value": "1"}}}},
		path: "podTemplate.spec.securityContext.sysctls[0]", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	{name: "pod seLinux role", pod: map[string]any{"securityContext": map[string]any{"seLinuxOptions": map[string]any{"role": "sysadm_r"}}},
		path: "podTemplate.spec.securityContext.seLinuxOptions", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	{name: "pod seLinux type outside baseline", pod: map[string]any{"securityContext": map[string]any{"seLinuxOptions": map[string]any{"type": "spc_t"}}},
		path: "podTemplate.spec.securityContext.seLinuxOptions", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	{name: "pod hostProcess", pod: map[string]any{"securityContext": map[string]any{"windowsOptions": map[string]any{"hostProcess": true}}},
		path: "podTemplate.spec.securityContext.windowsOptions.hostProcess", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},

	// §5.2 / §5.4 container
	{name: "privileged", container: map[string]any{"securityContext": map[string]any{"privileged": true}},
		path: "podTemplate.spec.containers[0].securityContext.privileged", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	{name: "init container privileged (initContainers walk)",
		pod:  map[string]any{"initContainers": []any{map[string]any{"name": "init", "image": "alpine:3.20", "securityContext": map[string]any{"privileged": true}}}},
		path: "podTemplate.spec.initContainers[0].securityContext.privileged", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	{name: "capability SYS_ADMIN", container: map[string]any{"securityContext": map[string]any{"capabilities": map[string]any{"add": []any{"SYS_ADMIN"}}}},
		path: "podTemplate.spec.containers[0].securityContext.capabilities.add[0]", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	{name: "capability cap_net_admin", container: map[string]any{"securityContext": map[string]any{"capabilities": map[string]any{"add": []any{"cap_net_admin"}}}},
		path: "podTemplate.spec.containers[0].securityContext.capabilities.add[0]", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	// Capability names compare exactly, as the Kubernetes API and the Pod
	// Security baseline do: "CAP_CHOWN" and "chown" are not baseline names.
	{name: "capability CAP_CHOWN spelling", container: map[string]any{"securityContext": map[string]any{"capabilities": map[string]any{"add": []any{"CAP_CHOWN"}}}},
		path: "podTemplate.spec.containers[0].securityContext.capabilities.add[0]", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	{name: "capability lowercase chown", container: map[string]any{"securityContext": map[string]any{"capabilities": map[string]any{"add": []any{"chown"}}}},
		path: "podTemplate.spec.containers[0].securityContext.capabilities.add[0]", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	// hostAliases, dnsPolicy and dnsConfig are Pod-wide, so they steer the
	// artifact sidecar's connections too: denied unless opted in.
	{name: "opt-in hostAliases", pod: map[string]any{"hostAliases": []any{map[string]any{"ip": "10.0.0.1", "hostnames": []any{"broker.local"}}}},
		path: "podTemplate.spec.hostAliases", allow: "allowPodSpecFields: [hostAliases]",
		fix: JobPodPolicy{AllowPodSpecFields: []string{"hostAliases"}}},
	{name: "opt-in dnsPolicy", pod: map[string]any{"dnsPolicy": "None"},
		path: "podTemplate.spec.dnsPolicy", allow: "allowPodSpecFields: [dnsPolicy]",
		fix: JobPodPolicy{AllowPodSpecFields: []string{"dnsPolicy"}}},
	{name: "opt-in dnsConfig", pod: map[string]any{"dnsConfig": map[string]any{"nameservers": []any{"10.0.0.2"}}},
		path: "podTemplate.spec.dnsConfig", allow: "allowPodSpecFields: [dnsConfig]",
		fix: JobPodPolicy{AllowPodSpecFields: []string{"dnsConfig"}}},
	{name: "procMount Unmasked", container: map[string]any{"securityContext": map[string]any{"procMount": "Unmasked"}},
		path: "podTemplate.spec.containers[0].securityContext.procMount", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	{name: "container apparmor Unconfined", container: map[string]any{"securityContext": map[string]any{"appArmorProfile": map[string]any{"type": "Unconfined"}}},
		path: "podTemplate.spec.containers[0].securityContext.appArmorProfile.type", allow: "allowUnconfinedProfiles: true",
		fix: JobPodPolicy{AllowUnconfinedProfiles: true}},
	{name: "container seccomp Unconfined", container: map[string]any{"securityContext": map[string]any{"seccompProfile": map[string]any{"type": "Unconfined"}}},
		path: "podTemplate.spec.containers[0].securityContext.seccompProfile.type", allow: "allowUnconfinedProfiles: true",
		fix: JobPodPolicy{AllowUnconfinedProfiles: true}},
	{name: "container seLinux user", container: map[string]any{"securityContext": map[string]any{"seLinuxOptions": map[string]any{"user": "system_u"}}},
		path: "podTemplate.spec.containers[0].securityContext.seLinuxOptions", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	{name: "container seLinux type outside baseline", container: map[string]any{"securityContext": map[string]any{"seLinuxOptions": map[string]any{"type": "spc_t"}}},
		path: "podTemplate.spec.containers[0].securityContext.seLinuxOptions", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	{name: "container hostProcess", container: map[string]any{"securityContext": map[string]any{"windowsOptions": map[string]any{"hostProcess": true}}},
		path: "podTemplate.spec.containers[0].securityContext.windowsOptions.hostProcess", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	{name: "hostPort", container: map[string]any{"ports": []any{map[string]any{"containerPort": 80, "hostPort": 8080}}},
		path: "podTemplate.spec.containers[0].ports[0]", allow: "allowHostAccess: true", fix: JobPodPolicy{AllowHostAccess: true}},
	{name: "probe httpGet host", container: map[string]any{"livenessProbe": map[string]any{"httpGet": map[string]any{"host": "169.254.169.254", "port": 80}}},
		path: "podTemplate.spec.containers[0].livenessProbe.httpGet.host", allow: "allowHostAccess: true", fix: JobPodPolicy{AllowHostAccess: true}},
	{name: "readinessProbe tcpSocket host", container: map[string]any{"readinessProbe": map[string]any{"tcpSocket": map[string]any{"host": "10.0.0.1", "port": 22}}},
		path: "podTemplate.spec.containers[0].readinessProbe.tcpSocket.host", allow: "allowHostAccess: true", fix: JobPodPolicy{AllowHostAccess: true}},
	{name: "startupProbe httpGet host", container: map[string]any{"startupProbe": map[string]any{"httpGet": map[string]any{"host": "169.254.169.254", "port": 80}}},
		path: "podTemplate.spec.containers[0].startupProbe.httpGet.host", allow: "allowHostAccess: true", fix: JobPodPolicy{AllowHostAccess: true}},
	{name: "postStart httpGet host", container: map[string]any{"lifecycle": map[string]any{"postStart": map[string]any{"httpGet": map[string]any{"host": "169.254.169.254", "port": 80}}}},
		path: "podTemplate.spec.containers[0].lifecycle.postStart.httpGet.host", allow: "allowHostAccess: true", fix: JobPodPolicy{AllowHostAccess: true}},
	{name: "preStop tcpSocket host", container: map[string]any{"lifecycle": map[string]any{"preStop": map[string]any{"tcpSocket": map[string]any{"host": "10.0.0.1", "port": 22}}}},
		path: "podTemplate.spec.containers[0].lifecycle.preStop.tcpSocket.host", allow: "allowHostAccess: true", fix: JobPodPolicy{AllowHostAccess: true}},
	{name: "env secretKeyRef", container: map[string]any{"env": []any{map[string]any{"name": "P", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "db", "key": "pw"}}}}},
		path: "podTemplate.spec.containers[0].env[0].valueFrom.secretKeyRef", allow: `allowSecrets: ["db"]`,
		fix: JobPodPolicy{AllowSecrets: []string{"db"}}},
	{name: "envFrom secretRef", container: map[string]any{"envFrom": []any{map[string]any{"secretRef": map[string]any{"name": "unified-cd-s3"}}}},
		path: "podTemplate.spec.containers[0].envFrom[0].secretRef", allow: `allowSecrets: ["unified-cd-s3"]`,
		fix: JobPodPolicy{AllowSecrets: []string{"unified-cd-s3"}}},
	{name: "R7 broker token mount", container: map[string]any{"volumeMounts": []any{map[string]any{"name": sidecarBrokerTokenVolumeName, "mountPath": "/t"}}},
		path: "podTemplate.spec.containers[0].volumeMounts[0]", allow: ""},
	{name: "R7 s3 credentials volumeDevice", container: map[string]any{"volumeDevices": []any{map[string]any{"name": sidecarS3CredentialsVolumeName, "devicePath": "/dev/x"}}},
		path: "podTemplate.spec.containers[0].volumeDevices[0]", allow: ""},

	// §5.3 volumes
	{name: "hostPath volume", pod: map[string]any{"volumes": []any{map[string]any{"name": "h", "hostPath": map[string]any{"path": "/"}}}},
		path: "podTemplate.spec.volumes[0].hostPath", allow: "allowHostAccess: true", fix: JobPodPolicy{AllowHostAccess: true}},
	{name: "secret volume", pod: map[string]any{"volumes": []any{map[string]any{"name": "s", "secret": map[string]any{"secretName": "db"}}}},
		path: "podTemplate.spec.volumes[0].secret", allow: `allowSecrets: ["db"]`, fix: JobPodPolicy{AllowSecrets: []string{"db"}}},
	{name: "pvc volume", pod: map[string]any{"volumes": []any{map[string]any{"name": "p", "persistentVolumeClaim": map[string]any{"claimName": "ci-main-go-cache"}}}},
		path: "podTemplate.spec.volumes[0].persistentVolumeClaim", allow: `allowPersistentVolumeClaims: ["ci-main-go-cache"]`,
		fix: JobPodPolicy{AllowPersistentVolumeClaims: []string{"ci-main-go-cache"}}},
	{name: "nfs volume", pod: map[string]any{"volumes": []any{map[string]any{"name": "n", "nfs": map[string]any{"server": "nfs", "path": "/"}}}},
		path: "podTemplate.spec.volumes[0].nfs", allow: "allowVolumeTypes: [nfs]", fix: JobPodPolicy{AllowVolumeTypes: []string{"nfs"}}},
	{name: "projected secret", pod: projectedSources(map[string]any{"secret": map[string]any{"name": "db"}}),
		path: "podTemplate.spec.volumes[0].projected.sources[0].secret", allow: `allowSecrets: ["db"]`,
		fix: JobPodPolicy{AllowSecrets: []string{"db"}}},
	{name: "projected SA token", pod: projectedSources(map[string]any{"serviceAccountToken": map[string]any{"path": "t"}}),
		path: "podTemplate.spec.volumes[0].projected.sources[0].serviceAccountToken", allow: `allowServiceAccounts: ["default"]`,
		fix: JobPodPolicy{AllowServiceAccounts: []string{"default"}}},
	{name: "projected podCertificate", pod: projectedSources(map[string]any{"podCertificate": map[string]any{
		"signerName": "example.com/signer", "keyType": "ED25519", "credentialBundlePath": "cred.pem"}}),
		path: "podTemplate.spec.volumes[0].projected.sources[0].podCertificate", allow: `allowServiceAccounts: ["default"]`,
		fix: JobPodPolicy{AllowServiceAccounts: []string{"default"}}},
}

func TestJobPodPolicy_InlineDeniedByDefault(t *testing.T) {
	for _, tc := range inlinePolicyCases {
		t.Run(tc.name, func(t *testing.T) {
			pe := policyErr(t, JobPodPolicy{}, nil, inlineJob(tc.container, tc.pod))
			require.NotNil(t, pe, "expected a violation")
			require.Len(t, pe.Violations, 1, "%v", pe.Violations)
			assert.Equal(t, tc.path, pe.Violations[0].Path)
			assert.Equal(t, tc.allow, pe.Violations[0].Allow)
		})
	}
}

func TestJobPodPolicy_InlineOptInRestores(t *testing.T) {
	for _, tc := range inlinePolicyCases {
		if tc.allow == "" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			fix := tc.fix
			assert.NoError(t, fix.CheckJobPodTemplate(nil, inlineJob(tc.container, tc.pod)))
		})
	}
}

// TestJobPodPolicy_R7NothingAllows: the credential volumes stay refused under
// the most permissive policy expressible, for mounts and for block devices.
func TestJobPodPolicy_R7NothingAllows(t *testing.T) {
	all := JobPodPolicy{AllowHostAccess: true, AllowPrivileged: true, AllowUnconfinedProfiles: true, AllowNodeName: true,
		AllowSecrets: []string{"x"}, AllowServiceAccounts: []string{"default"}}
	for _, name := range []string{sidecarS3CredentialsVolumeName, sidecarBrokerTokenVolumeName} {
		for _, field := range []string{"volumeMounts", "volumeDevices"} {
			tmpl := inlineJob(map[string]any{field: []any{map[string]any{"name": name, "mountPath": "/x", "devicePath": "/dev/x"}}}, nil)
			pe := policyErr(t, all, nil, tmpl)
			require.NotNil(t, pe, "%s %s", field, name)
			assert.Equal(t, "", pe.Violations[0].Allow)
		}
	}
}

// TestJobPodPolicy_NameListsAreExact: allowing one name does not allow another.
func TestJobPodPolicy_NameListsAreExact(t *testing.T) {
	p := JobPodPolicy{AllowSecrets: []string{"db"}, AllowServiceAccounts: []string{"builder"}}
	assert.NotNil(t, policyErr(t, p, nil, inlineJob(map[string]any{"envFrom": []any{map[string]any{"secretRef": map[string]any{"name": "other"}}}}, nil)))
	assert.NotNil(t, policyErr(t, p, nil, inlineJob(nil, map[string]any{"serviceAccountName": "deployer"})))
	// A projected token is checked against the Pod's effective account, not "default".
	tok := map[string]any{"volumes": []any{map[string]any{"name": "t", "projected": map[string]any{"sources": []any{map[string]any{"serviceAccountToken": map[string]any{"path": "t"}}}}}},
		"serviceAccountName": "builder"}
	assert.NoError(t, p.CheckJobPodTemplate(nil, inlineJob(nil, tok)))
}

// TestJobPodPolicy_OtherNamesStillFail: an opt-in for one object or type does
// not allow a different one.
func TestJobPodPolicy_OtherNamesStillFail(t *testing.T) {
	pvc := func(claim string) *dsl.PodTemplate {
		return inlineJob(nil, map[string]any{"volumes": []any{map[string]any{"name": "p", "persistentVolumeClaim": map[string]any{"claimName": claim}}}})
	}
	pvcPolicy := JobPodPolicy{AllowPersistentVolumeClaims: []string{"ci-main-go-cache"}}
	assert.NoError(t, pvcPolicy.CheckJobPodTemplate(nil, pvc("ci-main-go-cache")))
	pe := policyErr(t, pvcPolicy, nil, pvc("other-claim"))
	require.NotNil(t, pe)
	require.Len(t, pe.Violations, 1)
	assert.Equal(t, "podTemplate.spec.volumes[0].persistentVolumeClaim", pe.Violations[0].Path)
	assert.Equal(t, `allowPersistentVolumeClaims: ["other-claim"]`, pe.Violations[0].Allow)

	pull := func(name string) *dsl.PodTemplate {
		return inlineJob(nil, map[string]any{"imagePullSecrets": []any{map[string]any{"name": name}}})
	}
	pullPolicy := JobPodPolicy{AllowImagePullSecrets: []string{"regcred"}}
	assert.NoError(t, pullPolicy.CheckJobPodTemplate(nil, pull("regcred")))
	pe = policyErr(t, pullPolicy, nil, pull("other-cred"))
	require.NotNil(t, pe)
	require.Len(t, pe.Violations, 1)
	assert.Equal(t, "podTemplate.spec.imagePullSecrets[0]", pe.Violations[0].Path)
	assert.Equal(t, `allowImagePullSecrets: ["other-cred"]`, pe.Violations[0].Allow)

	vol := func(typ string, body map[string]any) *dsl.PodTemplate {
		return inlineJob(nil, map[string]any{"volumes": []any{map[string]any{"name": "v", typ: body}}})
	}
	typePolicy := JobPodPolicy{AllowVolumeTypes: []string{"csi"}}
	assert.NoError(t, typePolicy.CheckJobPodTemplate(nil, vol("csi", map[string]any{"driver": "d"})))
	pe = policyErr(t, typePolicy, nil, vol("nfs", map[string]any{"server": "nfs", "path": "/"}))
	require.NotNil(t, pe)
	require.Len(t, pe.Violations, 1)
	assert.Equal(t, "podTemplate.spec.volumes[0].nfs", pe.Violations[0].Path)
	assert.Equal(t, "allowVolumeTypes: [nfs]", pe.Violations[0].Allow)
}

// TestJobPodPolicy_AutomountServiceAccountToken: false and unset never grant a
// token, so only true is refused.
func TestJobPodPolicy_AutomountServiceAccountToken(t *testing.T) {
	p := &JobPodPolicy{}
	assert.NoError(t, p.CheckJobPodTemplate(nil, inlineJob(nil, map[string]any{"automountServiceAccountToken": false})))
	assert.NoError(t, p.CheckJobPodTemplate(nil, inlineJob(nil, nil)))
	pe := policyErr(t, JobPodPolicy{}, nil, inlineJob(nil, map[string]any{"automountServiceAccountToken": true}))
	require.NotNil(t, pe)
	assert.Equal(t, "podTemplate.spec.automountServiceAccountToken", pe.Violations[0].Path)
}

// TestJobPodPolicy_BaselineBoundaries pins spec R8.
func TestJobPodPolicy_BaselineBoundaries(t *testing.T) {
	ok := []map[string]any{
		{"securityContext": map[string]any{"allowPrivilegeEscalation": true}},
		{"securityContext": map[string]any{"runAsUser": 0}},
		{"securityContext": map[string]any{"capabilities": map[string]any{"add": []any{"NET_BIND_SERVICE", "CHOWN"}}}},
		{"securityContext": map[string]any{"seccompProfile": map[string]any{"type": "RuntimeDefault"}}},
		{"securityContext": map[string]any{"privileged": false}},
		{"securityContext": map[string]any{"seLinuxOptions": map[string]any{"type": "container_t"}}},
	}
	for _, c := range ok {
		assert.NoError(t, (&JobPodPolicy{}).CheckJobPodTemplate(nil, inlineJob(c, nil)), "%v", c)
	}
	assert.NoError(t, (&JobPodPolicy{}).CheckJobPodTemplate(nil, inlineJob(nil, map[string]any{
		"automountServiceAccountToken": false, "hostNetwork": false,
		"securityContext": map[string]any{"sysctls": []any{map[string]any{"name": "net.ipv4.tcp_syncookies", "value": "1"}}},
	})))
}
