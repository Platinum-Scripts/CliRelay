package config

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// YAMLRootKeys returns the root keys a config.yaml document spells out, or nil
// when it is not a YAML mapping. A raw config.yaml save is judged by these:
// a key the operator wrote is meant, even with the value a default would give.
func YAMLRootKeys(data []byte) map[string]bool {
	if len(data) == 0 {
		return nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil
	}
	mapping := root.Content[0]
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	keys := make(map[string]bool, len(mapping.Content)/2)
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		key := mapping.Content[i]
		if key == nil || key.Kind != yaml.ScalarNode {
			continue
		}
		name := strings.TrimSpace(key.Value)
		if name != "" {
			keys[name] = true
		}
	}
	return keys
}
