package k8sagent

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

// JobPodPolicy is the k8s-agent's opt-in list for the Pod fields a job's OWN
// podTemplate content may use (an inline podTemplate.spec, an override patch).
// Agent-side podTemplates are operator-written and never checked. The zero
// value is the most restrictive policy. See
// docs/superpowers/specs/2026-10-26-k8s-job-pod-field-policy-design.md.
type JobPodPolicy struct {
	// Name lists. Each entry is one object name; there are no wildcards.
	AllowServiceAccounts        []string `yaml:"allowServiceAccounts,omitempty"`
	AllowSecrets                []string `yaml:"allowSecrets,omitempty"`
	AllowImagePullSecrets       []string `yaml:"allowImagePullSecrets,omitempty"`
	AllowPersistentVolumeClaims []string `yaml:"allowPersistentVolumeClaims,omitempty"`
	// Categories.
	AllowHostAccess         bool `yaml:"allowHostAccess,omitempty"`
	AllowPrivileged         bool `yaml:"allowPrivileged,omitempty"`
	AllowUnconfinedProfiles bool `yaml:"allowUnconfinedProfiles,omitempty"`
	AllowNodeName           bool `yaml:"allowNodeName,omitempty"`
	// Escape hatches for fields in the opt-in class.
	AllowPodSpecFields []string `yaml:"allowPodSpecFields,omitempty"`
	AllowVolumeTypes   []string `yaml:"allowVolumeTypes,omitempty"`
}

// fieldClass is how the policy treats one JSON field of a Kubernetes type.
type fieldClass int

const (
	// classAllowed fields are always allowed; their nested content may still
	// be checked by value.
	classAllowed fieldClass = iota + 1
	// classCategory fields are allowed only through a named jobPodPolicy
	// category, and are checked by value in typed code.
	classCategory
	// classOptIn fields are denied unless listed in allowPodSpecFields or
	// allowVolumeTypes.
	classOptIn
	// classNever fields cannot be set when a Pod is created.
	classNever
)

// podSpecFieldClass classifies every corev1.PodSpec field (spec §5.1).
// workloadRef is absent on purpose: k8s.io/api v0.36.1 tombstones it (the field
// is commented out of PodSpec), so the completeness test would reject an entry.
var podSpecFieldClass = map[string]fieldClass{
	"containers": classAllowed, "initContainers": classAllowed, "volumes": classAllowed,
	"securityContext": classAllowed, "restartPolicy": classAllowed,
	"terminationGracePeriodSeconds": classAllowed, "activeDeadlineSeconds": classAllowed,
	"dnsPolicy": classAllowed, "dnsConfig": classAllowed, "nodeSelector": classAllowed,
	"affinity": classAllowed, "tolerations": classAllowed, "topologySpreadConstraints": classAllowed,
	"hostname": classAllowed, "subdomain": classAllowed, "setHostnameAsFQDN": classAllowed,
	"hostAliases": classAllowed, "enableServiceLinks": classAllowed, "os": classAllowed,
	"hostUsers": classAllowed, "resources": classAllowed,

	"serviceAccountName": classCategory, "serviceAccount": classCategory,
	"automountServiceAccountToken": classCategory, "nodeName": classCategory,
	"hostNetwork": classCategory, "hostPID": classCategory, "hostIPC": classCategory,
	"imagePullSecrets": classCategory,

	"shareProcessNamespace": classOptIn, "runtimeClassName": classOptIn,
	"priorityClassName": classOptIn, "priority": classOptIn, "preemptionPolicy": classOptIn,
	"schedulerName": classOptIn, "readinessGates": classOptIn, "overhead": classOptIn,
	"schedulingGates": classOptIn, "resourceClaims": classOptIn, "hostnameOverride": classOptIn,
	"schedulingGroup": classOptIn,

	"ephemeralContainers": classNever,
}

