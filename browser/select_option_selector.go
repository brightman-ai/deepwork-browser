package browser

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// SelectOptionSelector identifies exactly one field of a native option. An
// explicit empty string is a valid choice; a missing or null field is not.
type SelectOptionSelector struct {
	Label *string `json:"label,omitempty" yaml:"label,omitempty"`
	Value *string `json:"value,omitempty" yaml:"value,omitempty"`
}

func SelectByLabel(label string) *SelectOptionSelector { return &SelectOptionSelector{Label: &label} }
func SelectByValue(value string) *SelectOptionSelector { return &SelectOptionSelector{Value: &value} }

func (s *SelectOptionSelector) Validate() error {
	if s == nil || (s.Label == nil) == (s.Value == nil) {
		return fmt.Errorf("option requires exactly one string field: label or value")
	}
	return nil
}
func (s *SelectOptionSelector) UnmarshalJSON(data []byte) error {
	fields, err := uniqueJSONFields(data)
	if err != nil {
		return err
	}
	if len(fields) != 1 {
		return fmt.Errorf("option requires exactly one string field: label or value")
	}
	var next SelectOptionSelector
	for key, raw := range fields {
		if key != "label" && key != "value" {
			return fmt.Errorf("unknown option field %q", key)
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("option %s must be a string", key)
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if key == "label" {
			next.Label = &value
		} else {
			next.Value = &value
		}
	}
	*s = next
	return s.Validate()
}
func (s *SelectOptionSelector) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode || len(node.Content) != 2 {
		return fmt.Errorf("option requires exactly one string field: label or value")
	}
	key, value := node.Content[0].Value, node.Content[1]
	if key != "label" && key != "value" {
		return fmt.Errorf("unknown option field %q", key)
	}
	if value.Tag != "!!str" {
		return fmt.Errorf("option %s must be a string", key)
	}
	if key == "label" {
		*s = *SelectByLabel(value.Value)
	} else {
		*s = *SelectByValue(value.Value)
	}
	return nil
}

// SelectOptionSelectorFields is the shared exact-selector vocabulary.
func SelectOptionSelectorFields() []string { return []string{"label", "value"} }

// uniqueJSONFields preserves object presence and rejects duplicate keys rather
// than allowing the JSON decoder's last-field-wins interpretation.
func uniqueJSONFields(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if token != json.Delim('{') {
		return nil, fmt.Errorf("expected an object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("expected an object field")
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("duplicate field %q", key)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		fields[key] = raw
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing JSON")
	}
	return fields, nil
}
