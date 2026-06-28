package controller

import (
	"encoding/json"

	"github.com/unified-cd/unified-cd/internal/dsl"
	"gopkg.in/yaml.v3"
)

func specJSONToYAML(specJSON []byte) ([]byte, error) {
	var spec dsl.Spec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return nil, err
	}
	return yaml.Marshal(spec)
}
