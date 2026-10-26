# Kubernetes job Pod field policy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** By default the k8s agent refuses, at claim time, a job whose own `podTemplate` content reaches a ServiceAccount, the node, a Kubernetes Secret, a PVC, or an unclassified field. Each category is restored by an explicit `jobPodPolicy` opt-in in the agent config, and the shipped `ci` namespace enforces Pod Security `baseline`.

**Architecture:** A new `internal/k8sagent/podpolicy.go` decodes job-supplied Pod content with the same functions `BuildPod` uses and checks it against classification tables (allowed / category / opt-in / never). It returns a typed `*JobPodPolicyError` listing every violation. `K8sAgent.executeRun` calls it right after the `native` check, before the pool, volume locks and `BuildPod`. Agent templates are trusted and not checked. `BuildPod` is unchanged.

**Tech Stack:** Go, `k8s.io/api` v0.36.1 (`corev1`), `k8s.io/apimachinery/pkg/util/validation`, `gopkg.in/yaml.v3`, testify, kind (`-tags k8s`). No new dependencies.

**Spec:** `docs/superpowers/specs/2026-10-26-k8s-job-pod-field-policy-design.md`. Read it first. Its rulings (R1–R12) and tables (§5) are the contract; this plan argues from them and does not repeat the reasoning.

## Global Constraints

- Everything in this repository is written in English: code, comments, commit messages, docs, error strings.
- No personally identifying information. Use `/path/to/unified-cd` and `your-username` as placeholders.
- `BuildPod`'s signature does not change (45 call sites). The policy is a separate function called from `executeRun` (R1).
- Agent templates (`Config.PodTemplates`) are never checked (R3). Only `podTemplate.spec` (when no `name`) and `podTemplate.override` are checked, plus template volumes that an override container mounts (§5.6).
- The policy reads the **decoded typed** form (R4). If decoding fails, return `nil` and let `BuildPod` produce its existing error.
- No wildcards in name lists, no environment variables for `jobPodPolicy` (R6, R9).
- The tables in `podpolicy.go` must classify **every** JSON field of the covered `corev1` types. The completeness test enforces this; never silence it by deleting a struct from the test.
- The error prefix is exactly `k8s: job pod policy:`. Docs and troubleshooting entries grep for it.
- `docs/reference/field-reference.md` and `schemas/unified-cd.schema.json` are generated and this change touches no DSL type, so neither should change. If `go generate ./...` produces a diff, stop and find out why.
- Do not modify `internal/dsl/podtemplate.go` or `HostSupportedContainerFields`. An unmerged branch (host `securityContext` support) edits them (spec §9).
- Verification commands for every task: `go build ./...`, `go vet ./internal/k8sagent/...`, `go test ./internal/k8sagent/... -count=1`. Tasks also list their own focused `-run` command.

## Review Focus

Failure modes a happy path would not exercise:

1. **Pool reuse skips `BuildPod`.** If the check sits anywhere but before `a.pool.ClaimPod`, an idle Pod built before the upgrade is handed to a denied job. — Task 5.
2. **Override containers mounting template volumes.** Checking only the patch's own volumes lets a job add a container that mounts the operator's `hostPath`/`secret` volume. — Task 3.
3. **Credential volumes (R7).** `sidecar-s3-credentials` / `sidecar-store-credential-token` must be refused even under the most permissive policy. — Tasks 1 and 2.
4. **Fail-open on inspection errors.** `jsonKeys` failing must produce a violation, never a pass. — Task 1.
5. **Effective ServiceAccount for overrides** is the template's, not `default`. — Task 3.
6. **`omitempty` assumptions.** `hostNetwork: false` disappears on re-encode (allowed). `automountServiceAccountToken: false` does not (it is a pointer) and must be allowed by value. — Task 2.
7. **Docs made false.** The threat-model sentence "the job container never sees them", the interim "What a job can put in the Pod" section, the authorization paragraph from 913ea9a and its anchor. — Task 7.

---

### Task 1: Policy engine, tables and completeness test

**Files:**
- Create: `internal/k8sagent/podpolicy.go`
- Create: `internal/k8sagent/podpolicy_test.go`