// podSpecCategoryKey names the jobPodPolicy key governing each category-class
// PodSpec field, for configuration error messages.
var podSpecCategoryKey = map[string]string{
	"serviceAccountName": "allowServiceAccounts", "serviceAccount": "allowServiceAccounts",
	"automountServiceAccountToken": "allowServiceAccounts", "nodeName": "allowNodeName",
	"hostNetwork": "allowHostAccess", "hostPID": "allowHostAccess", "hostIPC": "allowHostAccess",
	"imagePullSecrets": "allowImagePullSecrets",
}

// containerFieldClass classifies every corev1.Container field (spec §5.2).
// All of them are allowed as fields; value rules live in checkContainer.
var containerFieldClass = map[string]fieldClass{
	"name": classAllowed, "image": classAllowed, "command": classAllowed, "args": classAllowed,
	"workingDir": classAllowed, "ports": classAllowed, "envFrom": classAllowed, "env": classAllowed,
	"resources": classAllowed, "resizePolicy": classAllowed, "restartPolicy": classAllowed,
	"restartPolicyRules": classAllowed, "volumeMounts": classAllowed, "volumeDevices": classAllowed,
	"livenessProbe": classAllowed, "readinessProbe": classAllowed, "startupProbe": classAllowed,
	"lifecycle": classAllowed, "terminationMessagePath": classAllowed,
	"terminationMessagePolicy": classAllowed, "imagePullPolicy": classAllowed,
	"securityContext": classAllowed, "stdin": classAllowed, "stdinOnce": classAllowed, "tty": classAllowed,
}

// volumeSourceClass classifies every corev1.VolumeSource type (spec §5.3).
var volumeSourceClass = map[string]fieldClass{
	"emptyDir": classAllowed, "configMap": classAllowed, "downwardAPI": classAllowed,
	"image": classAllowed, "projected": classAllowed,

	"hostPath": classCategory, "secret": classCategory, "persistentVolumeClaim": classCategory,

	"ephemeral": classOptIn, "csi": classOptIn, "nfs": classOptIn, "iscsi": classOptIn,
	"fc": classOptIn, "rbd": classOptIn, "cephfs": classOptIn, "glusterfs": classOptIn,
	"flexVolume": classOptIn, "gitRepo": classOptIn, "cinder": classOptIn, "flocker": classOptIn,
	"azureFile": classOptIn, "azureDisk": classOptIn, "awsElasticBlockStore": classOptIn,
	"gcePersistentDisk": classOptIn, "vsphereVolume": classOptIn, "quobyte": classOptIn,
	"photonPersistentDisk": classOptIn, "portworxVolume": classOptIn, "scaleIO": classOptIn,
	"storageos": classOptIn,
}

// volumeCategoryKey names the jobPodPolicy key governing each category-class
// volume type, for configuration error messages.
var volumeCategoryKey = map[string]string{
	"hostPath": "allowHostAccess", "secret": "allowSecrets",
	"persistentVolumeClaim": "allowPersistentVolumeClaims",
}

// projectionSourceClass classifies every corev1.VolumeProjection source.
var projectionSourceClass = map[string]fieldClass{
	"configMap": classAllowed, "downwardAPI": classAllowed, "clusterTrustBundle": classAllowed,
	"secret": classCategory, "serviceAccountToken": classCategory, "podCertificate": classCategory,
}

// podSecurityContextFieldClass / securityContextFieldClass classify every
// field of the two security-context types (spec §5.4); value rules live in
// checkPodSecurityContext / checkSecurityContext.
var podSecurityContextFieldClass = map[string]fieldClass{
	"runAsUser": classAllowed, "runAsGroup": classAllowed, "runAsNonRoot": classAllowed,
	"supplementalGroups": classAllowed, "supplementalGroupsPolicy": classAllowed,
	"fsGroup": classAllowed, "fsGroupChangePolicy": classAllowed, "seLinuxChangePolicy": classAllowed,
	"seccompProfile": classCategory, "appArmorProfile": classCategory, "seLinuxOptions": classCategory,
	"sysctls": classCategory, "windowsOptions": classCategory,
}

