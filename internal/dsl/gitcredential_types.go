package dsl

// GitCredential is the DSL type that defines git credentials for private repositories.
type GitCredential struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   Metadata          `yaml:"metadata"`
	Spec       GitCredentialSpec `yaml:"spec"`
}

// GitCredentialSpec is the spec section of GitCredential.
type GitCredentialSpec struct {
	Host      string `yaml:"host"`                                      // hostname to use these credentials for (e.g. github.com)
	Type      string `yaml:"type" schema:"enum:token,sshKey,githubApp"` // authentication type
	SecretRef string `yaml:"secretRef"`                                 // name of the StoredSecret that holds the value (token, SSH private key, or GitHub App PEM private key)
	// KnownHosts optionally pins the SSH host keys (OpenSSH known_hosts lines) of host; sshKey type only.
	// When set, ssh runs with StrictHostKeyChecking=yes against exactly these keys; when empty the controller
	// uses trust-on-first-use and rejects a host key that later changes.
	KnownHosts string `yaml:"knownHosts,omitempty"`
	// ControllerUse controls whether the controller may use this credential for its own git fetches
	// (AppSource sync and uses: resolution). Defaults to true.
	ControllerUse *bool `yaml:"controllerUse,omitempty"`
	// GitHubApp configures the GitHub App; required for, and only valid with, type githubApp.
	GitHubApp *GitHubAppSpec `yaml:"githubApp,omitempty"`
	// JobAccess optionally lets jobs mint short-lived tokens from this credential; githubApp type only.
	// A credential with jobAccess shares the secrets namespace, so its name must not collide with a StoredSecret.
	JobAccess *GitJobAccessSpec `yaml:"jobAccess,omitempty"`
}

// ControllerUseOrDefault returns spec.controllerUse, or true when it is omitted.
func (s GitCredentialSpec) ControllerUseOrDefault() bool {
	return s.ControllerUse == nil || *s.ControllerUse
}

// GitHubAppSpec identifies the GitHub App behind a githubApp credential.
type GitHubAppSpec struct {
	AppID int64 `yaml:"appId"` // numeric GitHub App ID
	// InstallationID is the installation to mint tokens for. When 0 or omitted, the controller looks it up
	// from the repository owner.
	InstallationID int64 `yaml:"installationId,omitempty"`
	// APIURL is the GitHub REST API base URL; defaults to https://api.github.com. For GitHub Enterprise
	// Server use https://HOST/api/v3.
	APIURL string `yaml:"apiURL,omitempty"`
}

// GitJobAccessSpec lets jobs obtain short-lived tokens minted from a githubApp credential. A credential
// without jobAccess cannot be used by jobs.
type GitJobAccessSpec struct {
	// Folders lists the job folders allowed to use this credential ("*" allows every job).
	Folders []string `yaml:"folders"`
	// Repositories lists the repositories a job token may reach, as "owner/name", "owner/*" or
	// "owner/prefix*". All entries must share one owner, and "owner/*" must stand alone.
	Repositories []string `yaml:"repositories"`
	// Permissions maps GitHub App permission names (for example contents, pull_requests) to the highest
	// level a job may request: read, write or admin.
	Permissions map[string]string `yaml:"permissions"`
}
