package cli

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds the CLI configuration.
type Config struct {
	Server string `yaml:"server"`
	Token  string `yaml:"token"`
	// Headers are extra HTTP headers ("Key: Value") added to every request to
	// Server — e.g. an IAP identity token in "Proxy-Authorization" when the
	// controller sits behind an identity-aware proxy. Applied only to requests
	// whose host matches Server, so they never leak to the OIDC issuer during
	// `login`. `--header`/`-H` and $UNIFIED_HEADER add to this list.
	Headers []string `yaml:"headers,omitempty"`
	// RefreshToken, TokenExpiry, LoginClient and LoginRelay are written by SSO
	// login so the CLI can refresh Token before it expires. TokenExpiry is the
	// id_token exp. LoginClient is "device" or "browser"; LoginRelay is true
	// when the device flow went through the controller relay.
	RefreshToken string    `yaml:"refreshToken,omitempty"`
	TokenExpiry  time.Time `yaml:"tokenExpiry,omitempty"`
	LoginClient  string    `yaml:"loginClient,omitempty"`
	LoginRelay   bool      `yaml:"loginRelay,omitempty"`
}

// LoadConfig reads configuration from a YAML file at the given path.
// Returns an empty config (no error) if the file does not exist.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// SaveConfig writes configuration to a YAML file at the given path, creating
// its parent directory if needed. The write is atomic (temporary file in the
// same directory, then rename) and the file mode is 0600.
func SaveConfig(path string, c Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// DefaultConfigPath returns the default configuration file path.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home + "/.config/unified-cd/config.yaml"
}