var securityContextFieldClass = map[string]fieldClass{
	"runAsUser": classAllowed, "runAsGroup": classAllowed, "runAsNonRoot": classAllowed,
	"readOnlyRootFilesystem": classAllowed, "allowPrivilegeEscalation": classAllowed,
	"privileged": classCategory, "capabilities": classCategory, "procMount": classCategory,
	"seLinuxOptions": classCategory, "windowsOptions": classCategory,
	"seccompProfile": classCategory, "appArmorProfile": classCategory,
}

// baselineCapabilities is Pod Security baseline's allowed capabilities.add
// set (spec R8): the container runtime's defaults, so adding one grants nothing.
var baselineCapabilities = map[string]bool{
	"AUDIT_WRITE": true, "CHOWN": true, "DAC_OVERRIDE": true, "FOWNER": true, "FSETID": true,
	"KILL": true, "MKNOD": true, "NET_BIND_SERVICE": true, "SETFCAP": true, "SETGID": true,
	"SETPCAP": true, "SETUID": true, "SYS_CHROOT": true,
}

// baselineSELinuxTypes is Pod Security baseline's allowed seLinuxOptions.type set.
var baselineSELinuxTypes = map[string]bool{
	"": true, "container_t": true, "container_init_t": true, "container_kvm_t": true, "container_engine_t": true,
}

// safeSysctls is Pod Security baseline's safe sysctl set (Kubernetes v1.34).
var safeSysctls = map[string]bool{
	"kernel.shm_rmid_forced": true, "net.ipv4.ip_local_port_range": true,
	"net.ipv4.ip_unprivileged_port_start": true, "net.ipv4.tcp_syncookies": true,
	"net.ipv4.ping_group_range": true, "net.ipv4.ip_local_reserved_ports": true,
	"net.ipv4.tcp_keepalive_time": true, "net.ipv4.tcp_fin_timeout": true,
	"net.ipv4.tcp_keepalive_intvl": true, "net.ipv4.tcp_keepalive_probes": true,
}

// credentialVolumeNames are the agent-injected volumes carrying the artifact
// sidecar's credentials (podbuilder.go). No job container may mount them,
// under any policy (spec R7).
var credentialVolumeNames = map[string]bool{
	sidecarS3CredentialsVolumeName: true,
	sidecarBrokerTokenVolumeName:   true,
}

// PodPolicyViolation is one job-supplied field the policy refused.
type PodPolicyViolation struct {
	Path    string // job-facing path, e.g. podTemplate.spec.containers[1].securityContext.privileged
	Subject string // e.g. `container "docker"`; may be empty
	What    string // what the field does, e.g. "privileged: true"
	Allow   string // jobPodPolicy setting that allows it, e.g. "allowPrivileged: true"; "" if none
}

func (v PodPolicyViolation) String() string {
	s := v.Path
	if v.Subject != "" {
		s += " (" + v.Subject + ")"
	}
	s += ": " + v.What
	if v.Allow == "" {
		return s + " (no k8s-agent setting allows this)"
	}
	return s + " (allow with jobPodPolicy." + v.Allow + " in the k8s-agent config)"
}

// JobPodPolicyError lists every violation in a claim's podTemplate (spec R5).
type JobPodPolicyError struct {
	Violations []PodPolicyViolation
}

func (e *JobPodPolicyError) Error() string {
	parts := make([]string, len(e.Violations))
	for i, v := range e.Violations {
		parts[i] = v.String()
	}
	return fmt.Sprintf("k8s: job pod policy: the job's podTemplate sets %d field(s) this agent does not allow: %s. "+
		"Agent-side podTemplates (podTemplates: in the k8s-agent config) are not subject to this policy; "+
		"see \"Job Pod field policy\" in docs/operator-manual/kubernetes-integration.md",
		len(e.Violations), strings.Join(parts, "; "))
}

