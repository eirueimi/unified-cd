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
// The UNIFIED_* names are the agent's own credentials, duplicated from
// internal/agent/stepenv.go's stepEnvDenied — leaking them lets a job author
// act as the agent. They are duplicated rather than imported because
// internal/dsl must not depend on internal/agent; stepEnvDenied remains the
// runtime backstop and a test asserts the two lists agree.
//
// PATH and HOME are refused because a global Vars manifest applies to every
// step of every job: a PATH that shadows the agent's baseline breaks all of
// them at once, in a way whose cause is not visible in the failure.
var reservedVarNames = map[string]bool{
	"UNIFIED_CACHE_KEY":                   true,
	"UNIFIED_CACHE_SECRET":                true,
	"UNIFIED_TOKEN":                       true,
	"UNIFIED_AGENT_CREDENTIAL_FILE":       true,
	"UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE": true,
	"PATH":                                true,
	"HOME":                                true,
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
