package extagent

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

//go:embed schema/adapter.schema.json
var adapterSchemaJSON []byte

var adapterSchema *jsonschema.Schema

func init() {
	var schemaDoc any
	if err := json.Unmarshal(adapterSchemaJSON, &schemaDoc); err != nil {
		panic("extagent: failed to parse adapter schema JSON: " + err.Error())
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("https://workhorse-agent/extagent/adapter.schema.json", schemaDoc); err != nil {
		panic("extagent: failed to load adapter schema: " + err.Error())
	}
	adapterSchema = compiler.MustCompile("https://workhorse-agent/extagent/adapter.schema.json")
}

// Parse validates raw YAML bytes and returns an Adapter. The caller is
// responsible for filename-stem-vs-name checks (done at the loader level).
func Parse(raw []byte) (*Adapter, error) {
	var a Adapter
	if err := yaml.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	var rawMap any
	if err := yaml.Unmarshal(raw, &rawMap); err != nil {
		return nil, err
	}
	normalized, err := normalizeYAMLMap(rawMap)
	if err != nil {
		return nil, err
	}
	if err := adapterSchema.Validate(normalized); err != nil {
		return nil, err
	}
	return &a, nil
}

// normalizeYAMLMap converts yaml.v3's map[any]any to map[string]any for
// JSON Schema validation. Non-string keys are rejected with an error so
// schema rules like additionalProperties:false cannot be bypassed by an
// int key that yaml.Unmarshal-into-struct silently coerces to a string.
func normalizeYAMLMap(v any) (any, error) {
	switch m := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("extagent: yaml map key must be string, got %T (%v)", k, k)
			}
			normVal, err := normalizeYAMLMap(val)
			if err != nil {
				return nil, err
			}
			out[ks] = normVal
		}
		return out, nil
	case []any:
		for i, elem := range m {
			normElem, err := normalizeYAMLMap(elem)
			if err != nil {
				return nil, err
			}
			m[i] = normElem
		}
		return m, nil
	default:
		return v, nil
	}
}