// CheckJobPodTemplate returns nil when every job-supplied Pod field in jobTmpl
// is allowed by p, and otherwise a *JobPodPolicyError listing each refused
// field. It inspects exactly what BuildPod consumes from jobTmpl: the inline
// spec when no name is set, the override patch when one is. A decode error
// returns nil so BuildPod reports it with its usual message (spec R4).
func (p *JobPodPolicy) CheckJobPodTemplate(agentTmpls map[string]AgentPodTemplate, jobTmpl *dsl.PodTemplate) error {
	if jobTmpl == nil {
		return nil
	}
	switch {
	case jobTmpl.Name != "":
		return nil // Task 3: override patches against the named template.
	case jobTmpl.Spec != nil:
		spec, err := podSpecFromMap(jobTmpl.Spec)
		if err != nil {
			return nil
		}
		c := &podPolicyChecker{p: p, sa: effectiveServiceAccount(spec)}
		c.checkPodSpec(spec, "podTemplate.spec")
		return c.result()
	}
	return nil
}

// effectiveServiceAccount is the account a Pod built from spec runs as (spec §5.5).
func effectiveServiceAccount(spec *corev1.PodSpec) string {
	if spec.ServiceAccountName != "" {
		return spec.ServiceAccountName
	}
	if spec.DeprecatedServiceAccount != "" {
		return spec.DeprecatedServiceAccount
	}
	return "default"
}

type podPolicyChecker struct {
	p          *JobPodPolicy
	sa         string // effective ServiceAccount of the Pod being checked
	violations []PodPolicyViolation
}

func (c *podPolicyChecker) deny(path, subject, what, allow string) {
	c.violations = append(c.violations, PodPolicyViolation{Path: path, Subject: subject, What: what, Allow: allow})
}

func (c *podPolicyChecker) result() error {
	if len(c.violations) == 0 {
		return nil
	}
	return &JobPodPolicyError{Violations: c.violations}
}