**Interfaces produced (later tasks depend on these exact names):**
- `type JobPodPolicy struct` with yaml tags (Task 4 embeds it in `Config`).
- `func (p *JobPodPolicy) CheckJobPodTemplate(agentTmpls map[string]AgentPodTemplate, jobTmpl *dsl.PodTemplate) error`. In this task it handles the inline branch. Task 3 fills in the named-template branch.
- `type PodPolicyViolation struct{ Path, Subject, What, Allow string }`, `type JobPodPolicyError struct{ Violations []PodPolicyViolation }`.
- Tables `podSpecFieldClass`, `containerFieldClass`, `volumeSourceClass`, `projectionSourceClass`, `podSecurityContextFieldClass`, `securityContextFieldClass`, plus `podSpecCategoryKey` and `volumeCategoryKey` (used by Task 4's validation messages).

- [ ] **Step 1: Write the completeness test first** (`podpolicy_test.go`):

```go
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
```

- [ ] **Step 2: Run it and watch it fail to compile** (`go test ./internal/k8sagent/ -run TestJobPodPolicy_TablesClassifyEveryField`): the tables do not exist yet.

- [ ] **Step 3: Write `podpolicy.go`.** Use the complete code below. The tables follow spec §5 exactly. If you believe a classification is wrong, stop and raise it; do not "fix" the spec silently.

```go
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
	"workloadRef": classOptIn, "schedulingGroup": classOptIn,

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
```

- [ ] **Step 4: Add the error-shape and fail-closed tests** to `podpolicy_test.go`:

```go
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
```

- [ ] **Step 5: Run** `go test ./internal/k8sagent/ -run 'TestJobPodPolicy_' -count=1 -v`. Everything passes. If the completeness test reports a field mismatch, the vendored `k8s.io/api` differs from v0.36.1: classify the reported field per spec §5's reasoning and report it in your summary.

- [ ] **Step 6: Commit** — `feat(k8sagent): job pod field policy engine and classification tables`.

---

### Task 2: Exhaustive inline deny/allow tests

**Files:**
- Create: `internal/k8sagent/podpolicy_inline_test.go`

**Depends on:** Task 1.

The purpose is to show, for each deny rule in spec §5.1–§5.4, that the default policy denies it with the expected path and `Allow`, and that the matching opt-in restores it.

- [ ] **Step 1: Write the table test.** Every row below must be present. Add rows if you find a deny branch in `podpolicy.go` not covered; the review will compare the table with the code's `c.deny` calls.

```go
package k8sagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type policyCase struct {
	name      string
	container map[string]any // merged into the "job" container
	pod       map[string]any // merged into the PodSpec
	path      string         // expected single violation path
	allow     string         // expected Allow ("" = nothing allows it)
	fix       JobPodPolicy   // a policy under which the case passes (zero when allow == "")
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
	{name: "pod unsafe sysctl", pod: map[string]any{"securityContext": map[string]any{"sysctls": []any{map[string]any{"name": "kernel.msgmax", "value": "1"}}}},
		path: "podTemplate.spec.securityContext.sysctls[0]", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	{name: "pod seLinux role", pod: map[string]any{"securityContext": map[string]any{"seLinuxOptions": map[string]any{"role": "sysadm_r"}}},
		path: "podTemplate.spec.securityContext.seLinuxOptions", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	{name: "pod hostProcess", pod: map[string]any{"securityContext": map[string]any{"windowsOptions": map[string]any{"hostProcess": true}}},
		path: "podTemplate.spec.securityContext.windowsOptions.hostProcess", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},

	// §5.2 / §5.4 container
	{name: "privileged", container: map[string]any{"securityContext": map[string]any{"privileged": true}},
		path: "podTemplate.spec.containers[0].securityContext.privileged", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	{name: "capability SYS_ADMIN", container: map[string]any{"securityContext": map[string]any{"capabilities": map[string]any{"add": []any{"SYS_ADMIN"}}}},
		path: "podTemplate.spec.containers[0].securityContext.capabilities.add[0]", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	{name: "capability cap_net_admin", container: map[string]any{"securityContext": map[string]any{"capabilities": map[string]any{"add": []any{"cap_net_admin"}}}},
		path: "podTemplate.spec.containers[0].securityContext.capabilities.add[0]", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	{name: "procMount Unmasked", container: map[string]any{"securityContext": map[string]any{"procMount": "Unmasked"}},
		path: "podTemplate.spec.containers[0].securityContext.procMount", allow: "allowPrivileged: true", fix: JobPodPolicy{AllowPrivileged: true}},
	{name: "container apparmor Unconfined", container: map[string]any{"securityContext": map[string]any{"appArmorProfile": map[string]any{"type": "Unconfined"}}},
		path: "podTemplate.spec.containers[0].securityContext.appArmorProfile.type", allow: "allowUnconfinedProfiles: true",
		fix: JobPodPolicy{AllowUnconfinedProfiles: true}},
	{name: "hostPort", container: map[string]any{"ports": []any{map[string]any{"containerPort": 80, "hostPort": 8080}}},
		path: "podTemplate.spec.containers[0].ports[0]", allow: "allowHostAccess: true", fix: JobPodPolicy{AllowHostAccess: true}},
	{name: "probe httpGet host", container: map[string]any{"livenessProbe": map[string]any{"httpGet": map[string]any{"host": "169.254.169.254", "port": 80}}},
		path: "podTemplate.spec.containers[0].livenessProbe.httpGet.host", allow: "allowHostAccess: true", fix: JobPodPolicy{AllowHostAccess: true}},
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
	{name: "projected secret", pod: map[string]any{"volumes": []any{map[string]any{"name": "pr", "projected": map[string]any{"sources": []any{map[string]any{"secret": map[string]any{"name": "db"}}}}}}},
		path: "podTemplate.spec.volumes[0].projected.sources[0].secret", allow: `allowSecrets: ["db"]`,
		fix: JobPodPolicy{AllowSecrets: []string{"db"}}},
	{name: "projected SA token", pod: map[string]any{"volumes": []any{map[string]any{"name": "pr", "projected": map[string]any{"sources": []any{map[string]any{"serviceAccountToken": map[string]any{"path": "t"}}}}}}},
		path: "podTemplate.spec.volumes[0].projected.sources[0].serviceAccountToken", allow: `allowServiceAccounts: ["default"]`,
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
// the most permissive policy expressible.
func TestJobPodPolicy_R7NothingAllows(t *testing.T) {
	all := JobPodPolicy{AllowHostAccess: true, AllowPrivileged: true, AllowUnconfinedProfiles: true, AllowNodeName: true,
		AllowSecrets: []string{"x"}, AllowServiceAccounts: []string{"default"}}
	for _, name := range []string{sidecarS3CredentialsVolumeName, sidecarBrokerTokenVolumeName} {
		tmpl := inlineJob(map[string]any{"volumeMounts": []any{map[string]any{"name": name, "mountPath": "/x"}}}, nil)
		pe := policyErr(t, all, nil, tmpl)
		require.NotNil(t, pe, name)
		assert.Equal(t, "", pe.Violations[0].Allow)
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

// TestJobPodPolicy_BaselineBoundaries pins spec R8.
func TestJobPodPolicy_BaselineBoundaries(t *testing.T) {
	ok := []map[string]any{
		{"securityContext": map[string]any{"allowPrivilegeEscalation": true}},
		{"securityContext": map[string]any{"runAsUser": 0}},
		{"securityContext": map[string]any{"capabilities": map[string]any{"add": []any{"NET_BIND_SERVICE", "CAP_CHOWN"}}}},
		{"securityContext": map[string]any{"seccompProfile": map[string]any{"type": "RuntimeDefault"}}},
		{"securityContext": map[string]any{"privileged": false}},
	}
	for _, c := range ok {
		assert.NoError(t, (&JobPodPolicy{}).CheckJobPodTemplate(nil, inlineJob(c, nil)), "%v", c)
	}
	assert.NoError(t, (&JobPodPolicy{}).CheckJobPodTemplate(nil, inlineJob(nil, map[string]any{
		"automountServiceAccountToken": false, "hostNetwork": false,
		"securityContext": map[string]any{"sysctls": []any{map[string]any{"name": "net.ipv4.tcp_syncookies", "value": "1"}}},
	})))
}
```

- [ ] **Step 2: Run** `go test ./internal/k8sagent/ -run 'TestJobPodPolicy_' -count=1 -v`. If a row fails because of a bug in Task 1's code, fix the code (not the expectation) and say so in your report.

- [ ] **Step 3: Commit** — `test(k8sagent): cover every inline job pod policy rule and its opt-in`.

---

### Task 3: Override patches and template volumes

**Files:**
- Modify: `internal/k8sagent/podpolicy.go` (the `jobTmpl.Name != ""` branch of `CheckJobPodTemplate`, plus a new `checkOverride`)
- Create: `internal/k8sagent/podpolicy_override_test.go`

**Depends on:** Task 1 (and Task 2's `inlinePolicyCases`, which this task reuses for container-level rows).

- [ ] **Step 1: Write failing tests** (`podpolicy_override_test.go`):

```go
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
```

- [ ] **Step 2: Run them** (`go test ./internal/k8sagent/ -run 'TestJobPodPolicy_(Named|Override)' -count=1`). They fail, because the named branch returns nil.

- [ ] **Step 3: Implement.** Replace the `case jobTmpl.Name != "":` branch of `CheckJobPodTemplate` and add `checkOverride`:

```go
	case jobTmpl.Name != "":
		if jobTmpl.Override == nil {
			return nil // an unpatched operator template is trusted (spec R3)
		}
		at, ok := agentTmpls[jobTmpl.Name]
		if !ok {
			return nil // BuildPod reports the missing template
		}
		base, err := podSpecFromMap(at.Spec)
		if err != nil {
			return nil // BuildPod reports it
		}
		return p.checkOverride(base, jobTmpl.Override)
```

```go
// checkOverride applies spec §5.6: patch volumes and containers are job
// content; a patch container's mount of a template volume the patch does not
// replace is checked as if the job had declared that volume.
func (p *JobPodPolicy) checkOverride(base *corev1.PodSpec, patch *dsl.PodSpecPatch) error {
	containers, err := containersFromMaps(patch.Containers)
	if err != nil {
		return nil
	}
	volumes, err := volumesFromMaps(patch.Volumes)
	if err != nil {
		return nil
	}
	c := &podPolicyChecker{p: p, sa: effectiveServiceAccount(base)}
	replaced := map[string]bool{}
	for _, v := range volumes {
		replaced[v.Name] = true
	}
	tmplVols := map[string]*corev1.Volume{}
	for i := range base.Volumes {
		if !replaced[base.Volumes[i].Name] {
			tmplVols[base.Volumes[i].Name] = &base.Volumes[i]
		}
	}
	lookup := func(name string) *corev1.Volume { return tmplVols[name] }
	for i := range volumes {
		c.checkVolume(&volumes[i], fmt.Sprintf("podTemplate.override.volumes[%d]", i))
	}
	for i := range containers {
		c.checkContainer(&containers[i], fmt.Sprintf("podTemplate.override.containers[%d]", i), lookup)
	}
	return c.result()
}
```

- [ ] **Step 4: Run** `go test ./internal/k8sagent/ -run 'TestJobPodPolicy_' -count=1 -v`. Everything passes.

- [ ] **Step 5: Commit** — `feat(k8sagent): apply the job pod policy to override patches and the template volumes they mount`.

---

### Task 4: `jobPodPolicy` in the agent config

**Files:**
- Modify: `internal/k8sagent/config.go` (field on `Config`; strict decode in `LoadConfig`; call in `Validate`)
- Modify: `internal/k8sagent/podpolicy.go` (add `validate` and `Summary`)
- Modify: `cmd/k8s-agent/main.go` (startup log line)
- Test: `internal/k8sagent/podpolicy_config_test.go`

**Depends on:** Task 1.

- [ ] **Step 1: Write failing tests** (`podpolicy_config_test.go`). Base every case on a minimal valid config: `Config{Server: "https://c.example", EnrollmentPolicy: "p"}`. Cases:

| Input | Expect |
|---|---|
| zero `JobPodPolicy` | `Validate()` ok; `Summary()` == `"none"` |
| `AllowSecrets: ["*"]` | error contains `jobPodPolicy: allowSecrets: "*": wildcards are not supported; list each name` |
| `AllowServiceAccounts: ["Bad_Name"]` | error contains `is not a valid Kubernetes object name` |
| `SidecarS3SecretName: "ucd-s3"`, `AllowSecrets: ["ucd-s3"]` | error contains `is sidecarS3SecretName` |
| `AgentName: "ci-main"`, `Volumes: [{Name: "go-cache", Mode: "exclusive", Size: "1Gi"}]`, `AllowPersistentVolumeClaims: ["ci-main-go-cache"]` | error contains `is this agent's named volume "go-cache"; jobs reach it only through spec.volumes` |
| `AllowPodSpecFields: ["runtimeClassName"]` | ok |
| `AllowPodSpecFields: ["containers"]` | error contains `"containers" is always allowed; remove it` |
| `AllowPodSpecFields: ["hostNetwork"]` | error contains `"hostNetwork" is governed by jobPodPolicy.allowHostAccess; use that instead` |
| `AllowPodSpecFields: ["ephemeralContainers"]` | error contains `cannot be allowed` |
| `AllowPodSpecFields: ["bogus"]` | error contains `is not an opt-in PodSpec field` and lists `runtimeClassName` |
| `AllowVolumeTypes: ["nfs"]` | ok |
| `AllowVolumeTypes: ["secret"]` | error contains `governed by jobPodPolicy.allowSecrets` |
| `AllowVolumeTypes: ["emptyDir"]` | error contains `is always allowed` |
| `LoadConfig` of a file with `jobPodPolicy:\n  allowHostAcess: true` | error contains `jobPodPolicy` and `allowHostAcess` |
| `LoadConfig` of a file with `jobPodPolicy:\n  allowHostAccess: true\n  allowSecrets: [db]` | ok; fields decoded |
| `Summary()` with `AllowHostAccess` and `AllowSecrets: [a, b]` | `"allowSecrets=a,b allowHostAccess"` (name lists first, in struct order, then booleans, then escape lists) |

Use `writeTempYAML` from `config_test.go` for the `LoadConfig` cases. The file also needs `server:` and `enrollmentPolicy:` only if the test calls `Validate`; `LoadConfig` alone does not require them.

- [ ] **Step 2: Run them and watch them fail** (`go test ./internal/k8sagent/ -run 'TestJobPodPolicyConfig' -count=1`).

- [ ] **Step 3: Implement.**

`config.go`: add the field after `Volumes`:

```go
	// JobPodPolicy lists the Pod fields a job's own podTemplate content may
	// use beyond the always-allowed set; the zero value allows none of the
	// gated categories. File-only (no environment variable). See podpolicy.go.
	JobPodPolicy JobPodPolicy `yaml:"jobPodPolicy,omitempty"`
```

In `LoadConfig`, after `rejectRemovedWorkspacePVC`:

```go
	if err := rejectUnknownJobPodPolicyKeys(configPath); err != nil {
		return cfg, err
	}
```

```go
// rejectUnknownJobPodPolicyKeys strictly decodes the jobPodPolicy block.
// loadYAML is lenient, and a misspelled opt-in would otherwise vanish silently.
func rejectUnknownJobPodPolicyKeys(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var raw struct {
		JobPodPolicy yaml.Node `yaml:"jobPodPolicy"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if raw.JobPodPolicy.Kind == 0 {
		return nil
	}
	block, err := yaml.Marshal(&raw.JobPodPolicy)
	if err != nil {
		return fmt.Errorf("%s: jobPodPolicy: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(block))
	dec.KnownFields(true)
	var p JobPodPolicy
	if err := dec.Decode(&p); err != nil {
		return fmt.Errorf("%s: jobPodPolicy: %w", path, err)
	}
	return nil
}
```

(add `"bytes"` to the imports). In `Validate`, after `validateVolumeConfigs`:

```go
	if err := c.JobPodPolicy.validate(c.SidecarS3SecretName, c.AgentName, c.Volumes); err != nil {
		return fmt.Errorf("jobPodPolicy: %w", err)
	}
```

`podpolicy.go` (add `"k8s.io/apimachinery/pkg/util/validation"` and `"github.com/eirueimi/unified-cd/internal/config"` to the imports):

```go
// validate applies spec §4's startup rules.
func (p *JobPodPolicy) validate(sidecarSecret, agentName string, vols []config.VolumeConfig) error {
	for _, l := range []struct {
		key   string
		names []string
	}{
		{"allowServiceAccounts", p.AllowServiceAccounts}, {"allowSecrets", p.AllowSecrets},
		{"allowImagePullSecrets", p.AllowImagePullSecrets}, {"allowPersistentVolumeClaims", p.AllowPersistentVolumeClaims},
	} {
		for _, n := range l.names {
			if strings.Contains(n, "*") {
				return fmt.Errorf("%s: %q: wildcards are not supported; list each name", l.key, n)
			}
			if errs := validation.IsDNS1123Subdomain(n); len(errs) > 0 {
				return fmt.Errorf("%s: %q is not a valid Kubernetes object name: %s", l.key, n, strings.Join(errs, "; "))
			}
		}
	}
	if sidecarSecret != "" && slices.Contains(p.AllowSecrets, sidecarSecret) {
		return fmt.Errorf("allowSecrets: %q is sidecarS3SecretName, the artifact sidecar's credentials, which job containers must never read", sidecarSecret)
	}
	for _, v := range vols {
		if claim := claimNameFor(agentName, v.Name); slices.Contains(p.AllowPersistentVolumeClaims, claim) {
			return fmt.Errorf("allowPersistentVolumeClaims: %q is this agent's named volume %q; jobs reach it only through spec.volumes, which holds its lock", claim, v.Name)
		}
	}
	if err := validateEscapeList("allowPodSpecFields", "PodSpec field", p.AllowPodSpecFields, podSpecFieldClass, podSpecCategoryKey); err != nil {
		return err
	}
	return validateEscapeList("allowVolumeTypes", "volume type", p.AllowVolumeTypes, volumeSourceClass, volumeCategoryKey)
}

func validateEscapeList(key, kind string, names []string, table map[string]fieldClass, categoryKey map[string]string) error {
	var optIn []string
	for k, cls := range table {
		if cls == classOptIn {
			optIn = append(optIn, k)
		}
	}
	sort.Strings(optIn)
	for _, n := range names {
		cls, ok := table[n]
		switch {
		case !ok:
			return fmt.Errorf("%s: %q is not an opt-in %s; valid entries: %s", key, n, kind, strings.Join(optIn, ", "))
		case cls == classAllowed:
			return fmt.Errorf("%s: %q is always allowed; remove it", key, n)
		case cls == classCategory:
			return fmt.Errorf("%s: %q is governed by jobPodPolicy.%s; use that instead", key, n, categoryKey[n])
		case cls == classNever:
			return fmt.Errorf("%s: %q cannot be allowed: it cannot be set when a Pod is created", key, n)
		}
	}
	return nil
}

// Summary renders the effective opt-ins for the agent's startup log line.
func (p JobPodPolicy) Summary() string {
	var parts []string
	list := func(k string, v []string) {
		if len(v) > 0 {
			parts = append(parts, k+"="+strings.Join(v, ","))
		}
	}
	flag := func(k string, on bool) {
		if on {
			parts = append(parts, k)
		}
	}
	list("allowServiceAccounts", p.AllowServiceAccounts)
	list("allowSecrets", p.AllowSecrets)
	list("allowImagePullSecrets", p.AllowImagePullSecrets)
	list("allowPersistentVolumeClaims", p.AllowPersistentVolumeClaims)
	flag("allowHostAccess", p.AllowHostAccess)
	flag("allowPrivileged", p.AllowPrivileged)
	flag("allowUnconfinedProfiles", p.AllowUnconfinedProfiles)
	flag("allowNodeName", p.AllowNodeName)
	list("allowPodSpecFields", p.AllowPodSpecFields)
	list("allowVolumeTypes", p.AllowVolumeTypes)
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}
```

`categoryKey[n]` for a category volume type `projected`? `projected` is `classAllowed`, so it never reaches that branch. Every category entry in `podSpecFieldClass` and `volumeSourceClass` has a `categoryKey` entry. Add a one-line test asserting that.

`cmd/k8s-agent/main.go`, right after the `cfg.Validate()` block:

```go
	slog.Info("k8s: job pod policy", "allowed", cfg.JobPodPolicy.Summary())
```

- [ ] **Step 4: Run** `go test ./internal/k8sagent/ -run 'TestJobPodPolicyConfig|TestLoadConfig|TestValidate' -count=1` and `go build ./cmd/k8s-agent/`.

- [ ] **Step 5: Commit** — `feat(k8sagent): jobPodPolicy agent config with startup validation`.

---

### Task 5: Enforce at claim time

**Files:**
- Modify: `internal/k8sagent/agent.go` (`executeRun`)
- Create: `internal/k8sagent/agent_podpolicy_test.go`

**Depends on:** Tasks 1, 3 and 4.

- [ ] **Step 1: Write failing tests** (`agent_podpolicy_test.go`), modelled on `agent_native_test.go`'s `httptest` mux and `fakePM`. Capture the step -1 bulk log body (`[]api.LogAppendRequest`) and the finish status. Tests:

1. `TestK8sExecuteRun_PodPolicyDeniesBeforeAnyPod`: the claim has `PodTemplate: inlineJob(map[string]any{"securityContext": map[string]any{"privileged": true}}, nil)` and one `run:` step. Expect `RunFailed`, a log line starting `k8s: job pod policy:` and containing `allowPrivileged`, and `pm.created == nil`.
2. `TestK8sExecuteRun_PodPolicyDeniesReuseBeforePool`: the same template with `Reuse: true`, and the agent built with `pool: nil`. Today this path would dereference the nil pool, so a pass proves the check runs first. Expect `RunFailed` and no panic.
3. `TestK8sExecuteRun_PodPolicyDeniesBeforeVolumeLocks`: a denied template plus `Volumes: []dsl.VolumeMount{{Name: dsl.StringOrSlice{"go-cache"}, MountPath: "cache"}}`, and a `Config` with `AgentName`/`Volumes` set but no kube client. Expect `RunFailed` with the policy error, not a volume error.
4. `TestK8sExecuteRun_PodPolicyAllowsWithOptIn`: the same privileged template, with `cfg.JobPodPolicy.AllowPrivileged = true`. Expect `pm.created != nil`. Reuse whatever minimal stubbing the existing "pod gets created" tests use (see `agent_env_test.go` / `podwait_test.go`). If a full run is awkward, assert only that `CreatePod` was reached and cancel the context.

Check the exact `dsl.StringOrSlice` literal shape against `internal/dsl/types.go`.

- [ ] **Step 2: Run them and watch them fail** (`go test ./internal/k8sagent/ -run TestK8sExecuteRun_PodPolicy -count=1`).

- [ ] **Step 3: Implement.** In `executeRun`, immediately after the `if c.Native { ... }` block and before the sidecar-S3 check:

```go
	// The job pod field policy (podpolicy.go). Checked before the pool, any
	// volume lock and BuildPod: a reused pod is handed out without BuildPod,
	// and a refused run must neither wait for nor take a volume.
	if err := a.cfg.JobPodPolicy.CheckJobPodTemplate(a.cfg.PodTemplates, c.PodTemplate); err != nil {
		a.failRun(ctx, c.RunID, err.Error())
		return
	}
```

- [ ] **Step 4: Run** the new tests, then the whole package: `go test ./internal/k8sagent/... -count=1`. Every existing test still passes. None of them uses a denied field, which was verified during design. If one does, report it rather than adding an opt-in to make it pass.

- [ ] **Step 5: Commit** — `feat(k8sagent)!: refuse claims whose podTemplate violates the job pod policy`.

---

### Task 6: Pod Security `baseline` on the shipped namespace

**Files:**
- Modify: `manifests/base/namespace/namespace.yaml`
- Create: `internal/k8sagent/manifests_podsecurity_test.go` (no build tag)
- Create: `internal/k8sagent/podsecurity_k8s_test.go` (`//go:build k8s`)

**Depends on:** nothing (it can run in parallel with Tasks 1–5).

- [ ] **Step 1: Label the namespace.** Change only the `ci` document:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: ci
  labels:
    # Job Pods. The k8s agent's jobPodPolicy is the unified-cd-side check; this is the
    # cluster-side one. baseline rejects privileged, hostPath, host namespaces and
    # Unconfined profiles even in operator templates: an agent template that needs them
    # (Docker-in-Docker, rootless BuildKit) needs a namespace labelled privileged.
    # restricted is audited only: agent-built Pods do not meet it, and a warn label
    # would print a warning on every Pod the agent creates.
    pod-security.kubernetes.io/enforce: baseline
    pod-security.kubernetes.io/audit: restricted
```

- [ ] **Step 2: Manifest test** (`manifests_podsecurity_test.go`): read the file via `runtime.Caller` like `credentials_test.go`, normalise `\r\n`, and assert that the `ci` document contains `pod-security.kubernetes.io/enforce: baseline` and `pod-security.kubernetes.io/audit: restricted`, and contains no `pod-security.kubernetes.io/warn`. Split the file on `\n---\n` and select the document containing `name: ci`.

- [ ] **Step 3: Admission test** (`podsecurity_k8s_test.go`, `//go:build k8s`):

```go
//go:build k8s

package k8sagent

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// TestAgentBuiltPodsAreAdmittedUnderBaseline proves spec §6's claim against a
// real Pod Security admission controller: every Pod shape the agent builds is
// admitted in a namespace that enforces baseline. Server-side dry-run runs
// admission without scheduling anything, so no image is pulled.
func TestAgentBuiltPodsAreAdmittedUnderBaseline(t *testing.T) {
	client, _ := newTestKubeClient(t)
	ns := newTestNamespace(t, client)
	ctx := context.Background()
	nsObj, err := client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	require.NoError(t, err)
	nsObj.Labels = map[string]string{"pod-security.kubernetes.io/enforce": "baseline"}
	_, err = client.CoreV1().Namespaces().Update(ctx, nsObj, metav1.UpdateOptions{})
	require.NoError(t, err)
	dry := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}

	for _, mode := range []string{SidecarS3SecretModeEnv, SidecarS3SecretModeFile, SidecarS3SecretModeBroker} {
		sc := SidecarSpec{Image: "sidecar:test", S3SecretName: "ucd-s3", S3SecretMode: mode, BrokerURL: "https://c.example"}
		vols := []PodVolume{{Name: "go-cache", ClaimName: "ci-main-go-cache", MountPath: "cache"}}
		pod, err := BuildPod(uniqueRunID("baseline-"+mode), ns, nil, nil, testImage, sc, testShimImage, vols)
		require.NoError(t, err)
		_, err = client.CoreV1().Pods(ns).Create(ctx, pod, dry)
		require.NoError(t, err, "agent-built run Pod (sidecar mode %s) must be admitted under baseline", mode)

		scope := buildScopePod(uniqueRunID("scope-"+mode), ns, "scope:x", testImage, map[string]string{"A": "1"}, sc, testShimImage, corev1.ResourceRequirements{})
		_, err = client.CoreV1().Pods(ns).Create(ctx, scope, dry)
		require.NoError(t, err, "agent-built scope Pod (sidecar mode %s) must be admitted under baseline", mode)
	}

	// Negative control: admission is really enforcing, so the passes above mean something.
	bad, err := BuildPod(uniqueRunID("baseline-bad"), ns, nil, nil, testImage, SidecarSpec{}, testShimImage, nil)
	require.NoError(t, err)
	bad.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
	_, err = client.CoreV1().Pods(ns).Create(ctx, bad, dry)
	require.Error(t, err)
	require.Contains(t, err.Error(), "violates PodSecurity")
}
```

Check `PodVolume`'s field names against `internal/k8sagent/volumes.go` and adjust the literal to match. Check that `testShimImage` (declared in `podbuilder_test.go`, which has no build tag) is visible under `-tags k8s`.

- [ ] **Step 4: Run** `go test ./internal/k8sagent/ -run TestManifest -count=1`. If a kind or other cluster is reachable, also run `go test -tags k8s ./internal/k8sagent/ -run TestAgentBuiltPodsAreAdmittedUnderBaseline -count=1 -v`. Otherwise state that it runs in CI's `k8s` job. Run `kubectl kustomize manifests/core-install` and `manifests/agent-only` if `kubectl` is available, and confirm the labels render.

- [ ] **Step 5: Commit** — `feat(manifests)!: enforce Pod Security baseline on the shipped ci namespace`.

---

### Task 7: Documentation, examples, templates, migration note, TODO

**Files:**
- Modify: `docs/operator-manual/kubernetes-integration.md`
- Modify: `docs/reference/configuration.md`
- Modify: `docs/operator-manual/authorization.md`
- Modify: `docs/user-guide/writing-jobs/isolation-and-containers.md`
- Modify: `docs/troubleshooting/steps-and-execution.md`
- Create: `docs/operator-manual/migrations/2026-10-26-k8s-job-pod-field-policy.md`
- Modify: `examples/config/k8s-agent.yaml`, `manifests/base/k8s-agent/config-configmap.yaml`
- Modify: `templates/buildkit-rootless-build-push.yaml` (header comment only), `templates/README.md`
- Modify: `TODO.md`
- Check: `README.md`, `mkdocs.yml` (nav entry for the migration note if migrations are listed there), `docs/operator-manual/agents.md`, `test/edgecase/**`

**Depends on:** Tasks 1–6 (the docs describe the final behaviour and quote real error strings, so run the code to copy the strings).

- [ ] **Step 1: `kubernetes-integration.md`.**
  - Replace the whole "### What a job can put in the Pod" section with "### Job Pod field policy", so the anchor becomes `#job-pod-field-policy`. Cover: the trust boundary (spec R3 table, condensed); what is denied by default (spec §5 in condensed tables: PodSpec categories, container rules, volume types, security contexts); the `jobPodPolicy` YAML block with comments (spec §4); the startup validation errors; the run error with a real example captured from Task 5's test output; why some names can never be allowed (R6, R7); the shipped namespace labels and what `baseline` refuses in operator templates (§6); the `default` ServiceAccount recommendation (R10) as a short YAML snippet (`automountServiceAccountToken: false` on `ServiceAccount/default` in the job namespace); and keeping an admission policy for `serviceAccountName` defence in depth.
  - "Security note / threat model": the S3 credentials stay out of the job container **by default**. State the exceptions: `allowPodSpecFields: [shareProcessNamespace]` and the open `container: unified-artifact` question (link the new TODO item from Step 9).
  - Setup section §1 example and the "Named volumes" `fsGroup` example: make sure nothing contradicts the policy (`fsGroup` and `runAsUser` stay allowed). Where an example agent template uses `privileged: true`, add one sentence that `baseline` refuses it in the shipped namespace.
  - The top "intentional differences" bullets: no change unless one now reads as false.
- [ ] **Step 2: `configuration.md`.** Add a commented `jobPodPolicy` block to the K8s Agent config example and a `jobPodPolicy` row to the config-fields table. The row links to the policy section, says the block is file-only with no environment variables, states that the absent block is the most restrictive, and lists the startup errors. Add `jobPodPolicy` to the "Pod template fields" prose: agent templates are not filtered, job content is.
- [ ] **Step 3: `authorization.md`.** Rewrite the paragraph added by 913ea9a. Roles stay global. On a Kubernetes agent, a job's reach is the agent's `jobPodPolicy` plus the namespace's admission. Link `kubernetes-integration.md#job-pod-field-policy`.
- [ ] **Step 4: `isolation-and-containers.md`.** In "Kubernetes Pod Template (`podTemplate`)" and the "podTemplate fields" table (`spec`, `override.*` rows), add that a Kubernetes agent refuses some fields unless its operator allows them, quote the error prefix, and link the policy section.
- [ ] **Step 5: `troubleshooting/steps-and-execution.md`.** Under "Job isolation", add two entries:
  - `Run fails immediately on Kubernetes: "k8s: job pod policy: ..."`: what it means; read the path and the `allow with` hint; the operator either adds the setting or moves the content into an agent template; a job author cannot fix it from the job except by removing the field.
  - `Run fails: "k8s: failed to create Pod: ... violates PodSecurity "baseline:latest""`: the namespace label from the shipped manifests; it applies to operator templates too; the options are relabelling, or a separate agent and namespace for privileged templates.
- [ ] **Step 6: Migration note** `docs/operator-manual/migrations/2026-10-26-k8s-job-pod-field-policy.md`, in the style of `2026-10-23-unevaluable-if-fails-step.md`. Include the spec §8 table (before → after, with the setting that restores each), the exact error prefix and one full example, the startup validation errors, the namespace label change and how to apply it (`kubectl label namespace ci pod-security.kubernetes.io/enforce=baseline` plus a `--dry-run=server` check first, which lists existing violators), and idle pooled Pods built before the upgrade (R12). For R12, give the command to remove them after scaling the agent down: `kubectl -n ci delete pods -l app=unified-cd-agent`. Also give a grep that finds affected job definitions:

```
grep -rnE "serviceAccount|automountServiceAccountToken|hostPath|hostNetwork|hostPID|hostIPC|hostPort|privileged|capabilities|procMount|Unconfined|sysctls|seLinuxOptions|nodeName|secretKeyRef|secretRef|secretName|imagePullSecrets|persistentVolumeClaim|runtimeClassName|priorityClassName|shareProcessNamespace|ephemeral|csi:|nfs:" <your job definitions>
```

  If `mkdocs.yml` lists the migration notes, add the new one.
- [ ] **Step 7: Examples and manifests.** In `examples/config/k8s-agent.yaml`, add a fully commented `jobPodPolicy:` block (every key, with the defaults shown), and above the `golang` template's DinD container a comment that the shipped `ci` namespace (`baseline`) refuses `privileged: true`. In `manifests/base/k8s-agent/config-configmap.yaml`, add a short comment pointing at `jobPodPolicy` (no keys set). Keep `TestExamplesParse` and the config tests green.
- [ ] **Step 8: Templates.** In the header of `templates/buildkit-rootless-build-push.yaml`, extend "Cluster requirements" with two items: the k8s agent must set `jobPodPolicy.allowUnconfinedProfiles: true`, and the job namespace must admit `Unconfined` (Pod Security `privileged`; the shipped `baseline` refuses it). Change the comment only, not the job. Update its row in `templates/README.md` to match. Run `go test ./internal/dsl/ -run 'Templates|Examples' -count=1`.
- [ ] **Step 9: `TODO.md`.** Delete item 36 entirely. Add, under Critical, item `37. Refuse the artifact sidecar as a step's container: target`, following the existing format:
  - **Status:** Needs verification.
  - **Impact:** a `container: unified-artifact` step may run job code inside the container that holds the sidecar's object-store credentials.
  - **Evidence:** `internal/dsl/container.go` `ValidateContainerReferences` accepts every `IsReservedContainerName` target, and `internal/k8sagent/agent.go` `execContainer` has no visible guard.
  - **Done when:** apply rejects the sidecar as a step target, the k8s agent refuses such a claim as a backstop, and tests cover both.
- [ ] **Step 10: Sweep for statements made false.** Run each grep and fix or justify every hit outside `docs/superpowers/` and historical migration notes:

```
grep -rn "What a job can put in the Pod\|what-a-job-can-put-in-the-pod\|TODO item 36" docs README.md examples templates manifests
grep -rn "Nothing else in a job's\|is the only check unified-cd applies\|job container never sees" docs
grep -rn -i "full PodSpec\|real Kubernetes PodSpec\|Kubernetes PodSpec (containers, volumes" docs README.md
grep -rn "serviceAccountName\|hostPath\|privileged: true\|secretKeyRef\|envFrom" docs examples templates test/edgecase | grep -v superpowers
```

  The `secretKeyRef` hits in `migrations/podtemplate-subfield-routing.md` (which tells users to use `valueFrom: secretKeyRef`) and in `high-availability.md` (the controller's own Deployment, which is not job content) need a judgement call. Add a one-line note to the first ("on a Kubernetes agent this now also requires `jobPodPolicy.allowSecrets`; see ..."). Leave the second alone, because it is not a job Pod. For `test/edgecase/**`, any workload that sets a denied field gets a comment, or its rig config gets the opt-in. List what you changed.
- [ ] **Step 11: Verify.** Run `go test ./... -short -count=1`, `go generate ./...` (then confirm with `git status` that no generated file changed), and `mkdocs build --strict` if mkdocs is installed (otherwise check every new link and anchor by hand).
- [ ] **Step 12: Commit** — `docs: describe the k8s job pod field policy and its migration; close TODO 36`.

In your report, list every document you changed, and every grep hit you left unchanged together with the reason.
