package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// coerceToJSONBytes converts YAML config to JSON bytes so we can re-use the strict
// JSON decoder (DisallowUnknownFields) for both formats.
//
// Returns (jsonBytes, format, err) where format is "json" or "yaml".
func coerceToJSONBytes(path string, data []byte) ([]byte, string, error) {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".yaml" && ext != ".yml" {
		return data, "json", nil
	}

	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, "yaml", fmt.Errorf("yaml unmarshal: %w", err)
	}

	v = normalizeYAML(v)

	j, err := json.Marshal(v)
	if err != nil {
		return nil, "yaml", fmt.Errorf("yaml->json marshal: %w", err)
	}
	return j, "yaml", nil
}

// normalizeYAML ensures all map keys are strings so the result can be JSON-marshaled.
func normalizeYAML(in any) any {
	switch x := in.(type) {
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, v := range x {
			m[fmt.Sprint(k)] = normalizeYAML(v)
		}
		return m
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, v := range x {
			m[k] = normalizeYAML(v)
		}
		return m
	case []any:
		for i := range x {
			x[i] = normalizeYAML(x[i])
		}
		return x
	default:
		return in
	}
}
