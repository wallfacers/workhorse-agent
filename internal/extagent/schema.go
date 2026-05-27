package extagent

import (
	_ "embed"
	"encoding/json"

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
	normalized := normalizeYAMLMap(rawMap)
	if err := adapterSchema.Validate(normalized); err != nil {
		return nil, err
	}
	return &a, nil
}

func normalizeYAMLMap(v any) any {
	switch m := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			ks, ok := k.(string)
			if !ok {
				continue
			}
			out[ks] = normalizeYAMLMap(val)
		}
		return out
	case []any:
		for i, elem := range m {
			m[i] = normalizeYAMLMap(elem)
		}
		return m
	default:
		return v
	}
}
