package logging

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

const redactedValue = "[REDACTED]"

var secretKeyFragments = []string{
	"password",
	"secret",
	"token",
	"credential",
	"mysql_pwd",
}

func redactFields(fields map[string]any) map[string]any {
	if fields == nil {
		return nil
	}
	redacted := make(map[string]any, len(fields))
	for key, value := range fields {
		if isSecretKey(key) {
			redacted[key] = redactedValue
			continue
		}
		redacted[key] = redactValue(value)
	}
	return redacted
}

func redactValue(value any) any {
	return redactReflect(reflect.ValueOf(value), 0)
}

func redactReflect(value reflect.Value, depth int) any {
	if !value.IsValid() {
		return nil
	}
	if depth > 1024 {
		return redactedValue
	}
	for value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}
	if value.CanInterface() {
		if _, ok := value.Interface().(json.Marshaler); ok {
			return redactMarshaledValue(value.Interface(), depth)
		}
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		return redactReflect(value.Elem(), depth+1)
	}

	switch value.Kind() {
	case reflect.Map:
		if value.IsNil() {
			return nil
		}
		if value.Type().Key().Kind() != reflect.String {
			return value.Interface()
		}
		redacted := make(map[string]any, value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			key := iterator.Key().String()
			if isSecretKey(key) {
				redacted[key] = redactedValue
				continue
			}
			redacted[key] = redactReflect(iterator.Value(), depth+1)
		}
		return redacted
	case reflect.Slice, reflect.Array:
		if value.Kind() == reflect.Slice && value.IsNil() {
			return nil
		}
		redacted := make([]any, value.Len())
		for index := range value.Len() {
			redacted[index] = redactReflect(value.Index(index), depth+1)
		}
		return redacted
	case reflect.Struct:
		return redactMarshaledValue(value.Interface(), depth)
	default:
		return value.Interface()
	}
}

func redactMarshaledValue(value any, depth int) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return redactedValue
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return redactedValue
	}
	return redactReflect(reflect.ValueOf(decoded), depth+1)
}

func isSecretKey(key string) bool {
	lower := strings.ToLower(key)
	for _, fragment := range secretKeyFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}
