package controller

import (
	"context"
	"testing"

	"github.com/eirueimi/unified-cd/internal/store"
)

func TestApplyResource_EachKind(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()
	cases := []struct {
		kind, wantName, doc string
	}{
		{"Job", "j1", "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j1\nspec:\n  agentSelector: [kind:docker]\n  steps:\n    - name: s\n      run: echo hi"},
		{"Schedule", "sc1", "apiVersion: unified-cd/v1\nkind: Schedule\nmetadata:\n  name: sc1\nspec:\n  cron: \"* * * * *\"\n  job: j1"},
		{"WebhookReceiver", "wh1", "apiVersion: unified-cd/v1\nkind: WebhookReceiver\nmetadata:\n  name: wh1\nspec:\n  trigger:\n    job: j1\n  auth:\n    type: none"},
		{"GitCredential", "gc1", "apiVersion: unified-cd/v1\nkind: GitCredential\nmetadata:\n  name: gc1\nspec:\n  host: github.com\n  type: token\n  secretRef: s"},
		{"AppSource", "as1", "apiVersion: unified-cd/v1\nkind: AppSource\nmetadata:\n  name: as1\nspec:\n  repoURL: https://x/y\n  targetRevision: main\n  path: jobs"},
	}
	for _, c := range cases {
		got, err := applyResource(ctx, pg, c.kind, []byte(c.doc))
		if err != nil {
			t.Fatalf("%s: applyResource error: %v", c.kind, err)
		}
		if got != c.wantName {
			t.Errorf("%s: name = %q, want %q", c.kind, got, c.wantName)
		}
	}
}

func TestApplyResource_UnknownAndBad(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()
	if _, err := applyResource(ctx, pg, "Nope", []byte("kind: Nope")); err == nil {
		t.Error("unknown kind: expected error")
	}
	if _, err := applyResource(ctx, pg, "Job", []byte("kind: Job\nmetadata: {name: x}\nspec: {steps: []}")); err == nil {
		t.Error("invalid Job: expected error")
	}
}

func TestProbeKind(t *testing.T) {
	if k := probeKind([]byte("kind: Schedule\nmetadata: {name: x}")); k != "Schedule" {
		t.Errorf("probeKind = %q, want Schedule", k)
	}
	if k := probeKind([]byte("metadata: {name: x}")); k != "" {
		t.Errorf("probeKind (no kind) = %q, want empty", k)
	}
}