// jsonKeys returns the sorted top-level keys of v's JSON encoding: the fields
// a typed Kubernetes value carries to the API server (omitempty drops unset ones).
func jsonKeys(v any) ([]string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

// checkKeys applies a classification table to v's set fields. Category fields
// are left to the typed value checks; an inspection failure is a violation,
// never a pass.
func (c *podPolicyChecker) checkKeys(path, subject string, v any, table map[string]fieldClass, optIn []string, optInKey string) {
	keys, err := jsonKeys(v)
	if err != nil {
		c.deny(path, subject, "could not be inspected: "+err.Error(), "")
		return
	}
	for _, k := range keys {
		cls, ok := table[k]
		switch {
		case !ok:
			c.deny(path+"."+k, subject, "is not classified by this agent's job pod policy", "")
		case cls == classNever:
			c.deny(path+"."+k, subject, "cannot be set when a Pod is created", "")
		case cls == classOptIn && !slices.Contains(optIn, k):
			allow := ""
			if optInKey != "" {
				allow = fmt.Sprintf("%s: [%s]", optInKey, k)
			}
			c.deny(path+"."+k, subject, "is not on the allowed list", allow)
		}
	}
}

func (c *podPolicyChecker) checkPodSpec(spec *corev1.PodSpec, path string) {
	p := c.p
	c.checkKeys(path, "", spec, podSpecFieldClass, p.AllowPodSpecFields, "allowPodSpecFields")

	for _, f := range []struct{ field, name string }{
		{"serviceAccountName", spec.ServiceAccountName},
		{"serviceAccount", spec.DeprecatedServiceAccount},
	} {
		if f.name != "" && !slices.Contains(p.AllowServiceAccounts, f.name) {
			c.deny(path+"."+f.field, "", fmt.Sprintf("selects ServiceAccount %q", f.name), fmt.Sprintf("allowServiceAccounts: [%q]", f.name))
		}
	}
	if t := spec.AutomountServiceAccountToken; t != nil && *t && !slices.Contains(p.AllowServiceAccounts, c.sa) {
		c.deny(path+".automountServiceAccountToken", "", fmt.Sprintf("true mounts the token of ServiceAccount %q", c.sa), fmt.Sprintf("allowServiceAccounts: [%q]", c.sa))
	}
	if spec.NodeName != "" && !p.AllowNodeName {
		c.deny(path+".nodeName", "", fmt.Sprintf("pins the Pod to node %q, bypassing the scheduler", spec.NodeName), "allowNodeName: true")
	}
	if !p.AllowHostAccess {
		if spec.HostNetwork {
			c.deny(path+".hostNetwork", "", "true shares the node's network namespace", "allowHostAccess: true")
		}
		if spec.HostPID {
			c.deny(path+".hostPID", "", "true shares the node's process namespace", "allowHostAccess: true")
		}
		if spec.HostIPC {
			c.deny(path+".hostIPC", "", "true shares the node's IPC namespace", "allowHostAccess: true")
		}
	}
	for i, s := range spec.ImagePullSecrets {
		if !slices.Contains(p.AllowImagePullSecrets, s.Name) {
			c.deny(fmt.Sprintf("%s.imagePullSecrets[%d]", path, i), "", fmt.Sprintf("pulls images with Secret %q", s.Name), fmt.Sprintf("allowImagePullSecrets: [%q]", s.Name))
		}
	}
	c.checkPodSecurityContext(spec.SecurityContext, path+".securityContext")
	for i := range spec.InitContainers {
		c.checkContainer(&spec.InitContainers[i], fmt.Sprintf("%s.initContainers[%d]", path, i), nil)
	}
	for i := range spec.Containers {
		c.checkContainer(&spec.Containers[i], fmt.Sprintf("%s.containers[%d]", path, i), nil)
	}
	for i := range spec.Volumes {
		c.checkVolume(&spec.Volumes[i], fmt.Sprintf("%s.volumes[%d]", path, i))
	}
}

// checkContainer applies spec §5.2. templateVolume resolves a mount name to an
// agent template volume (override containers only; nil for an inline spec).
func (c *podPolicyChecker) checkContainer(ct *corev1.Container, path string, templateVolume func(name string) *corev1.Volume) {
	p := c.p
	subj := fmt.Sprintf("container %q", ct.Name)
	c.checkKeys(path, subj, ct, containerFieldClass, nil, "")

	if !p.AllowHostAccess {
		for i, port := range ct.Ports {
			if port.HostPort != 0 || port.HostIP != "" {
				c.deny(fmt.Sprintf("%s.ports[%d]", path, i), subj, "binds a port on the node (hostPort/hostIP)", "allowHostAccess: true")
			}
		}
	}
	for i, e := range ct.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			c.secret(fmt.Sprintf("%s.env[%d].valueFrom.secretKeyRef", path, i), subj, e.ValueFrom.SecretKeyRef.Name, "")
		}
	}
	for i, e := range ct.EnvFrom {
		if e.SecretRef != nil {
			c.secret(fmt.Sprintf("%s.envFrom[%d].secretRef", path, i), subj, e.SecretRef.Name, "")
		}
	}
	for _, pr := range []struct {
		field string
		probe *corev1.Probe
	}{{"livenessProbe", ct.LivenessProbe}, {"readinessProbe", ct.ReadinessProbe}, {"startupProbe", ct.StartupProbe}} {
		if pr.probe != nil {
			c.handlerHost(path+"."+pr.field, subj, pr.probe.HTTPGet, pr.probe.TCPSocket)
		}
	}
	if ct.Lifecycle != nil {
		if h := ct.Lifecycle.PostStart; h != nil {
			c.handlerHost(path+".lifecycle.postStart", subj, h.HTTPGet, h.TCPSocket)
		}
		if h := ct.Lifecycle.PreStop; h != nil {
			c.handlerHost(path+".lifecycle.preStop", subj, h.HTTPGet, h.TCPSocket)
		}
	}
	c.checkSecurityContext(ct.SecurityContext, path+".securityContext", subj)
	for i, m := range ct.VolumeMounts {
		c.mount(fmt.Sprintf("%s.volumeMounts[%d]", path, i), subj, m.Name, templateVolume)
	}
	for i, d := range ct.VolumeDevices {
		c.mount(fmt.Sprintf("%s.volumeDevices[%d]", path, i), subj, d.Name, templateVolume)
	}
}

