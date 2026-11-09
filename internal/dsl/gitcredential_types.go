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
	Host      string `yaml:"host"`                            // hostname to use these credentials for (e.g. github.com)
	Type      string `yaml:"type" schema:"enum:token,sshKey"` // authentication type
	SecretRef string `yaml:"secretRef"`                       // name of the StoredSecret that holds the value
	// KnownHosts optionally pins the SSH host keys (OpenSSH known_hosts lines) of host; sshKey type only.
	// When set, ssh runs with StrictHostKeyChecking=yes against exactly these keys; when empty the controller
	// uses trust-on-first-use and rejects a host key that later changes.
	KnownHosts string `yaml:"knownHosts,omitempty"`
}
