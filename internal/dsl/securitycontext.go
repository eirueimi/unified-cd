package dsl

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// hostHonoursHardeningFields is the single switch for tier B of the
// securityContext design (docs/superpowers/specs/2026-10-23-host-security-context-design.md
// section 4): the flag-only hardening fields allowPrivilegeEscalation,
// capabilities.drop/add, privileged: false, readOnlyRootFilesystem, procMount:
// Default and seccompProfile/appArmorProfile RuntimeDefault. While false, every
// one of them is pinned to a Kubernetes agent; flipping it to true makes the
// host honour them. Tier A (identity, fsGroup) does not depend on it.
const hostHonoursHardeningFields = true

// maxSecurityID is the largest uid/gid Kubernetes accepts (int32 max).
const maxSecurityID = 2147483647

// EffectiveSecurity is the securityContext of one container after resolving
// the pod-level and container-level values (Kubernetes precedence). It is pure
// data: the host claim-pod builder and the routing predicate both read it.
type EffectiveSecurity struct {
	UID, GID *int64
	// SupplementalGroups, FSGroup and FSGroupChangePolicy are pod-level only,
	// as in Kubernetes; the builder unions FSGroup into the group list.
	SupplementalGroups  []int64
	FSGroup             *int64
	FSGroupChangePolicy string
	RunAsNonRoot        bool
	CapDrop, CapAdd     []string
	NoNewPrivileges     bool // allowPrivilegeEscalation: false
	ReadOnlyRootfs      bool // readOnlyRootFilesystem: true
}

// securityID decodes a uid/gid. YAML yields int, the store/claim JSON round
// trip yields float64; anything else, a fractional number or an out-of-range
// value is an error. path names the field in the message.
func securityID(path string, v any) (int64, error) {
	var n int64
	switch x := v.(type) {
	case int:
		n = int64(x)
	case int64:
		n = x
	case float64:
		if x != math.Trunc(x) || x < 0 || x > maxSecurityID {
			return 0, badSecurityID(path, v)
		}
		n = int64(x)
	default:
		return 0, badSecurityID(path, v)
	}
	if n < 0 || n > maxSecurityID {
		return 0, badSecurityID(path, v)
	}
	return n, nil
}

func badSecurityID(path string, v any) error {
	return fmt.Errorf("%s: must be an integer between 0 and %d (got %#v)", path, maxSecurityID, v)
}

