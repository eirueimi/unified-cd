package dsl

import (
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParseGitCredential decodes and validates a GitCredential YAML document.
func ParseGitCredential(r io.Reader) (*GitCredential, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var gc GitCredential
	if err := dec.Decode(&gc); err != nil {
		return nil, fmt.Errorf("decode yaml: %w", err)
	}
	if err := gc.Validate(); err != nil {
		return nil, err
	}
	return &gc, nil
}

// Validate validates the required fields of a GitCredential.
func (gc *GitCredential) Validate() error {
	if gc.APIVersion != SupportedAPIVersion {
		return fmt.Errorf("unsupported apiVersion %q (want %q)", gc.APIVersion, SupportedAPIVersion)
	}
	if gc.Kind != "GitCredential" {
		return fmt.Errorf("unsupported kind %q (want \"GitCredential\")", gc.Kind)
	}
	if err := ValidateName(gc.Metadata.Name); err != nil {
		return fmt.Errorf("metadata.name %w", err)
	}
	if gc.Spec.Host == "" {
		return fmt.Errorf("spec.host is required")
	}
	if gc.Spec.Type != "token" && gc.Spec.Type != "sshKey" && gc.Spec.Type != "githubApp" {
		return fmt.Errorf("spec.type must be \"token\", \"sshKey\" or \"githubApp\", got %q", gc.Spec.Type)
	}
	if gc.Spec.SecretRef == "" {
		return fmt.Errorf("spec.secretRef is required")
	}
	if gc.Spec.KnownHosts != "" {
		if gc.Spec.Type != "sshKey" {
			return fmt.Errorf("spec.knownHosts is only valid for type \"sshKey\"")
		}
		if err := ValidateKnownHosts(gc.Spec.KnownHosts); err != nil {
			return fmt.Errorf("spec.%w", err)
		}
	}
	if err := gc.validateGitHubApp(); err != nil {
		return err
	}
	return gc.validateJobAccess()
}

var permissionNameRe = regexp.MustCompile(`^[a-z][a-z_]*$`)

func (gc *GitCredential) validateGitHubApp() error {
	app := gc.Spec.GitHubApp
	if gc.Spec.Type != "githubApp" {
		if app != nil {
			return fmt.Errorf("spec.githubApp is only valid for type \"githubApp\"")
		}
		return nil
	}
	if app == nil {
		return fmt.Errorf("spec.githubApp is required for type \"githubApp\"")
	}
	if app.AppID <= 0 {
		return fmt.Errorf("spec.githubApp.appId must be a positive integer")
	}
	if app.InstallationID < 0 {
		return fmt.Errorf("spec.githubApp.installationId must not be negative")
	}
	if app.APIURL != "" {
		u, err := url.Parse(app.APIURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return fmt.Errorf("spec.githubApp.apiURL must be an https URL without credentials, query or fragment")
		}
	}
	return nil
}

func (gc *GitCredential) validateJobAccess() error {
	ja := gc.Spec.JobAccess
	if ja == nil {
		return nil
	}
	if gc.Spec.Type != "githubApp" {
		return fmt.Errorf("spec.jobAccess is only valid for type \"githubApp\"")
	}
	if len(ja.Folders) == 0 {
		return fmt.Errorf("spec.jobAccess.folders must not be empty")
	}
	if len(ja.Repositories) == 0 {
		return fmt.Errorf("spec.jobAccess.repositories must not be empty")
	}
	if len(ja.Permissions) == 0 {
		return fmt.Errorf("spec.jobAccess.permissions must not be empty")
	}
	if _, _, err := ParseRepositoryPatterns(ja.Repositories); err != nil {
		return fmt.Errorf("spec.jobAccess.%w", err)
	}
	names := make([]string, 0, len(ja.Permissions))
	for k := range ja.Permissions {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if !permissionNameRe.MatchString(k) {
			return fmt.Errorf("spec.jobAccess.permissions: invalid permission name %q", k)
		}
		switch ja.Permissions[k] {
		case "read", "write", "admin":
		default:
			return fmt.Errorf("spec.jobAccess.permissions.%s must be \"read\", \"write\" or \"admin\", got %q", k, ja.Permissions[k])
		}
	}
	if err := ValidateSecretName(gc.Metadata.Name); err != nil {
		return fmt.Errorf("metadata.name %q must match the secret-name pattern ^[A-Za-z_][A-Za-z0-9_-]*$ when spec.jobAccess is set", gc.Metadata.Name)
	}
	return nil
}

// MaxKnownHostsBytes caps the size of GitCredential spec.knownHosts.
const MaxKnownHostsBytes = 64 << 10

// ValidateKnownHosts checks that s is a plausible OpenSSH known_hosts block: blank
// lines and "#" comments are allowed, every other line needs at least a host
// pattern, a key type and a base64 key (an optional leading "@cert-authority" or
// "@revoked" marker is accepted). It does not verify the key material itself.
func ValidateKnownHosts(s string) error {
	if len(s) > MaxKnownHostsBytes {
		return fmt.Errorf("knownHosts is larger than %d bytes", MaxKnownHostsBytes)
	}
	n := 0
	for i, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if strings.HasPrefix(fields[0], "@") {
			fields = fields[1:]
		}
		if len(fields) < 3 {
			return fmt.Errorf("knownHosts line %d: want \"<host> <key-type> <base64-key>\"", i+1)
		}
		n++
	}
	if n == 0 {
		return fmt.Errorf("knownHosts has no key lines")
	}
	return nil
}
