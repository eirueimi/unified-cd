package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/spf13/cobra"
)

// This file is the CLI request contract: one table that pins the exact HTTP
// method and path of every request the CLI sends to the controller. Command
// specific tests (apply_test.go, token_test.go, ...) keep asserting bodies,
// authorization and responses; this table is the single place that proves a
// command did not switch verbs (GET vs POST vs DELETE) or endpoints.
//
// Two guards keep the table exhaustive:
//
//   - TestRequestContractCoversEveryCommand walks the real command tree and
//     fails for any leaf command that has no row (and is not listed in
//     noRequestCommands).
//   - TestRequestConstructorInventory counts the request constructions in the
//     package source per file and fails when the count differs from
//     requestConstructors. Adding a constructor therefore forces a developer
//     to touch this file, where the failure message points back at the table.

// contractRow describes one CLI invocation and the exact request sequence it
// must send, in order, as "METHOD /path?query".
type contractRow struct {
	name string
	// leaf is the command path below the root (for example "token create"),
	// used by the coverage guard. Empty for rows that call a helper directly.
	leaf string
	args []string
	// call, when set, is run instead of the root command (for helpers that
	// are only reachable through an interactive flow).
	call func(ctx context.Context) error
	// resp overrides the response body per "METHOD /path" (query stripped).
	resp map[string]string
	// status overrides the response status per "METHOD /path" (query stripped).
	status map[string]int
	want   []string
	// wantErr marks rows where the command legitimately ends in an error after
	// the request under test (for example login stopping at the OIDC probe).
	wantErr bool
}

// noRequestCommands are leaf commands that never contact the controller.
var noRequestCommands = map[string]string{
	"keygen":  "generates a key pair locally",
	"version": "prints the build version",
}

// requestConstructors is the number of request constructions
// (http.NewRequest, http.NewRequestWithContext, http.Get, http.Post, ...)
// per non-test source file. Every one of them must be exercised by a row in
// contractRows. Update this map together with the table.
var requestConstructors = map[string]int{
	"agent_enrollment.go": 1,
	"agent_install.go":    3,
	"apply.go":            1,
	"approvals.go":        1,
	"appsource.go":        4,
	"artifact.go":         2,
	"audit.go":            1,
	"authz.go":            1,
	"export.go":           1,
	"gitcredential.go":    2,
	"jobs.go":             4,
	"login.go":            3,
	"logs.go":             1,
	"run.go":              12,
	"schedule.go":         2,
	"secret.go":           3,
	"token.go":            4,
	"wait.go":             2,
	"webhook.go":          2,
}

const (
	get  = http.MethodGet
	post = http.MethodPost
	del  = http.MethodDelete
	put  = http.MethodPut
)

