// schemagen generates schemas/unified-cd.schema.json from internal/dsl/*_types.go.
// Run via: go generate ./internal/dsl/
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/eirueimi/unified-cd/internal/dslschema"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "schemagen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := projectRoot()
	if err != nil {
		return err
	}
	dslDir := filepath.Join(root, "internal", "dsl")

	structs, typeDescs, err := dslschema.ParseDSL(dslDir)
	if err != nil {
		return fmt.Errorf("parse dsl: %w", err)
	}

	schema := dslschema.BuildSchema(structs, typeDescs)

	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return err
	}
	out := filepath.Join(root, "schemas", "unified-cd.schema.json")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(out, append(data, '\n'), 0o644)
}

func projectRoot() (string, error) {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found from %s", dir)
		}
		dir = parent
	}
}
