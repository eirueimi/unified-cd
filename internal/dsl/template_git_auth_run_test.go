package dsl

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireGit231(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("requires a POSIX sh")
	}
	out, err := exec.Command("git", "version").Output()
	if err != nil {
		t.Skip("git not available")
	}
	m := regexp.MustCompile(`(\d+)\.(\d+)`).FindStringSubmatch(string(out))
	if m == nil {
		t.Skip("cannot parse git version")
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	if maj < 2 || (maj == 2 && min < 31) {
		t.Skipf("git %s.%s < 2.31 (GIT_CONFIG_COUNT unsupported)", m[1], m[2])
	}
}

func loadGitAuthRunScript(t *testing.T) (*JobTemplate, string) {
	t.Helper()
	data, err := os.ReadFile("../../templates/git-auth-run.yaml")
	require.NoError(t, err)
	tpl, err := ParseJobTemplate(data)
	require.NoError(t, err)
	require.NotEmpty(t, tpl.Spec.Steps)
	return tpl, tpl.Spec.Steps[0].Run
}

type authRecorder struct {
	mu   sync.Mutex
	seen map[string][]string // path prefix -> passwords
}

func (r *authRecorder) handler(w http.ResponseWriter, req *http.Request) {
	_, pw, ok := req.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	prefix := "other"
	for _, p := range []string{"/acme/", "/widget/"} {
		if strings.HasPrefix(req.URL.Path, p) {
			prefix = p
		}
	}
	r.mu.Lock()
	r.seen[prefix] = append(r.seen[prefix], pw)
	r.mu.Unlock()
	w.WriteHeader(http.StatusNotFound)
}

func runGitAuthScript(t *testing.T, script string, extra map[string]string) (*authRecorder, string) {
	t.Helper()
	rec := &authRecorder{seen: map[string][]string{}}
	srv := httptest.NewTLSServer(http.HandlerFunc(rec.handler))
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	require.NoError(t, err)
	hostport := "127.0.0.1:" + port

	home := t.TempDir()
	gcfg := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(gcfg, []byte("[credential]\n\thelper = \"!f() { echo username=wrong; echo password=global-helper; }; f\"\n"), 0o600))

	env := append(os.Environ(),
		"GIT_AUTH_HOST="+hostport,
		"GIT_SSL_NO_VERIFY=1",
		"HOME="+home,
		"GIT_CONFIG_GLOBAL="+gcfg,
		"GIT_TERMINAL_PROMPT=0",
	)
	for k, v := range extra {
		env = append(env, strings.ReplaceAll(k+"="+v, "{HOSTPORT}", hostport))
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = env
	cmd.Dir = t.TempDir() // outside any enclosing repository
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return rec, home
}

func assertNoFileContains(t *testing.T, dir string, needles ...string) {
	t.Helper()
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, n := range needles {
			assert.NotContains(t, string(b), n, "token leaked to disk: %s", p)
		}
		return nil
	})
}

func TestGitAuthRunTemplate_RoutesTokenPerOwner(t *testing.T) {
	requireGit231(t)
	_, script := loadGitAuthRunScript(t)
	rec, home := runGitAuthScript(t, script, map[string]string{
		"GIT_AUTH_OWNER_1": "acme", "GIT_AUTH_TOKEN_1": "tok-acme",
		"GIT_AUTH_OWNER_2": "widget", "GIT_AUTH_TOKEN_2": "tok-widget",
		"GIT_AUTH_COMMAND": "git ls-remote https://{HOSTPORT}/acme/app.git; git ls-remote https://{HOSTPORT}/widget/lib.git; true",
	})
	assert.Contains(t, rec.seen["/acme/"], "tok-acme")
	assert.Contains(t, rec.seen["/widget/"], "tok-widget")
	assert.NotContains(t, rec.seen["/acme/"], "tok-widget")
	assert.NotContains(t, rec.seen["/widget/"], "tok-acme")
	for _, pws := range rec.seen {
		assert.NotContains(t, pws, "global-helper")
	}
	assertNoFileContains(t, home, "tok-acme", "tok-widget")
}

func TestGitAuthRunTemplate_EmptySlotsIgnored(t *testing.T) {
	requireGit231(t)
	_, script := loadGitAuthRunScript(t)
	rec, _ := runGitAuthScript(t, script, map[string]string{
		"GIT_AUTH_OWNER_1": "acme", "GIT_AUTH_TOKEN_1": "tok-acme",
		"GIT_AUTH_OWNER_2": "", "GIT_AUTH_TOKEN_2": "",
		"GIT_AUTH_COMMAND": `git ls-remote https://{HOSTPORT}/acme/app.git; test "$GIT_CONFIG_COUNT" = 3`,
	})
	assert.Contains(t, rec.seen["/acme/"], "tok-acme")
	// useHttpPath + (reset, helper) for one owner = 3 entries.
}

func TestGitAuthRunTemplate_RequiresCommandAndSlot1(t *testing.T) {
	requireGit231(t)
	_, script := loadGitAuthRunScript(t)
	for name, env := range map[string][]string{
		"no command": {"GIT_AUTH_OWNER_1=acme", "GIT_AUTH_TOKEN_1=x", "GIT_AUTH_COMMAND="},
		"no slot 1":  {"GIT_AUTH_OWNER_1=", "GIT_AUTH_TOKEN_1=", "GIT_AUTH_COMMAND=true"},
	} {
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(), append([]string{"GIT_AUTH_HOST=github.com"}, env...)...)
		out, err := cmd.CombinedOutput()
		assert.Error(t, err, name)
		assert.Contains(t, string(out), "git-auth-run:", name)
	}
}

func TestGitAuthRunTemplate_HalfFilledSlotFails(t *testing.T) {
	requireGit231(t)
	_, script := loadGitAuthRunScript(t)
	for name, env := range map[string][]string{
		"owner without credential": {"GIT_AUTH_OWNER_1=acme", "GIT_AUTH_TOKEN_1=x", "GIT_AUTH_OWNER_2=widget", "GIT_AUTH_TOKEN_2="},
		"credential without owner": {"GIT_AUTH_OWNER_1=acme", "GIT_AUTH_TOKEN_1=x", "GIT_AUTH_OWNER_2=", "GIT_AUTH_TOKEN_2=y"},
	} {
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(), append([]string{"GIT_AUTH_HOST=github.com", "GIT_AUTH_COMMAND=true"}, env...)...)
		cmd.Dir = t.TempDir()
		out, err := cmd.CombinedOutput()
		assert.Error(t, err, name)
		assert.Contains(t, string(out), "git-auth-run: slot 2 needs both credential_2 and owner_2", name)
	}
}

func TestGitHubCommitStatusTemplate_TokenSecretParam(t *testing.T) {
	data, err := os.ReadFile("../../templates/github-commit-status.yaml")
	require.NoError(t, err)
	tpl, err := ParseJobTemplate(data)
	require.NoError(t, err)
	var found bool
	for _, in := range tpl.Spec.Params.Inputs {
		if in.Name == "token_secret" {
			found = true
			assert.Equal(t, "string", in.Type)
			assert.Equal(t, "github-token", in.Default)
		}
	}
	assert.True(t, found, "token_secret param missing")
	assert.Equal(t, "{{ index .Secrets .Params.token_secret }}", tpl.Spec.Steps[0].Env["GITHUB_TOKEN"])
}
