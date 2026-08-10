package config

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

var databaseSelectionType = reflect.TypeOf(DatabaseSelection{})

func validateStrictYAML(data []byte) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	var additional yaml.Node
	if err := decoder.Decode(&additional); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("configuration must contain exactly one YAML document")
	}
	if len(document.Content) == 0 {
		return fmt.Errorf("configuration document is empty")
	}
	root := document.Content[0]
	if err := rejectYAMLIndirection(root, "configuration"); err != nil {
		return err
	}
	return validateYAMLType(root, reflect.TypeOf(Config{}), "configuration")
}

func rejectYAMLIndirection(node *yaml.Node, path string) error {
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		return fmt.Errorf("%s: YAML anchors and aliases are not supported", path)
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Tag == "!!merge" || key.Value == "<<" {
				return fmt.Errorf("%s: YAML merge keys are not supported", path)
			}
		}
	}
	for _, child := range node.Content {
		if err := rejectYAMLIndirection(child, path); err != nil {
			return err
		}
	}
	return nil
}

func validateYAMLType(node *yaml.Node, typ reflect.Type, path string) error {
	if node.Tag == "!!null" {
		return fmt.Errorf("%s: null is not allowed", path)
	}
	if typ == databaseSelectionType {
		return validateDatabaseSelectionYAMLType(node, path)
	}

	switch typ.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode || node.Tag != "!!map" {
			return yamlKindError(path, "mapping", node)
		}
		fields := yamlStructFields(typ)
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return yamlKindError(path+" key", "string scalar", key)
			}
			fieldType, known := fields[key.Value]
			if !known {
				continue // KnownFields reports the existing unknown-field error.
			}
			if err := validateYAMLType(value, fieldType, joinYAMLPath(path, key.Value)); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if node.Kind != yaml.MappingNode || node.Tag != "!!map" {
			return yamlKindError(path, "mapping", node)
		}
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if err := validateYAMLType(key, typ.Key(), path+" key"); err != nil {
				return err
			}
			if err := validateYAMLType(value, typ.Elem(), joinYAMLPath(path, key.Value)); err != nil {
				return err
			}
		}
		return nil
	case reflect.Slice:
		if node.Kind != yaml.SequenceNode || node.Tag != "!!seq" {
			return yamlKindError(path, "sequence", node)
		}
		for i, item := range node.Content {
			if err := validateYAMLType(item, typ.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	case reflect.String:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
			return yamlKindError(path, "string scalar", node)
		}
		return nil
	case reflect.Bool:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" {
			return yamlKindError(path, "boolean scalar", node)
		}
		return nil
	case reflect.Int:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
			return yamlKindError(path, "integer scalar", node)
		}
		return nil
	default:
		return fmt.Errorf("%s: unsupported configuration type %s", path, typ)
	}
}

func validateDatabaseSelectionYAMLType(node *yaml.Node, path string) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag != "!!str" {
			return yamlKindError(path, "string scalar", node)
		}
		return nil
	case yaml.SequenceNode:
		if node.Tag != "!!seq" {
			return yamlKindError(path, "string sequence", node)
		}
		for i, item := range node.Content {
			if item.Kind != yaml.ScalarNode || item.Tag != "!!str" {
				return yamlKindError(fmt.Sprintf("%s[%d]", path, i), "string scalar", item)
			}
		}
		return nil
	default:
		return yamlKindError(path, "the scalar all or a string sequence", node)
	}
}

func yamlStructFields(typ reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		if name != "-" {
			fields[name] = field.Type
		}
	}
	return fields
}

func joinYAMLPath(path, field string) string {
	if path == "" {
		return field
	}
	return path + "." + field
}

func yamlKindError(path, want string, node *yaml.Node) error {
	return fmt.Errorf("%s: expected %s, got %s", path, want, node.ShortTag())
}
