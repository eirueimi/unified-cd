package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_WebhookReceiverCRUD(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	spec, _ := json.Marshal(map[string]any{"trigger": map[string]any{"job": "build"}})
	wr, err := pg.UpsertWebhookReceiver(ctx, "github-push", spec)
	require.NoError(t, err)
	assert.Equal(t, "github-push", wr.Name)

	got, err := pg.GetWebhookReceiver(ctx, "github-push")
	require.NoError(t, err)
	assert.Equal(t, wr.ID, got.ID)

	list, err := pg.ListWebhookReceivers(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	require.NoError(t, pg.DeleteWebhookReceiver(ctx, "github-push"))
	_, err = pg.GetWebhookReceiver(ctx, "github-push")
	require.Error(t, err)
}

func TestPostgres_CreateRun_TriggeredBy(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "webhook:github-push")
	require.NoError(t, err)
	assert.Equal(t, "webhook:github-push", run.TriggeredBy)

	got, err := pg.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, "webhook:github-push", got.TriggeredBy)
	_ = api.RunPending // keep import
}
