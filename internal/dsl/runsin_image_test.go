package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunsIn_ImageReferenceValidated(t *testing.T) {
	mk := func(image string) string {
		return "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  steps:\n    - name: s\n      uses: { job: \"git://example.com/x/tpl.yaml@main\" }\n      runsIn:\n        image: " + image + "\n"
	}
	for _, bad := range []string{`"--privileged"`, `"-v"`, `"a b"`, `"alpine\nx"`} {
		_, err := Parse(strings.NewReader(mk(bad)))
		require.Error(t, err, bad)
		assert.Contains(t, err.Error(), "runsIn.image", bad)
	}
	for _, good := range []string{"alpine", "localhost:5000/team/app:tag", "ghcr.io/o/i:v1@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"} {
		_, err := Parse(strings.NewReader(mk(good)))
		require.NoError(t, err, good)
	}
}