func (c *podPolicyChecker) handlerHost(path, subj string, h *corev1.HTTPGetAction, t *corev1.TCPSocketAction) {
	if c.p.AllowHostAccess {
		return
	}
	if h != nil && h.Host != "" {
		c.deny(path+".httpGet.host", subj, fmt.Sprintf("makes the kubelet connect to %q from the node", h.Host), "allowHostAccess: true")
	}
	if t != nil && t.Host != "" {
		c.deny(path+".tcpSocket.host", subj, fmt.Sprintf("makes the kubelet connect to %q from the node", t.Host), "allowHostAccess: true")
	}
}

func (c *podPolicyChecker) secret(path, subj, name, prefix string) {
	if !slices.Contains(c.p.AllowSecrets, name) {
		c.deny(path, subj, prefix+fmt.Sprintf("reads Secret %q", name), fmt.Sprintf("allowSecrets: [%q]", name))
	}
}

func (c *podPolicyChecker) mount(path, subj, name string, templateVolume func(string) *corev1.Volume) {
	if credentialVolumeNames[name] {
		c.deny(path, subj, fmt.Sprintf("mounts %q, the artifact sidecar's credential volume", name), "")
		return
	}
	if templateVolume == nil {
		return
	}
	if tv := templateVolume(name); tv != nil {
		c.checkVolumeSource(&tv.VolumeSource, subj, func(string) string { return path },
			fmt.Sprintf("mounts agent template volume %q, which is ", name))
	}
}

func (c *podPolicyChecker) checkVolume(v *corev1.Volume, path string) {
	c.checkVolumeSource(&v.VolumeSource, fmt.Sprintf("volume %q", v.Name),
		func(k string) string { return path + "." + k }, "")
}

// checkVolumeSource applies spec §5.3. at maps a source key to the violation
// path; prefix phrases a template volume reached through a mount.
func (c *podPolicyChecker) checkVolumeSource(src *corev1.VolumeSource, subj string, at func(key string) string, prefix string) {
	p := c.p
	keys, err := jsonKeys(src)
	if err != nil {
		c.deny(at(""), subj, prefix+"a volume that could not be inspected: "+err.Error(), "")
		return
	}
	for _, k := range keys {
		cls, ok := volumeSourceClass[k]
		switch {
		case !ok:
			c.deny(at(k), subj, prefix+fmt.Sprintf("a %s volume, a type this agent's job pod policy does not classify", k), "")
		case cls == classOptIn && !slices.Contains(p.AllowVolumeTypes, k):
			c.deny(at(k), subj, prefix+fmt.Sprintf("a %s volume", k), fmt.Sprintf("allowVolumeTypes: [%s]", k))
		}
	}
	if src.HostPath != nil && !p.AllowHostAccess {
		c.deny(at("hostPath"), subj, prefix+fmt.Sprintf("a hostPath volume (%q on the node)", src.HostPath.Path), "allowHostAccess: true")
	}
	if src.Secret != nil {
		c.secret(at("secret"), subj, src.Secret.SecretName, prefix+"a secret volume that ")
	}
	if pvc := src.PersistentVolumeClaim; pvc != nil && !slices.Contains(p.AllowPersistentVolumeClaims, pvc.ClaimName) {
		c.deny(at("persistentVolumeClaim"), subj, prefix+fmt.Sprintf("a volume of PersistentVolumeClaim %q", pvc.ClaimName), fmt.Sprintf("allowPersistentVolumeClaims: [%q]", pvc.ClaimName))
	}
	if src.Projected != nil {
		for i := range src.Projected.Sources {
			s := &src.Projected.Sources[i]
			sp := fmt.Sprintf("%s.sources[%d]", at("projected"), i)
			c.checkKeys(sp, subj, s, projectionSourceClass, nil, "")
			if s.Secret != nil {
				c.secret(sp+".secret", subj, s.Secret.Name, prefix+"a projected volume that ")
			}
			if !slices.Contains(p.AllowServiceAccounts, c.sa) {
				if s.ServiceAccountToken != nil {
					c.deny(sp+".serviceAccountToken", subj, prefix+fmt.Sprintf("a projected token of ServiceAccount %q", c.sa), fmt.Sprintf("allowServiceAccounts: [%q]", c.sa))
				}
				if s.PodCertificate != nil {
					c.deny(sp+".podCertificate", subj, prefix+fmt.Sprintf("a projected certificate for ServiceAccount %q", c.sa), fmt.Sprintf("allowServiceAccounts: [%q]", c.sa))
				}
			}
		}
	}
}

