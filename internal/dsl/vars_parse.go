package dsl

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// varKeyRe is the environment-variable name syntax. The values become
// environment variables, so a key that is not a valid variable name has no
// useful behaviour to fall back on.
var varKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reservedVarNames may never be defined as a variable.
//
// Three groups, refused for two different reasons.
//
// CREDENTIALS. UNIFIED_CACHE_KEY, UNIFIED_CACHE_SECRET, UNIFIED_TOKEN,
// UNIFIED_AGENT_CREDENTIAL_FILE and UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE are the
// agent's own credentials, duplicated from internal/agent/stepenv.go's
// stepEnvDenied — leaking them lets a job author act as the agent. They are
// duplicated rather than imported because internal/dsl must not depend on
// internal/agent.
//
// SHADOWING THE STEP'S OWN CONTRACT. PATH and HOME are the shell's baseline;
// UNIFIED_WORKSPACE and UNIFIED_AGENT_OS are synthesised by the orchestrator
// into the SAME extraEnv slice the variables are appended to, and appended
// AFTER them, so the last definition wins. A global Vars manifest applies to
// every step of every job, so one of these silently redefines the ground every
// step stands on: a shadowed PATH breaks every step at once, and a shadowed
// UNIFIED_WORKSPACE has every step succeed while reading and writing the wrong
// directory — which the docs actively tell authors to build artifact and cache
// paths from. Neither leaves anything in a log to say what happened. The cost
// of refusing is that nobody can set them globally, which nobody should want
// to do.
//
// The runtime backstop is internal/agent's varsDenied, which filters a claim's
// vars against this whole set (via ReservedVarNames), against stepEnvDenied,
// and against the orchestrator's synthesised names. The test that all three
// agree lives in internal/agent — the package that can import both, and so
// compare the real structures instead of a hand-copied list — as
// TestVarsDenied_AgreesWithApplyTimeValidation.
//
// Names here are upper-case; ValidateVarKeys upper-cases before the lookup, so
// `path` and `Path` are refused too.
var reservedVarNames = map[string]bool{
	"UNIFIED_CACHE_KEY":                   true,
	"UNIFIED_CACHE_SECRET":                true,
	"UNIFIED_TOKEN":                       true,
	"UNIFIED_AGENT_CREDENTIAL_FILE":       true,
	"UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE": true,
	"UNIFIED_WORKSPACE":                   true,
	"UNIFIED_AGENT_OS":                    true,
	"PATH":                                true,
	"HOME":                                true,
}

// ReservedVarNames returns the variable names apply-time validation refuses,
// upper-cased. It exists so the agent's runtime backstop can filter against
// the real set rather than a copy that drifts: a copy is what the two lists
// already are, and one drifting list is enough.
func ReservedVarNames() map[string]bool {
	out := make(map[string]bool, len(reservedVarNames))
	for k, v := range reservedVarNames {
		out[k] = v
	}
	return out
}

// ValidateVarKeys checks key syntax and reserved names for one map of
// variables. It reports every offending key, sorted, so an author fixing a
// large manifest does not have to re-apply once per mistake.
func ValidateVarKeys(vars map[string]string) error {
	var bad []string
	for k := range vars {
		switch {
		case k == "":
			bad = append(bad, "(empty key)")
		case !varKeyRe.MatchString(k):
			bad = append(bad, fmt.Sprintf("%q is not a valid variable name (want [A-Za-z_][A-Za-z0-9_]*)", k))
		case reservedVarNames[strings.ToUpper(k)]:
			bad = append(bad, fmt.Sprintf("%q is reserved", k))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("invalid vars: %s", strings.Join(bad, "; "))
}

// CheckVarsCollision reports keys defined by two different Vars manifests.
//
// Last-writer-wins would make the effective value depend on apply order, which
// is a debugging problem disguised as a feature. Two manifests with the SAME
// name are the same manifest being re-applied, which is not a collision.
func CheckVarsCollision(existing map[string]string, existingManifest string,
	incoming map[string]string, incomingManifest string) error {
	if existingManifest == incomingManifest {
		return nil
	}
	var dup []string
	for k := range incoming {
		if _, ok := existing[k]; ok {
			dup = append(dup, k)
		}
	}
	if len(dup) == 0 {
		return nil
	}
	sort.Strings(dup)
	return fmt.Errorf("vars %s defined by both %q and %q",
		strings.Join(dup, ", "), existingManifest, incomingManifest)
}

// ParseVars decodes and validates a kind: Vars YAML document from an io.Reader.
func ParseVars(r io.Reader) (*Vars, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var v Vars
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode yaml: %w", err)
	}
	if err := v.Validate(); err != nil {
		return nil, err
	}
	return &v, nil
}

// Validate validates the required fields and consistency of a Vars manifest.
func (v *Vars) Validate() error {
	if v.APIVersion != SupportedAPIVersion {
		return fmt.Errorf("unsupported apiVersion %q", v.APIVersion)
	}
	if v.Kind != "Vars" {
		return fmt.Errorf("unsupported kind %q (want \"Vars\")", v.Kind)
	}
	if err := ValidateName(v.Metadata.Name); err != nil {
		return fmt.Errorf("metadata.name %w", err)
	}
	if err := ValidateVarKeys(v.Spec.Vars); err != nil {
		return err
	}
	return nil
}
