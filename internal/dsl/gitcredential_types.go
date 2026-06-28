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
	Host      string `yaml:"host"`                                // hostname to use these credentials for (e.g. github.com)
	Type      string `yaml:"type" schema:"enum:token,sshKey"`     // authentication type
	SecretRef string `yaml:"secretRef"`                           // name of the StoredSecret that holds the value
}
