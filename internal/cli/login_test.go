package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestFetchServerOIDCConfig_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/auth/oidc-config", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"issuer":"https://dex.example.com","clientId":"cli"}`))
	}))
	defer srv.Close()

	cfg, err := fetchServerOIDCConfig(srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "https://dex.example.com", cfg.Issuer)
	assert.Equal(t, "cli", cfg.ClientID)
}

func TestFetchServerOIDCConfig_404_ReturnsErrOIDCNotConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cfg, err := fetchServerOIDCConfig(srv.URL)
	assert.Nil(t, cfg)
	assert.ErrorIs(t, err, errOIDCNotConfigured)
}

func TestFetchServerOIDCConfig_500_ReturnsGenericError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg, err := fetchServerOIDCConfig(srv.URL)
	assert.Nil(t, cfg)
	require.Error(t, err)
	assert.NotErrorIs(t, err, errOIDCNotConfigured, "HTTP 500 must not return the sentinel")
}

// newTestCmd creates a minimal cobra.Command wired with the given stdin reader and a
// captured stdout. Used to test tokenPromptLogin without touching os.Stdin directly.
func newTestCmd(stdin string) (*cobra.Command, *bytes.Buffer) {
	out := &bytes.Buffer{}
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetOut(out)
	cmd.SetErr(out)
	return cmd, out
}

func TestTokenPromptLogin_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/tokens":
			assert.Equal(t, "Bearer mytoken123", r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cmd, out := newTestCmd("mytoken123\n")
	err := tokenPromptLogin(cmd, srv.URL, configPath)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "SSO is not configured on this server.")
	assert.Contains(t, out.String(), "Logged in.")

	// Verify config file written correctly
	b, err := os.ReadFile(configPath)
	require.NoError(t, err)
	var cfg AgentConfig
	require.NoError(t, yaml.Unmarshal(b, &cfg))
	assert.Equal(t, srv.URL, cfg.Server)
	assert.Equal(t, "mytoken123", cfg.Token)
}

func TestTokenPromptLogin_InvalidToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/tokens":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cmd, _ := newTestCmd("badtoken\n")
	err := tokenPromptLogin(cmd, srv.URL, configPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid token")

	// Config must NOT have been written
	_, statErr := os.Stat(configPath)
	assert.True(t, os.IsNotExist(statErr), "config must not be written on auth failure")
}

func TestTokenPromptLogin_EmptyToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cmd, _ := newTestCmd("\n")
	err := tokenPromptLogin(cmd, srv.URL, configPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token cannot be empty")
}

func TestTokenPromptLogin_VerificationServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cmd, _ := newTestCmd("sometoken\n")
	err := tokenPromptLogin(cmd, srv.URL, configPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token verification failed")
}
