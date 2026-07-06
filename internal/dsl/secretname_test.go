package dsl

import "testing"

func TestValidateSecretName(t *testing.T) {
	valid := []string{"AWS_KEY", "github-token", "_private", "a", "GITHUB_TOKEN", "db-pass-1"}
	for _, n := range valid {
		if err := ValidateSecretName(n); err != nil {
			t.Errorf("ValidateSecretName(%q) = %v, want nil (env-var-style names must be allowed)", n, err)
		}
	}
	invalid := []string{"", "1leading-digit", "has space", "has!bang", "has.dot", "スペース"}
	for _, n := range invalid {
		if err := ValidateSecretName(n); err == nil {
			t.Errorf("ValidateSecretName(%q) = nil, want error", n)
		}
	}
}
