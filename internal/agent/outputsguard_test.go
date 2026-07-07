package agent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/secrets"
	"github.com/stretchr/testify/assert"
)

func TestFilterSecretOutputs_SkipsSecretValues(t *testing.T) {
	m := secrets.NewMasker([]string{"s3cr3t-value"})
	var skipped []string
	out := FilterSecretOutputs(map[string]string{
		"clean": "hello",
		"leaky": "token=s3cr3t-value",
	}, m, func(k string) { skipped = append(skipped, k) })
	assert.Equal(t, map[string]string{"clean": "hello"}, out)
	assert.Equal(t, []string{"leaky"}, skipped)
}

func TestFilterSecretOutputs_EncodedVariantAlsoSkipped(t *testing.T) {
	m := secrets.NewMasker([]string{"s3cr3t"})
	var skipped []string
	// base64("s3cr3t") = "czNjcjN0"
	out := FilterSecretOutputs(map[string]string{"b64": "czNjcjN0"}, m,
		func(k string) { skipped = append(skipped, k) })
	assert.Empty(t, out)
	assert.Equal(t, []string{"b64"}, skipped)
}

func TestFilterSecretOutputs_NoOpMaskerPassesThrough(t *testing.T) {
	out := FilterSecretOutputs(map[string]string{"k": "anything"}, secrets.NoOpMasker,
		func(k string) { t.Fatalf("unexpected skip: %s", k) })
	assert.Equal(t, map[string]string{"k": "anything"}, out)
}

func TestFilterSecretOutputs_NilMaskerPassesThrough(t *testing.T) {
	out := FilterSecretOutputs(map[string]string{"k": "v"}, nil,
		func(k string) { t.Fatalf("unexpected skip: %s", k) })
	assert.Equal(t, map[string]string{"k": "v"}, out)
}

func TestFilterSecretOutputs_DoesNotMutateInput(t *testing.T) {
	m := secrets.NewMasker([]string{"s3cr3t"})
	in := map[string]string{"leaky": "s3cr3t", "clean": "ok"}
	_ = FilterSecretOutputs(in, m, nil)
	assert.Equal(t, map[string]string{"leaky": "s3cr3t", "clean": "ok"}, in)
}
