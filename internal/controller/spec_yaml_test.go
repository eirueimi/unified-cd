package controller

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"gopkg.in/yaml.v3"
)

// TestSpecJSONToYAML_MatrixForeachFidelity pins that the job-YAML endpoint
// renders matrix/foreach in the authorable mapping form (via the MarshalYAML
// implementations on MatrixDef/ForeachSource), not the raw struct shape
// ("dimensions:", "literal:") that the strict parser rejects.
func TestSpecJSONToYAML_MatrixForeachFidelity(t *testing.T) {
	const jobYAML = `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: matrix-build
spec:
  steps:
    - name: build
      matrix:
        os: [linux, windows]
        exclude:
          - os: windows
      run: echo build
    - name: deploy
      foreach:
        key: env
        in: "{{ .Params.envs }}"
      run: echo deploy
`
	job, err := dsl.Parse(strings.NewReader(jobYAML))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	specJSON, err := json.Marshal(job.Spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}

	out, err := specJSONToYAML(specJSON)
	if err != nil {
		t.Fatalf("specJSONToYAML: %v", err)
	}
	s := string(out)
	for _, banned := range []string{"dimensions:", "literal:", "expr:"} {
		if strings.Contains(s, banned) {
			t.Errorf("rendered yaml leaks struct-shaped key %q:\n%s", banned, s)
		}
	}

	// The rendered spec must strict-decode back into dsl.Spec with the
	// matrix/foreach content intact.
	dec := yaml.NewDecoder(strings.NewReader(s))
	dec.KnownFields(true)
	var spec dsl.Spec
	if err := dec.Decode(&spec); err != nil {
		t.Fatalf("rendered spec must strict-decode: %v\n%s", err, s)
	}
	if spec.Steps[0].Matrix == nil || len(spec.Steps[0].Matrix.Dimensions) != 1 ||
		spec.Steps[0].Matrix.Dimensions[0].Name != "os" ||
		len(spec.Steps[0].Matrix.Exclude) != 1 {
		t.Errorf("matrix not reproduced: %+v", spec.Steps[0].Matrix)
	}
	if spec.Steps[1].Foreach == nil || spec.Steps[1].Foreach.Source.Expr != "{{ .Params.envs }}" {
		t.Errorf("foreach not reproduced: %+v", spec.Steps[1].Foreach)
	}
}
