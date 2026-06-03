package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// PermissionConfig is the subset of config.yaml the assistant settings page and
// the hot-reload path care about: the default fallback decision and the preset
// permission rules under the `tools` mapping.
type PermissionConfig struct {
	DefaultPermission string       `yaml:"default_permission"`
	PresetRules       []PresetRule `yaml:"preset_rules"`
}

// ReadPermissionConfig extracts the permission subset from config.yaml. A
// missing file yields the zero value (empty default, no rules) rather than an
// error — the operator may not have run `init` yet.
func ReadPermissionConfig(path string) (PermissionConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PermissionConfig{}, nil
		}
		return PermissionConfig{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	var doc struct {
		Tools PermissionConfig `yaml:"tools"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return PermissionConfig{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return doc.Tools, nil
}

// WritePermissionConfig rewrites only the `tools.default_permission` and
// `tools.preset_rules` keys of config.yaml, preserving every other key,
// comment, and the document's key order. It edits the YAML node tree in place
// (rather than marshalling a struct) so operator comments survive. The write is
// atomic: a temp file in the same directory is renamed over the target.
func WritePermissionConfig(path string, pc PermissionConfig) error {
	var root yaml.Node
	if data, err := os.ReadFile(path); err == nil {
		if uerr := yaml.Unmarshal(data, &root); uerr != nil {
			return fmt.Errorf("config: parse %s: %w", path, uerr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("config: read %s: %w", path, err)
	}

	mapping := documentMapping(&root)

	// default_permission scalar.
	defNode := &yaml.Node{}
	if err := defNode.Encode(pc.DefaultPermission); err != nil {
		return fmt.Errorf("config: encode default_permission: %w", err)
	}
	// preset_rules sequence.
	rulesNode := &yaml.Node{}
	rules := pc.PresetRules
	if rules == nil {
		rules = []PresetRule{}
	}
	if err := rulesNode.Encode(rules); err != nil {
		return fmt.Errorf("config: encode preset_rules: %w", err)
	}

	tools := ensureChildMapping(mapping, "tools")
	setMapValue(tools, "default_permission", defNode)
	setMapValue(tools, "preset_rules", rulesNode)

	out, err := yaml.Marshal(&root)
	if err != nil {
		return fmt.Errorf("config: marshal %s: %w", path, err)
	}
	return atomicWrite(path, out)
}

// documentMapping returns the top-level mapping node, initialising the document
// and its root mapping when the file was empty or absent.
func documentMapping(root *yaml.Node) *yaml.Node {
	if root.Kind == 0 {
		root.Kind = yaml.DocumentNode
	}
	if len(root.Content) == 0 {
		root.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	return root.Content[0]
}

// findMapValue returns the value node paired with key in a mapping node, or nil.
func findMapValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// setMapValue replaces the value node for key, or appends a new key/value pair
// (preserving existing keys and their comments).
func setMapValue(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value,
	)
}

// ensureChildMapping returns the mapping node under key, creating an empty one
// if the key is absent or not a mapping.
func ensureChildMapping(parent *yaml.Node, key string) *yaml.Node {
	if v := findMapValue(parent, key); v != nil && v.Kind == yaml.MappingNode {
		return v
	}
	child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	setMapValue(parent, key, child)
	return child
}

// atomicWrite writes data to a temp file in the same directory then renames it
// over path, so a reader never observes a half-written file.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("config: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("config: rename temp: %w", err)
	}
	return nil
}
