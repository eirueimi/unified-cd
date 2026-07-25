package controller

import (
	"encoding/json"
	"fmt"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

func prepareRunSpec(spec dsl.Spec, params map[string]string) ([]byte, error) {
	if err := dsl.ResolveSecretNameParamsInSpec(&spec, params); err != nil {
		return nil, fmt.Errorf("resolve secret name parameters: %w", err)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("marshal resolved run spec: %w", err)
	}
	return raw, nil
}