// optionalID reads an optional uid/gid field; nil counts as unset.
func optionalID(sc map[string]any, key, path string) (*int64, error) {
	v, ok := sc[key]
	if !ok || v == nil {
		return nil, nil
	}
	n, err := securityID(path+"."+key, v)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func idList(sc map[string]any, key, path string) ([]int64, error) {
	v, ok := sc[key]
	if !ok || v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s.%s: must be a list of integers (got %#v)", path, key, v)
	}
	out := make([]int64, 0, len(list))
	for i, e := range list {
		n, err := securityID(fmt.Sprintf("%s.%s[%d]", path, key, i), e)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func stringList(sc map[string]any, key, path string) ([]string, error) {
	v, ok := sc[key]
	if !ok || v == nil {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s.%s: must be a list of strings (got %#v)", path, key, v)
	}
	out := make([]string, 0, len(list))
	for i, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("%s.%s[%d]: must be a string (got %#v)", path, key, i, e)
		}
		out = append(out, s)
	}
	return out, nil
}

func optionalBool(sc map[string]any, key, path string) (*bool, error) {
	v, ok := sc[key]
	if !ok || v == nil {
		return nil, nil
	}
	b, ok := v.(bool)
	if !ok {
		return nil, fmt.Errorf("%s.%s: must be a boolean (got %#v)", path, key, v)
	}
	return &b, nil
}

// ResolveSecurity resolves the effective identity and hardening of one
// container (design section 5.1). A container value overrides the pod value
// for runAsUser, runAsGroup and runAsNonRoot; supplementalGroups, fsGroup and
// fsGroupChangePolicy come from the pod only; capabilities,
// allowPrivilegeEscalation and readOnlyRootFilesystem from the container only.
// Either map may be nil. The error names the offending field.
func ResolveSecurity(podSC, containerSC map[string]any) (EffectiveSecurity, error) {
	const podPath, ctrPath = "podTemplate.spec.securityContext", "container.securityContext"
	var eff EffectiveSecurity
	var err error

	if eff.UID, err = optionalID(containerSC, "runAsUser", ctrPath); err != nil {
		return EffectiveSecurity{}, err
	}
	if eff.UID == nil {
		if eff.UID, err = optionalID(podSC, "runAsUser", podPath); err != nil {
			return EffectiveSecurity{}, err
		}
	}
	if eff.GID, err = optionalID(containerSC, "runAsGroup", ctrPath); err != nil {
		return EffectiveSecurity{}, err
	}
	if eff.GID == nil {
		if eff.GID, err = optionalID(podSC, "runAsGroup", podPath); err != nil {
			return EffectiveSecurity{}, err
		}
	}
	if eff.SupplementalGroups, err = idList(podSC, "supplementalGroups", podPath); err != nil {
		return EffectiveSecurity{}, err
	}
	if eff.FSGroup, err = optionalID(podSC, "fsGroup", podPath); err != nil {
		return EffectiveSecurity{}, err
	}
	if v, ok := podSC["fsGroupChangePolicy"]; ok && v != nil {
		if err := checkEnum(podPath+".fsGroupChangePolicy", v, "Always", "OnRootMismatch"); err != nil {
			return EffectiveSecurity{}, err
		}
		eff.FSGroupChangePolicy = v.(string)
	}

	nonRoot, err := optionalBool(containerSC, "runAsNonRoot", ctrPath)
	if err != nil {
		return EffectiveSecurity{}, err
	}
	if nonRoot == nil {
		if nonRoot, err = optionalBool(podSC, "runAsNonRoot", podPath); err != nil {
			return EffectiveSecurity{}, err
		}
	}
	eff.RunAsNonRoot = nonRoot != nil && *nonRoot

	if caps, ok := containerSC["capabilities"]; ok && caps != nil {
		cm, ok := caps.(map[string]any)
		if !ok {
			return EffectiveSecurity{}, fmt.Errorf("%s.capabilities: must be a map (got %#v)", ctrPath, caps)
		}
		if eff.CapDrop, err = stringList(cm, "drop", ctrPath+".capabilities"); err != nil {
			return EffectiveSecurity{}, err
		}
		if eff.CapAdd, err = stringList(cm, "add", ctrPath+".capabilities"); err != nil {
			return EffectiveSecurity{}, err
		}
	}
	ape, err := optionalBool(containerSC, "allowPrivilegeEscalation", ctrPath)
	if err != nil {
		return EffectiveSecurity{}, err
	}
	eff.NoNewPrivileges = ape != nil && !*ape
	ro, err := optionalBool(containerSC, "readOnlyRootFilesystem", ctrPath)
	if err != nil {
		return EffectiveSecurity{}, err
	}
	eff.ReadOnlyRootfs = ro != nil && *ro
	return eff, nil
}

// securityField classifies one securityContext key. honoured reports whether
// the host honours the given value. tierB marks the flag-only hardening fields
// that follow hostHonoursHardeningFields.
type securityField struct {
	tierB    bool
	honoured func(v any) bool
}

func isBool(v any) bool { _, ok := v.(bool); return ok }

func isID(v any) bool {
	_, err := securityID("", v)
	return err == nil
}

func oneOf(allowed ...string) func(any) bool {
	return func(v any) bool {
		s, ok := v.(string)
		if !ok {
			return false
		}
		for _, a := range allowed {
			if s == a {
				return true
			}
		}
		return false
	}
}

// profileHonoured accepts {type: RuntimeDefault} only: Unconfined widens and
// Localhost names a node-local file.
func profileHonoured(v any) bool {
	m, ok := v.(map[string]any)
	return ok && len(m) == 1 && oneOf("RuntimeDefault")(m["type"])
}

func idListHonoured(v any) bool {
	list, ok := v.([]any)
	if !ok {
		return false
	}
	for _, e := range list {
		if !isID(e) {
			return false
		}
	}
	return true
}

// capabilitiesHonoured accepts drop (any capability names) and add limited to
// NET_BIND_SERVICE, the one capability Pod Security Standards "restricted"
// allows.
func capabilitiesHonoured(v any) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	for k, val := range m {
		if k != "drop" && k != "add" {
			return false
		}
		list, ok := val.([]any)
		if !ok {
			return false
		}
		for _, e := range list {
			s, ok := e.(string)
			if !ok || (k == "add" && s != "NET_BIND_SERVICE") {
				return false
			}
		}
	}
	return true
}

// Decision tables, design sections 4.1 and 4.2. Any key not listed is pinned.
var podSecurityFields = map[string]securityField{
	"runAsUser":                {honoured: isID},
	"runAsGroup":               {honoured: isID},
	"runAsNonRoot":             {honoured: isBool},
	"supplementalGroups":       {honoured: idListHonoured},
	"fsGroup":                  {honoured: isID},
	"fsGroupChangePolicy":      {honoured: oneOf("Always", "OnRootMismatch")},
	"supplementalGroupsPolicy": {honoured: oneOf("Merge")},
	"seccompProfile":           {tierB: true, honoured: profileHonoured},
	"appArmorProfile":          {tierB: true, honoured: profileHonoured},
}

var containerSecurityFields = map[string]securityField{
	"runAsUser":                {honoured: isID},
	"runAsGroup":               {honoured: isID},
	"runAsNonRoot":             {honoured: isBool},
	"allowPrivilegeEscalation": {tierB: true, honoured: isBool},
	"capabilities":             {tierB: true, honoured: capabilitiesHonoured},
	"privileged":               {tierB: true, honoured: func(v any) bool { b, ok := v.(bool); return ok && !b }},
	"readOnlyRootFilesystem":   {tierB: true, honoured: isBool},
	"procMount":                {tierB: true, honoured: oneOf("Default")},
	"seccompProfile":           {tierB: true, honoured: profileHonoured},
	"appArmorProfile":          {tierB: true, honoured: profileHonoured},
}

// securityNeedsKubernetes is true when sc sets a key or value the host does
// not honour. A nil value counts as unset, as it does in Kubernetes.
func securityNeedsKubernetes(sc map[string]any, table map[string]securityField) bool {
	for key, v := range sc {
		if v == nil {
			continue
		}
		f, ok := table[key]
		if !ok || (f.tierB && !hostHonoursHardeningFields) || !f.honoured(v) {
			return true
		}
	}
	return false
}

// podSecurityNeedsKubernetes reports whether a pod-level securityContext uses
// anything the host does not honour (design section 4.1).
func podSecurityNeedsKubernetes(sc map[string]any) bool {
	return securityNeedsKubernetes(sc, podSecurityFields)
}

// containerSecurityNeedsKubernetes reports whether a container-level
// securityContext uses anything the host does not honour (design section 4.2).
func containerSecurityNeedsKubernetes(sc map[string]any) bool {
	return securityNeedsKubernetes(sc, containerSecurityFields)
}

// HasSecurityContext reports whether pt sets a non-empty pod-level or
// container-level securityContext.
func HasSecurityContext(pt *PodTemplate) bool {
	if pt == nil {
		return false
	}
	if sc, _ := pt.Spec["securityContext"].(map[string]any); len(sc) > 0 {
		return true
	}
	for _, c := range PodTemplateContainers(pt) {
		if sc, _ := c["securityContext"].(map[string]any); len(sc) > 0 {
			return true
		}
	}
	return false
}

// validatePodTemplateSecurityContext is the apply-time check of design
// section 4.3: Kubernetes rejects these values when it decodes the PodSpec, so
// the DSL rejects them up front and both backends fail identically and early.
func validatePodTemplateSecurityContext(pt *PodTemplate) error {
	if pt == nil {
		return nil
	}
	const podPath = "podTemplate.spec.securityContext"
	if v, ok := pt.Spec["securityContext"]; ok && v != nil {
		sc, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: must be a map (got %#v)", podPath, v)
		}
		if err := validateSecurityContext(podPath, sc, true); err != nil {
			return err
		}
	}
	raw, _ := pt.Spec["containers"].([]any)
	for i, r := range raw {
		c, ok := r.(map[string]any)
		if !ok {
			continue
		}
		v, ok := c["securityContext"]
		if !ok || v == nil {
			continue
		}
		path := fmt.Sprintf("podTemplate.spec.containers[%d].securityContext", i)
		sc, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: must be a map (got %#v)", path, v)
		}
		if err := validateSecurityContext(path, sc, false); err != nil {
			return err
		}
	}
	return nil
}

func validateSecurityContext(path string, sc map[string]any, pod bool) error {
	// Sorted so the first reported error is deterministic.
	keys := make([]string, 0, len(sc))
	for k := range sc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		v := sc[key]
		if v == nil {
			continue
		}
		var err error
		switch {
		case key == "runAsUser" || key == "runAsGroup" || (pod && key == "fsGroup"):
			_, err = securityID(path+"."+key, v)
		case pod && key == "supplementalGroups":
			_, err = idList(sc, key, path)
		case pod && key == "fsGroupChangePolicy":
			err = checkEnum(path+"."+key, v, "Always", "OnRootMismatch")
		case pod && key == "supplementalGroupsPolicy":
			err = checkEnum(path+"."+key, v, "Merge", "Strict")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func checkEnum(path string, v any, allowed ...string) error {
	if oneOf(allowed...)(v) {
		return nil
	}
	return fmt.Errorf("%s: must be one of %s (got %#v)", path, strings.Join(allowed, ", "), v)
}
