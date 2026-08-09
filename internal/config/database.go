package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// DatabaseSelection selects either every database or an explicit list.
type DatabaseSelection struct {
	All   bool
	Names []string
}

// UnmarshalYAML accepts only the exact scalar "all" or a non-empty sequence
// of non-empty string database names.
func (d *DatabaseSelection) UnmarshalYAML(value *yaml.Node) error {
	*d = DatabaseSelection{}
	switch value.Kind {
	case yaml.ScalarNode:
		if value.Tag == "!!str" && value.Value == "all" {
			d.All = true
			return nil
		}
	case yaml.SequenceNode:
		if len(value.Content) == 0 {
			return fmt.Errorf("must be a non-empty sequence")
		}
		names := make([]string, 0, len(value.Content))
		for _, item := range value.Content {
			if item.Kind != yaml.ScalarNode || item.Tag != "!!str" || item.Value == "" {
				return fmt.Errorf("must contain only non-empty strings")
			}
			names = append(names, item.Value)
		}
		d.Names = names
		return nil
	}
	return fmt.Errorf("must be the scalar all or a non-empty sequence of strings")
}