func (c *podPolicyChecker) checkPodSecurityContext(sc *corev1.PodSecurityContext, path string) {
	if sc == nil {
		return
	}
	c.checkKeys(path, "", sc, podSecurityContextFieldClass, nil, "")
	c.profiles(path, "", sc.SeccompProfile, sc.AppArmorProfile)
	c.seLinux(path, "", sc.SELinuxOptions)
	c.hostProcess(path, "", sc.WindowsOptions)
	if !c.p.AllowPrivileged {
		for i, s := range sc.Sysctls {
			if !safeSysctls[s.Name] {
				c.deny(fmt.Sprintf("%s.sysctls[%d]", path, i), "", fmt.Sprintf("sets the unsafe sysctl %q", s.Name), "allowPrivileged: true")
			}
		}
	}
}

func (c *podPolicyChecker) checkSecurityContext(sc *corev1.SecurityContext, path, subj string) {
	if sc == nil {
		return
	}
	c.checkKeys(path, subj, sc, securityContextFieldClass, nil, "")
	c.profiles(path, subj, sc.SeccompProfile, sc.AppArmorProfile)
	c.seLinux(path, subj, sc.SELinuxOptions)
	c.hostProcess(path, subj, sc.WindowsOptions)
	if c.p.AllowPrivileged {
		return
	}
	if sc.Privileged != nil && *sc.Privileged {
		c.deny(path+".privileged", subj, "privileged: true", "allowPrivileged: true")
	}
	if sc.Capabilities != nil {
		for i, cp := range sc.Capabilities.Add {
			if !baselineCapabilities[normalizeCapability(string(cp))] {
				c.deny(fmt.Sprintf("%s.capabilities.add[%d]", path, i), subj, fmt.Sprintf("adds capability %q", cp), "allowPrivileged: true")
			}
		}
	}
	if sc.ProcMount != nil && *sc.ProcMount != corev1.DefaultProcMount {
		c.deny(path+".procMount", subj, fmt.Sprintf("procMount: %s", *sc.ProcMount), "allowPrivileged: true")
	}
}

// normalizeCapability upper-cases a capability and strips any CAP_ prefix,
// so "cap_chown" and "CHOWN" compare equal.
func normalizeCapability(s string) string {
	return strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(s)), "CAP_")
}

func (c *podPolicyChecker) profiles(path, subj string, seccomp *corev1.SeccompProfile, apparmor *corev1.AppArmorProfile) {
	if c.p.AllowUnconfinedProfiles {
		return
	}
	if seccomp != nil && seccomp.Type == corev1.SeccompProfileTypeUnconfined {
		c.deny(path+".seccompProfile.type", subj, "seccompProfile.type: Unconfined", "allowUnconfinedProfiles: true")
	}
	if apparmor != nil && apparmor.Type == corev1.AppArmorProfileTypeUnconfined {
		c.deny(path+".appArmorProfile.type", subj, "appArmorProfile.type: Unconfined", "allowUnconfinedProfiles: true")
	}
}

func (c *podPolicyChecker) seLinux(path, subj string, o *corev1.SELinuxOptions) {
	if o == nil || c.p.AllowPrivileged {
		return
	}
	if o.User != "" || o.Role != "" || !baselineSELinuxTypes[o.Type] {
		c.deny(path+".seLinuxOptions", subj, fmt.Sprintf("SELinux options outside the baseline set (user %q, role %q, type %q)", o.User, o.Role, o.Type), "allowPrivileged: true")
	}
}

func (c *podPolicyChecker) hostProcess(path, subj string, w *corev1.WindowsSecurityContextOptions) {
	if w == nil || w.HostProcess == nil || !*w.HostProcess || c.p.AllowPrivileged {
		return
	}
	c.deny(path+".windowsOptions.hostProcess", subj, "hostProcess: true runs the container as a process on the node", "allowPrivileged: true")
}
