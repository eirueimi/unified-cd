package controller

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
	"github.com/eirueimi/unified-cd/internal/store"
)

// A Kubernetes agent whose row a heartbeat recreated (so capabilities are
// NULL) gets pod back on its next claim poll and can then take a pod run. An
// older agent that sends no capabilities leaves the column as it was.
func TestAgentAPI_Claim_RestoresCapabilitiesOnARecreatedRow(t *testing.T) {
	s, pg := newTestServer(t)
	const podSpec = `{"podTemplate":{"spec":{"containers":[{"name":"c","image":"alpine"}],"volumes":[{"name":"v","emptyDir":{}}]}}}`
	_, err := pg.UpsertJob(t.Context(), "pj", "unified-cd/v1", []byte(podSpec))
	require.NoError(t, err)
	run, err := pg.CreateRun(t.Context(), "pj", nil, []byte(podSpec), nil, dsl.RequiredCaps(mustSpec(t, podSpec)), "", "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(t.Context(), 10)
	require.NoError(t, err)

	token := issueAgentAccessForTest(t, pg, "k8s", nil, nil)
	// The agent registered, then a sibling's deregistration removed its row and
	// its heartbeat recreated a minimal one.
	require.NoError(t, pg.UpsertAgent(t.Context(), "k8s", "h", "linux/k8s", "", nil, []string{dsl.CapPod, dsl.CapContainer}, nil, nil))
	require.NoError(t, pg.DeleteAgent(t.Context(), "k8s"))
	_, err = pg.TouchAgent(t.Context(), "k8s")
	require.NoError(t, err)

	// A claim without a body (an older agent) neither restores nor claims.
	rec := claimAs(t, s, token, "k8s", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got api.ClaimResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Empty(t, got.RunID, "a row without capabilities must not be handed a pod run")
	info, err := pg.GetAgent(t.Context(), "k8s")
	require.NoError(t, err)
	assert.Empty(t, info.Capabilities, "a claim that supplies none must leave the column as it was")

	// The next poll of a current agent carries the set.
	body, _ := json.Marshal(api.ClaimRequest{Capabilities: []string{dsl.CapPod, dsl.CapContainer}})
	rec = claimAs(t, s, token, "k8s", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, run.ID, got.RunID)
	info, err = pg.GetAgent(t.Context(), "k8s")
	require.NoError(t, err)
	// Stored normalized: pod implies securitycontext.
	assert.ElementsMatch(t, []string{dsl.CapPod, dsl.CapContainer, dsl.CapSecurityContext}, info.Capabilities)
}

// A capability outside the vocabulary is rejected on a claim just as it is at
// registration, rather than stored where it would silently never match.
func TestAgentAPI_Claim_RejectsAnUnknownCapability(t *testing.T) {
	s, pg := newTestServer(t)
	token := issueAgentAccessForTest(t, pg, "a", nil, nil)
	body, _ := json.Marshal(api.ClaimRequest{Capabilities: []string{dsl.CapNative, "gpu"}})
	rec := claimAs(t, s, token, "a", body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "unknown capability: gpu")
}

func mustSpec(t *testing.T, raw string) dsl.Spec {
	t.Helper()
	var sp dsl.Spec
	require.NoError(t, json.Unmarshal([]byte(raw), &sp))
	return sp
}

// The resolver fills podTemplate from fetched templates while the run is
// Pending, after required_caps was fixed from the spec as submitted. A template
// that contributes a pod-level volume makes the run Kubernetes-only, so a host
// agent must not be able to claim it once resolution has run.
func TestResolveGitPendingRuns_TemplateThatNeedsKubernetesMovesTheRunToPod(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := t.Context()
	require.NoError(t, pg.UpsertAgent(ctx, "host", "h", "linux", "", nil, []string{dsl.CapNative, dsl.CapContainer}, nil, nil))
	require.NoError(t, pg.UpsertAgent(ctx, "k8s", "k", "linux/k8s", "", nil, []string{dsl.CapPod, dsl.CapContainer}, nil, nil))
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))

	submitted := []byte(`{"steps":[{"name":"tpl","uses":{"job":"git://github.com/org/repo/job.yaml@v1"}}]}`)
	require.Equal(t, []string{dsl.CapContainer}, dsl.RequiredCaps(mustSpec(t, string(submitted))))
	run, err := pg.CreateRun(ctx, "j", nil, submitted, nil, []string{dsl.CapContainer}, "", "")
	require.NoError(t, err)

	tmpl := []byte("apiVersion: unified-cd/v1\nkind: JobTemplate\nmetadata: {name: tmpl}\nspec:\n  podTemplate:\n    spec:\n      containers: [{name: tools, image: alpine:3, volumeMounts: [{name: cache, mountPath: /c}]}]\n      volumes: [{name: cache, emptyDir: {}}]\n  steps:\n    - {name: s, container: tools, run: echo hi}\n")
	resolver := gittemplate.NewResolver(contentFetcher{yaml: tmpl}, nil)
	bo := newFailureBackoff(time.Minute, time.Hour, 10_000)
	resolveGitPendingRuns(ctx, pg, resolver, nil, bo, time.Hour)

	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)

	got, err := pg.ClaimNextRun(ctx, "host", nil, nil)
	require.NoError(t, err)
	assert.Nil(t, got, "a host agent must not claim a run whose resolved spec needs Kubernetes")
	got, err = pg.ClaimNextRun(ctx, "k8s", nil, nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, run.ID, got.ID)
}

// A template that adds only host-runnable containers changes nothing about
// where the run may go, so a host agent still claims it.
func TestResolveGitPendingRuns_HostRunnableTemplateStaysClaimableByAHostAgent(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := t.Context()
	require.NoError(t, pg.UpsertAgent(ctx, "host", "h", "linux", "", nil, []string{dsl.CapNative, dsl.CapContainer}, nil, nil))
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))

	submitted := []byte(`{"steps":[{"name":"tpl","uses":{"job":"git://github.com/org/repo/job.yaml@v1"}}]}`)
	run, err := pg.CreateRun(ctx, "j", nil, submitted, nil, []string{dsl.CapContainer}, "", "")
	require.NoError(t, err)

	tmpl := []byte("apiVersion: unified-cd/v1\nkind: JobTemplate\nmetadata: {name: tmpl}\nspec:\n  podTemplate:\n    spec:\n      containers: [{name: tools, image: alpine:3}]\n  steps:\n    - {name: s, container: tools, run: echo hi}\n")
	resolver := gittemplate.NewResolver(contentFetcher{yaml: tmpl}, nil)
	bo := newFailureBackoff(time.Minute, time.Hour, 10_000)
	resolveGitPendingRuns(ctx, pg, resolver, nil, bo, time.Hour)

	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	got, err := pg.ClaimNextRun(ctx, "host", nil, nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, run.ID, got.ID)
}
