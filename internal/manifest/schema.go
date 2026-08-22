package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const SchemaURL = "https://example.com/go-agent-optimizer/target-manifest-v1.schema.json"

var (
	schemaOnce     sync.Once
	compiledSchema *jsonschema.Schema
	schemaErr      error
)

// ValidateJSON validates the structural v1 contract with jsonschema/v6.
// Load additionally applies defaults and performs semantic checks that JSON
// Schema intentionally does not express, such as duplicate seed IDs and
// regular-expression compilation.
func ValidateJSON(data []byte) error {
	var document any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("decode manifest JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("manifest must contain exactly one JSON value")
		}
		return fmt.Errorf("decode trailing manifest JSON: %w", err)
	}

	schemaOnce.Do(func() {
		var schemaDocument any
		if err := json.Unmarshal(schemaJSON, &schemaDocument); err != nil {
			schemaErr = fmt.Errorf("decode embedded manifest schema: %w", err)
			return
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource(SchemaURL, schemaDocument); err != nil {
			schemaErr = fmt.Errorf("register manifest schema: %w", err)
			return
		}
		compiledSchema, schemaErr = compiler.Compile(SchemaURL)
	})
	if schemaErr != nil {
		return schemaErr
	}
	if err := compiledSchema.Validate(document); err != nil {
		return fmt.Errorf("manifest schema validation failed: %w", err)
	}
	return nil
}

func Load(data []byte) (Manifest, error) {
	if err := ValidateJSON(data); err != nil {
		return Manifest{}, err
	}
	var result Manifest
	if err := json.Unmarshal(data, &result); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	result.ApplyDefaults()
	if err := result.SemanticValidate(); err != nil {
		return Manifest{}, err
	}
	return result, nil
}

func LoadFile(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest %q: %w", path, err)
	}
	return Load(data)
}
