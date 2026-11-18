package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandImage(t *testing.T) {
	params := map[string]string{"TAG": "1.2"}
	vars := map[string]string{"GO_IMAGE": "golang:1.22", "REG": "reg.io"}

	got, err := ExpandImage("{{ .Vars.GO_IMAGE }}", params, vars)
	require.NoError(t, err)
	assert.Equal(t, "golang:1.22", got)

	got, err = ExpandImage("app:{{ .Params.TAG }}", params, vars)
	require.NoError(t, err)
	assert.Equal(t, "app:1.2", got)

	got, err = ExpandImage("{{ .Vars.REG }}/app:{{ .Params.TAG }}", params, vars)
	require.NoError(t, err)
	assert.Equal(t, "reg.io/app:1.2", got)

	got, err = ExpandImage("alpine:3", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "alpine:3", got)
}

func TestExpandImage_Errors(t *testing.T) {
	vars := map[string]string{"A": "x y", "E": ""}
	for name, tpl := range map[string]string{
		"missing key":   "{{ .Vars.NOPE }}",
		"secrets":       "{{ .Secrets.X }}",
		"steps":         "{{ .Steps.x }}",
		"parse":         "{{ .Vars.A",
		"space result":  "{{ .Vars.A }}",
		"empty result":  "{{ .Vars.E }}",
		"missing param": "{{ .Params.P }}",
	} {
		_, err := ExpandImage(tpl, map[string]string{}, vars)
		assert.Error(t, err, name)
	}
	_, err := ExpandImage("{{ .Vars.NOPE }}", nil, vars)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NOPE")
}

func TestValidateImageTemplate(t *testing.T) {
	assert.NoError(t, ValidateImageTemplate("{{ .Vars.X }}"))
	assert.NoError(t, ValidateImageTemplate("alpine"))
	assert.Error(t, ValidateImageTemplate("{{ .Vars.X"))
	assert.Error(t, ValidateImageTemplate("a b"))
	assert.Error(t, ValidateImageTemplate("--privileged"))
}

func TestParse_ImageTemplates(t *testing.T) {
	uses := func(image string) string {
		return "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  steps:\n    - name: s\n      uses: { job: \"git://example.com/x/tpl.yaml@main\" }\n      runsIn:\n        image: " + image + "\n"
	}
	_, err := Parse(strings.NewReader(uses(`"{{ .Vars.X }}"`)))
	require.NoError(t, err)
	_, err = Parse(strings.NewReader(uses(`"{{ .Vars.X"`)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runsIn.image")

	pod := func(image string) string {
		return "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  podTemplate:\n    spec:\n      containers:\n        - name: job\n          image: " + image + "\n  steps:\n    - name: s\n      run: echo\n"
	}
	_, err = Parse(strings.NewReader(pod(`"{{ .Vars.X }}"`)))
	require.NoError(t, err)
	_, err = Parse(strings.NewReader(pod(`"{{ .Vars.X"`)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "image")
}

func TestExpandPodTemplateImages(t *testing.T) {
	pt := &PodTemplate{
		Spec: map[string]any{
			"containers":     []any{map[string]any{"name": "build", "image": "{{ .Vars.G }}"}, map[string]any{"name": "db", "image": "pg:16"}},
			"initContainers": []any{map[string]any{"name": "i", "image": "{{ .Params.T }}"}},
		},
		Override: &PodSpecPatch{Containers: []map[string]any{{"name": "o", "image": "{{ .Vars.G }}-o"}}},
	}
	got, err := ExpandPodTemplateImages(pt, map[string]string{"T": "t:1"}, map[string]string{"G": "go:1"})
	require.NoError(t, err)
	cs := got.Spec["containers"].([]any)
	assert.Equal(t, "go:1", cs[0].(map[string]any)["image"])
	assert.Equal(t, "pg:16", cs[1].(map[string]any)["image"])
	assert.Equal(t, "t:1", got.Spec["initContainers"].([]any)[0].(map[string]any)["image"])
	assert.Equal(t, "go:1-o", got.Override.Containers[0]["image"])
	// input untouched
	assert.Equal(t, "{{ .Vars.G }}", pt.Spec["containers"].([]any)[0].(map[string]any)["image"])
	assert.Equal(t, "{{ .Params.T }}", pt.Spec["initContainers"].([]any)[0].(map[string]any)["image"])
	assert.Equal(t, "{{ .Vars.G }}-o", pt.Override.Containers[0]["image"])

	_, err = ExpandPodTemplateImages(pt, nil, map[string]string{"G": "go:1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `podTemplate initContainer "i" image "{{ .Params.T }}"`)

	got, err = ExpandPodTemplateImages(nil, nil, nil)
	require.NoError(t, err)
	assert.Nil(t, got)
}