func contractRows(t *testing.T) []contractRow {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	jobYAML := write("job.yaml", "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: hello\nspec:\n  steps:\n    - name: greet\n      run: echo hi\n")
	gcYAML := write("gc.yaml", "apiVersion: unified-cd/v1\nkind: GitCredential\nmetadata:\n  name: gc\nspec:\n  host: github.com\n  type: token\n  secretRef: gh\n")
	whYAML := write("wh.yaml", "apiVersion: unified-cd/v1\nkind: WebhookReceiver\nmetadata:\n  name: wh\nspec:\n  job: hello\n")
	asYAML := write("as.yaml", "apiVersion: unified-cd/v1\nkind: AppSource\nmetadata:\n  name: as\nspec:\n  repo: https://example.com/r.git\n")
	scYAML := write("sc.yaml", "apiVersion: unified-cd/v1\nkind: Schedule\nmetadata:\n  name: sc\nspec:\n  cron: \"* * * * *\"\n  job: hello\n")
	varsYAML := write("vars.yaml", "apiVersion: unified-cd/v1\nkind: Vars\nmetadata:\n  name: v\nspec:\n  values:\n    A: b\n")
	exportDir := filepath.Join(dir, "export")

	// A real (tiny) artifact archive, so download exercises the whole command.
	srcDir := filepath.Join(dir, "artifact-src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "out.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := artifact.WriteTarZstd(&archive, srcDir); err != nil {
		t.Fatal(err)
	}

	succeeded := map[string]string{"GET /api/v1/runs/r1": `{"id":"r1","status":"Succeeded"}`}

	return []contractRow{
		// apply: one POST per kind, each to its own collection endpoint.
		{name: "apply job", leaf: "apply", args: []string{"apply", "-f", jobYAML}, want: []string{post + " /api/v1/jobs"}},
		{name: "apply gitcredential", leaf: "apply", args: []string{"apply", "-f", gcYAML}, want: []string{post + " /api/v1/gitcredentials"}},
		{name: "apply webhook", leaf: "apply", args: []string{"apply", "-f", whYAML}, want: []string{post + " /api/v1/webhooks/"}},
		{name: "apply appsource", leaf: "apply", args: []string{"apply", "-f", asYAML}, want: []string{post + " /api/v1/appsources"}},
		{name: "apply schedule", leaf: "apply", args: []string{"apply", "-f", scYAML}, want: []string{post + " /api/v1/schedules/"}},
		{name: "apply vars", leaf: "apply", args: []string{"apply", "-f", varsYAML}, want: []string{post + " /api/v1/vars/"}},

		// export: authenticated GETs of every exportable collection.
		{name: "export", leaf: "export", args: []string{"export", "-o", exportDir}, want: []string{
			get + " /api/v1/appsources", get + " /api/v1/jobs", get + " /api/v1/schedules",
			get + " /api/v1/webhooks", get + " /api/v1/gitcredentials", get + " /api/v1/vars",
		}},

		// jobs
		{name: "jobs list", leaf: "jobs list", args: []string{"jobs", "list"}, want: []string{get + " /api/v1/jobs"}},
		{name: "jobs get", leaf: "jobs get", args: []string{"jobs", "get", "j1"}, want: []string{get + " /api/v1/jobs/j1"}},
		{name: "jobs show-yaml", leaf: "jobs show-yaml", args: []string{"jobs", "show-yaml", "j1"}, want: []string{get + " /api/v1/jobs/j1/yaml"}},
		{name: "jobs delete", leaf: "jobs delete", args: []string{"jobs", "delete", "j1"}, want: []string{del + " /api/v1/jobs/j1"}},

		// run
		{name: "run trigger", leaf: "run trigger", args: []string{"run", "trigger", "j1"}, want: []string{post + " /api/v1/runs"}},
		{name: "run trigger --wait", leaf: "run trigger", args: []string{"run", "trigger", "j1", "--wait"},
			resp: map[string]string{"POST /api/v1/runs": `{"id":"r1","status":"Queued"}`, "GET /api/v1/runs/r1": `{"id":"r1","status":"Succeeded"}`},
			want: []string{post + " /api/v1/runs", get + " /api/v1/runs/r1"}},
		{name: "run trigger --output", leaf: "run trigger", args: []string{"run", "trigger", "j1", "--output", "k"},
			resp: map[string]string{"POST /api/v1/runs": `{"id":"r1","status":"Queued"}`, "GET /api/v1/runs/r1": `{"id":"r1","status":"Succeeded"}`, "GET /api/v1/runs/r1/outputs": `{"outputs":{"k":"v"}}`},
			want: []string{post + " /api/v1/runs", get + " /api/v1/runs/r1", get + " /api/v1/runs/r1/outputs"}},
		{name: "run replay", leaf: "run replay", args: []string{"run", "replay", "r1"}, want: []string{post + " /api/v1/runs/r1/replay"}},
		{name: "run wait", leaf: "run wait", args: []string{"run", "wait", "r1"}, resp: succeeded, want: []string{get + " /api/v1/runs/r1"}},
		{name: "run wait --follow", leaf: "run wait", args: []string{"run", "wait", "r1", "--follow"}, resp: succeeded,
			want: []string{get + " /api/v1/runs/r1/logs?after=0", get + " /api/v1/runs/r1", get + " /api/v1/runs/r1/logs?after=0"}},
		{name: "run list", leaf: "run list", args: []string{"run", "list", "--job", "j1"}, want: []string{get + " /api/v1/runs?jobName=j1"}},
		{name: "run show", leaf: "run show", args: []string{"run", "show", "r1"}, resp: succeeded,
			want: []string{get + " /api/v1/runs/r1", get + " /api/v1/runs/r1/steps"}},
		{name: "run delete", leaf: "run delete", args: []string{"run", "delete", "r1"}, want: []string{del + " /api/v1/runs/r1"}},
		{name: "run list-active", leaf: "run list-active", args: []string{"run", "list-active"}, want: []string{get + " /api/v1/runs/active"}},
		{name: "run outputs", leaf: "run outputs", args: []string{"run", "outputs", "r1"}, want: []string{get + " /api/v1/runs/r1/outputs"}},
		{name: "run show-yaml", leaf: "run show-yaml", args: []string{"run", "show-yaml", "r1"}, want: []string{get + " /api/v1/runs/r1/yaml"}},
		{name: "run approvals", leaf: "run approvals", args: []string{"run", "approvals", "r1"}, want: []string{get + " /api/v1/runs/r1/approvals"}},
		{name: "run cancel", leaf: "run cancel", args: []string{"run", "cancel", "r1"}, want: []string{post + " /api/v1/runs/r1/cancel"}},

		// logs
		{name: "logs", leaf: "logs", args: []string{"logs", "r1"}, resp: succeeded,
			want: []string{get + " /api/v1/runs/r1/logs?after=0", get + " /api/v1/runs/r1"}},
		{name: "logs --step", leaf: "logs", args: []string{"logs", "r1", "--step"}, resp: succeeded,
			want: []string{get + " /api/v1/runs/r1/steps", get + " /api/v1/runs/r1/logs?after=0", get + " /api/v1/runs/r1"}},

		// approvals
		{name: "approve", leaf: "approve", args: []string{"approve", "r1", "2"}, want: []string{post + " /api/v1/runs/r1/approvals/2"}},
		{name: "reject", leaf: "reject", args: []string{"reject", "r1", "2"}, want: []string{post + " /api/v1/runs/r1/approvals/2"}},

		// artifacts
		{name: "artifact list", leaf: "artifact list", args: []string{"artifact", "list", "r1"}, want: []string{get + " /api/v1/runs/r1/artifacts"}},
		{name: "artifact download", leaf: "artifact download", args: []string{"artifact", "download", "r1", "out.txt", "--dest", filepath.Join(dir, "artifact-dst")},
			resp: map[string]string{"GET /api/v1/runs/r1/artifacts/out.txt": archive.String()},
			want: []string{get + " /api/v1/runs/r1/artifacts/out.txt"}},

		// agents
		{name: "agent list", leaf: "agent list", args: []string{"agent", "list"}, want: []string{get + " /api/v1/agents"}},
		{name: "agent get", leaf: "agent get", args: []string{"agent", "get", "a1"}, want: []string{get + " /api/v1/agents/a1"}},
		{name: "agent runs", leaf: "agent runs", args: []string{"agent", "runs", "a1"}, want: []string{get + " /api/v1/agents/a1/runs"}},
		{name: "agent enrollment create", leaf: "agent enrollment create", args: []string{"agent", "enrollment", "create"},
			resp: map[string]string{"POST /api/v1/agent-enrollments": `{"id":"e1","token":"t"}`}, status: map[string]int{"POST /api/v1/agent-enrollments": 201},
			want: []string{post + " /api/v1/agent-enrollments"}},
		{name: "agent enrollment list", leaf: "agent enrollment list", args: []string{"agent", "enrollment", "list"}, want: []string{get + " /api/v1/agent-enrollments"}},
		{name: "agent enrollment revoke", leaf: "agent enrollment revoke", args: []string{"agent", "enrollment", "revoke", "e1"},
			status: map[string]int{"DELETE /api/v1/agent-enrollments/e1": 204}, want: []string{del + " /api/v1/agent-enrollments/e1"}},
		{name: "agent enrollment-policy create", leaf: "agent enrollment-policy create",
			args:   []string{"agent", "enrollment-policy", "create", "p1", "--cluster", "c", "--namespace", "n", "--service-account", "s"},
			status: map[string]int{"POST /api/v1/agent-enrollment-policies": 201}, want: []string{post + " /api/v1/agent-enrollment-policies"}},
		{name: "agent enrollment-policy update", leaf: "agent enrollment-policy update",
			args: []string{"agent", "enrollment-policy", "update", "p1", "--cluster", "c", "--namespace", "n", "--service-account", "s"},
			want: []string{put + " /api/v1/agent-enrollment-policies/p1"}},
		{name: "agent enrollment-policy get", leaf: "agent enrollment-policy get", args: []string{"agent", "enrollment-policy", "get", "p1"},
			want: []string{get + " /api/v1/agent-enrollment-policies/p1"}},
		{name: "agent enrollment-policy list", leaf: "agent enrollment-policy list", args: []string{"agent", "enrollment-policy", "list"},
			want: []string{get + " /api/v1/agent-enrollment-policies"}},
		{name: "agent enrollment-policy delete", leaf: "agent enrollment-policy delete", args: []string{"agent", "enrollment-policy", "delete", "p1"},
			status: map[string]int{"DELETE /api/v1/agent-enrollment-policies/p1": 204}, want: []string{del + " /api/v1/agent-enrollment-policies/p1"}},
		{name: "agent identity get", leaf: "agent identity get", args: []string{"agent", "identity", "get", "a1"}, want: []string{get + " /api/v1/agent-identities/a1"}},
		{name: "agent identity enable", leaf: "agent identity enable", args: []string{"agent", "identity", "enable", "a1"},
			status: map[string]int{"POST /api/v1/agent-identities/a1/enable": 204}, want: []string{post + " /api/v1/agent-identities/a1/enable"}},
		{name: "agent identity disable", leaf: "agent identity disable", args: []string{"agent", "identity", "disable", "a1"},
			status: map[string]int{"POST /api/v1/agent-identities/a1/disable": 204}, want: []string{post + " /api/v1/agent-identities/a1/disable"}},
		{name: "agent identity revoke-credentials", leaf: "agent identity revoke-credentials", args: []string{"agent", "identity", "revoke-credentials", "a1"},
			status: map[string]int{"POST /api/v1/agent-identities/a1/credentials/revoke": 204}, want: []string{post + " /api/v1/agent-identities/a1/credentials/revoke"}},

		// secrets
		{name: "secret set", leaf: "secret set", args: []string{"secret", "set", "s1", "v"},
			status: map[string]int{"POST /api/v1/secrets/": 204}, want: []string{post + " /api/v1/secrets/"}},
		{name: "secret list", leaf: "secret list", args: []string{"secret", "list"}, want: []string{get + " /api/v1/secrets/"}},
		{name: "secret delete", leaf: "secret delete", args: []string{"secret", "delete", "s1"}, want: []string{del + " /api/v1/secrets/s1"}},

		// git credentials
		{name: "gitcredential list", leaf: "gitcredential list", args: []string{"gitcredential", "list"}, want: []string{get + " /api/v1/gitcredentials"}},
		{name: "gitcredential delete", leaf: "gitcredential delete", args: []string{"gitcredential", "delete", "g1"}, want: []string{del + " /api/v1/gitcredentials/g1"}},

		// schedules
		{name: "schedule list", leaf: "schedule list", args: []string{"schedule", "list"}, want: []string{get + " /api/v1/schedules/"}},
		{name: "schedule delete", leaf: "schedule delete", args: []string{"schedule", "delete", "s1"}, want: []string{del + " /api/v1/schedules/s1"}},

		// webhooks
		{name: "webhook list", leaf: "webhook list", args: []string{"webhook", "list"}, want: []string{get + " /api/v1/webhooks/"}},
		{name: "webhook delete", leaf: "webhook delete", args: []string{"webhook", "delete", "w1"}, want: []string{del + " /api/v1/webhooks/w1"}},

		// app sources
		{name: "appsource sync", leaf: "appsource sync", args: []string{"appsource", "sync", "a1"},
			status: map[string]int{"POST /api/v1/appsources/a1/sync": 204}, want: []string{post + " /api/v1/appsources/a1/sync"}},
		{name: "appsource list", leaf: "appsource list", args: []string{"appsource", "list"}, want: []string{get + " /api/v1/appsources"}},
		{name: "appsource get", leaf: "appsource get", args: []string{"appsource", "get", "a1"}, want: []string{get + " /api/v1/appsources/a1"}},
		{name: "appsource delete", leaf: "appsource delete", args: []string{"appsource", "delete", "a1"}, want: []string{del + " /api/v1/appsources/a1"}},

		// tokens
		{name: "token create", leaf: "token create", args: []string{"token", "create", "t1"},
			resp: map[string]string{"POST /api/v1/tokens": `{"id":"id1","name":"t1","token":"secret"}`}, want: []string{post + " /api/v1/tokens"}},
		{name: "token create --permission", leaf: "token create", args: []string{"token", "create", "t1", "--permission", "runs.read"},
			resp: map[string]string{"POST /api/v1/tokens": `{"id":"id1","name":"t1","token":"secret","permissions":["runs.read"]}`},
			want: []string{post + " /api/v1/tokens"}},
		{name: "token create --permission against an older controller revokes", leaf: "token create",
			args: []string{"token", "create", "t1", "--permission", "runs.read"}, wantErr: true,
			resp: map[string]string{"POST /api/v1/tokens": `{"id":"id1","name":"t1","token":"secret"}`},
			want: []string{post + " /api/v1/tokens", del + " /api/v1/tokens/id1"}},
		{name: "token list", leaf: "token list", args: []string{"token", "list"}, want: []string{get + " /api/v1/tokens"}},
		{name: "token delete", leaf: "token delete", args: []string{"token", "delete", "id1"}, want: []string{del + " /api/v1/tokens/id1"}},

		// audit
		{name: "audit list", leaf: "audit list", args: []string{"audit", "list"}, want: []string{get + " /api/v1/audit"}},

		// authz (#189)
		{name: "authz show", leaf: "authz show", args: []string{"authz", "show"}, want: []string{get + " /api/v1/authz/policy"}},
		{name: "authz explain", leaf: "authz explain", args: []string{"authz", "explain", "--user", "u@example.com"},
			want: []string{post + " /api/v1/authz/explain"}},

		// login: the helpers below are reachable only through interactive or
		// device flows, so they are called directly (leaf covers the command).
		{name: "login probes the OIDC config", leaf: "login", args: []string{"login", "--server", "http://controller.test"}, wantErr: true,
			status: map[string]int{"GET /api/v1/auth/oidc-config": 500}, want: []string{get + " /api/v1/auth/oidc-config"}},
		{name: "login verifies a pasted token", call: func(ctx context.Context) error {
			return verifyToken(ctx, "http://controller.test", "tok")
		}, want: []string{get + " /api/v1/tokens"}},
		{name: "login discovers the OIDC provider", wantErr: true, call: func(ctx context.Context) error {
			_, err := discoverOIDCEndpoint("http://issuer.test")
			return err
		}, status: map[string]int{"GET /.well-known/openid-configuration": 500}, want: []string{get + " /.well-known/openid-configuration"}},
	}
}

// contractTransport records "METHOD RequestURI" and answers from the row.
type contractTransport struct {
	row  contractRow
	seen []string
}

var collectionSegments = map[string]bool{
	"agents": true, "jobs": true, "runs": true, "active": true, "schedules": true, "webhooks": true,
	"gitcredentials": true, "appsources": true, "tokens": true, "secrets": true, "artifacts": true,
	"approvals": true, "steps": true, "logs": true, "audit": true, "vars": true,
	"agent-enrollments": true, "agent-enrollment-policies": true,
}

func (c *contractTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
	}
	c.seen = append(c.seen, req.Method+" "+req.URL.RequestURI())
	key := req.Method + " " + req.URL.Path
	status := http.StatusOK
	if s, ok := c.row.status[key]; ok {
		status = s
	}
	body, ok := c.row.resp[key]
	if !ok {
		body = "{}"
		segs := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
		if req.Method == get && collectionSegments[segs[len(segs)-1]] {
			body = "[]"
		}
	}
	if _, ok := c.row.status[key]; !ok && req.Method == del {
		status = http.StatusNoContent
	}
	if status == http.StatusNoContent {
		body = ""
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}

func TestRequestContract(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	for _, row := range contractRows(t) {
		t.Run(row.name, func(t *testing.T) {
			tr := &contractTransport{row: row}
			prev := http.DefaultClient.Transport
			http.DefaultClient.Transport = tr
			t.Cleanup(func() { http.DefaultClient.Transport = prev })

			var err error
			if row.call != nil {
				err = row.call(context.Background())
			} else {
				root := NewRoot()
				var out bytes.Buffer
				root.SetOut(&out)
				root.SetErr(&out)
				root.SetArgs(append([]string{"--config", cfgPath, "--server", "http://controller.test", "--token", "tok"}, row.args...))
				err = root.Execute()
			}
			if row.wantErr && err == nil {
				t.Errorf("expected the command to end in an error")
			}
			if !row.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if strings.Join(tr.seen, "\n") != strings.Join(row.want, "\n") {
				t.Errorf("requests mismatch\n got: %q\nwant: %q", tr.seen, row.want)
			}
		})
	}
}

// leafCommands collects the command paths (below the root) of every leaf.
func leafCommands(c *cobra.Command, prefix string, out *[]string) {
	for _, sub := range c.Commands() {
		name := strings.TrimSpace(prefix + " " + sub.Name())
		if sub.Name() == "help" || sub.Name() == "completion" {
			continue
		}
		if len(sub.Commands()) > 0 {
			leafCommands(sub, name, out)
			continue
		}
		*out = append(*out, name)
	}
}

func TestRequestContractCoversEveryCommand(t *testing.T) {
	covered := map[string]bool{}
	for _, row := range contractRows(t) {
		if row.leaf != "" {
			covered[row.leaf] = true
		}
	}
	var leaves []string
	leafCommands(NewRoot(), "", &leaves)
	if len(leaves) == 0 {
		t.Fatal("found no commands")
	}
	isLeaf := map[string]bool{}
	for _, leaf := range leaves {
		isLeaf[leaf] = true
		if covered[leaf] {
			continue
		}
		if _, ok := noRequestCommands[leaf]; ok {
			continue
		}
		t.Errorf("command %q has no row in contractRows (request_contract_test.go); add its method+path pair, or list it in noRequestCommands if it never calls the controller", leaf)
	}
	for leaf := range covered {
		if !isLeaf[leaf] {
			t.Errorf("contractRows names %q, which is not a command", leaf)
		}
	}
}

var requestConstructorRE = regexp.MustCompile(`\bhttp\.(NewRequestWithContext|NewRequest|Get|Post|PostForm|Head)\(`)

func TestRequestConstructorInventory(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(requestConstructorRE.FindAll(b, -1)); n > 0 {
			got[f] = n
		}
	}
	var names []string
	for f := range got {
		names = append(names, f)
	}
	for f := range requestConstructors {
		if _, ok := got[f]; !ok {
			names = append(names, f)
		}
	}
	sort.Strings(names)
	for _, f := range names {
		if got[f] != requestConstructors[f] {
			t.Errorf("%s builds %d HTTP requests but requestConstructors expects %d: add or remove the matching rows in contractRows (request_contract_test.go), then update requestConstructors",
				f, got[f], requestConstructors[f])
		}
	}
}
