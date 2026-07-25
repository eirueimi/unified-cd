package controller

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

func prepareRunSpec(specJSON []byte, params map[string]string) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(specJSON, &root); err != nil {
		return nil, fmt.Errorf("decode stored run spec: %w", err)
	}
	if root == nil {
		return nil, fmt.Errorf("stored run spec must be an object")
	}
	for _, field := range []string{"steps", "finally"} {
		if err := resolveRunSpecEntries(root, field, params); err != nil {
			return nil, fmt.Errorf("resolve secret name parameters: %w", err)
		}
	}
	raw, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("marshal resolved run spec: %w", err)
	}
	return raw, nil
}

func findJSONField(object map[string]any, wanted string) (string, any, bool, error) {
	var foundKey string
	var foundValue any
	for key, value := range object {
		if !strings.EqualFold(key, wanted) {
			continue
		}
		if foundKey != "" {
			return "", nil, false, fmt.Errorf("ambiguous fields %q and %q", foundKey, key)
		}
		foundKey, foundValue = key, value
	}
	return foundKey, foundValue, foundKey != "", nil
}

func resolveRunSpecEntries(root map[string]any, field string, params map[string]string) error {
	key, value, found, err := findJSONField(root, field)
	if err != nil {
		return err
	}
	if !found || value == nil {
		return nil
	}
	entries, ok := value.([]any)
	if !ok {
		return fmt.Errorf("field %q must be an array", key)
	}
	for index, entry := range entries {
		step, ok := entry.(map[string]any)
		path := fmt.Sprintf("%s[%d]", key, index)
		if !ok {
			return fmt.Errorf("field %q must be an object", path)
		}
		if err := resolveRunSpecStep(step, path, params); err != nil {
			return err
		}
	}
	return nil
}

func resolveRunSpecStep(step map[string]any, path string, params map[string]string) error {
	if err := resolveRunSpecStringField(step, "run", path, params); err != nil {
		return err
	}
	if err := resolveRunSpecEnv(step, path, params); err != nil {
		return err
	}

	key, value, found, err := findJSONField(step, "parallel")
	if err != nil {
		return err
	}
	if !found || value == nil {
		return nil
	}
	parallel, ok := value.([]any)
	if !ok {
		return fmt.Errorf("field %q must be an array", path+"."+key)
	}
	for index, entry := range parallel {
		child, ok := entry.(map[string]any)
		childPath := fmt.Sprintf("%s.%s[%d]", path, key, index)
		if !ok {
			return fmt.Errorf("field %q must be an object", childPath)
		}
		if err := resolveRunSpecStringField(child, "run", childPath, params); err != nil {
			return err
		}
		if err := resolveRunSpecEnv(child, childPath, params); err != nil {
			return err
		}
	}
	return nil
}

func resolveRunSpecStringField(object map[string]any, field, path string, params map[string]string) error {
	key, value, found, err := findJSONField(object, field)
	if err != nil {
		return err
	}
	if !found || value == nil {
		return nil
	}
	text, ok := value.(string)
	if !ok {
		return fmt.Errorf("field %q must be a string", path+"."+key)
	}
	resolved, err := dsl.ResolveSecretNameParams(text, params)
	if err != nil {
		return fmt.Errorf("field %q: %w", path+"."+key, err)
	}
	object[key] = resolved
	return nil
}

func resolveRunSpecEnv(step map[string]any, path string, params map[string]string) error {
	key, value, found, err := findJSONField(step, "env")
	if err != nil {
		return err
	}
	if !found || value == nil {
		return nil
	}
	env, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("field %q must be an object", path+"."+key)
	}
	for variable, value := range env {
		if value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("field %q must be a string", path+"."+key+"."+variable)
		}
		resolved, err := dsl.ResolveSecretNameParams(text, params)
		if err != nil {
			return fmt.Errorf("field %q: %w", path+"."+key+"."+variable, err)
		}
		env[variable] = resolved
	}
	return nil
}
