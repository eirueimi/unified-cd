package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseWebhookReceiver_Minimal(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: github-push
spec:
  trigger:
    job: build-and-deploy
  auth:
    type: github
    secretRef: github-webhook-secret
  paramsMapping:
    commit_sha: '{{ index .Payload "after" }}'
  filters:
    - '{{ eq (index .Payload "ref") "refs/heads/main" }}'
`
	wr, err := ParseWebhookReceiver(strings.NewReader(input))
	require.NoError(t, err)
	assert.Equal(t, "github-push", wr.Metadata.Name)
	assert.Equal(t, "build-and-deploy", wr.Spec.Trigger.Job)
	assert.Equal(t, "github", wr.Spec.Auth.Type)
	assert.Equal(t, "github-webhook-secret", wr.Spec.Auth.SecretRef)
	assert.Contains(t, wr.Spec.ParamsMapping["commit_sha"], "after")
	require.Len(t, wr.Spec.Filters, 1)
}

func TestParseWebhookReceiver_RejectsMissingJob(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: x
spec:
  trigger: {}
  auth:
    type: none
`
	_, err := ParseWebhookReceiver(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "trigger.job")
}

func TestWebhookTemplate_ExpandPayload(t *testing.T) {
	tpl := `{{ index .Payload "ref" }}`
	data := WebhookTemplateData{
		Payload: map[string]any{"ref": "refs/heads/main"},
	}
	result, err := ExpandWebhookTemplate(tpl, data)
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/main", result)
}

func TestWebhookTemplate_FilterTrue(t *testing.T) {
	tpl := `{{ eq (index .Payload "ref") "refs/heads/main" }}`
	data := WebhookTemplateData{
		Payload: map[string]any{"ref": "refs/heads/main"},
	}
	result, err := ExpandWebhookTemplate(tpl, data)
	require.NoError(t, err)
	assert.Equal(t, "true", result)
}

func TestWebhookTemplate_FilterFalse(t *testing.T) {
	tpl := `{{ eq (index .Payload "ref") "refs/heads/main" }}`
	data := WebhookTemplateData{
		Payload: map[string]any{"ref": "refs/heads/feature"},
	}
	result, err := ExpandWebhookTemplate(tpl, data)
	require.NoError(t, err)
	assert.Equal(t, "false", result)
}

func TestParseWebhookReceiver_RejectsInvalidNameFormat(t *testing.T) {
	input := `
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: Github_Push
spec:
  trigger:
    job: build-and-deploy
`
	_, err := ParseWebhookReceiver(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metadata.name is invalid")
}
