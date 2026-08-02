// Package main: validate.go is a minimal JSON Schema validator for the subset
// Latigo needs (spec §2.5: "schema-checked before dispatch"). It covers the
// vocabulary used by tool argument schemas and the finish tool's output
// schema: type, required, properties, items, enum, const, additionalProperties,
// and basic numeric/string bounds. Deliberately dependency-free — schema
// validation is security-adjacent (it gates dispatch), so a small auditable
// validator is preferable to a large transitive dependency.
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Schema is the subset of JSON Schema Latigo understands. Unknown keywords are
// ignored (forward-compatible), so a richer schema degrades gracefully to the
// parts we check.
type Schema struct {
	Type                 string             `json:"type,omitempty"` // "object"|"array"|"string"|"number"|"integer"|"boolean"|"null"
	Enum                 []json.RawMessage  `json:"enum,omitempty"`
	Const                *json.RawMessage   `json:"const,omitempty"`
	Required             []string           `json:"required,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	AdditionalProperties *json.RawMessage   `json:"additionalProperties,omitempty"`
	Minimum              *float64           `json:"minimum,omitempty"`
	Maximum              *float64           `json:"maximum,omitempty"`
	MinLength            int                `json:"minLength,omitempty"`
	MaxLength            int                `json:"maxLength,omitempty"`
	MinItems             int                `json:"minItems,omitempty"`
	MaxItems             int                `json:"maxItems,omitempty"`
}

// parseSchema decodes a raw JSON Schema blob.
func parseSchema(raw json.RawMessage) (*Schema, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// validateValue validates v (a decoded any) against s, returning a list of
// human-readable errors. Empty list means valid.
func validateValue(s *Schema, v any) []string {
	if s == nil {
		return nil
	}
	var errs []string
	if r, ok := v.(json.RawMessage); ok {
		var dec any
		if err := json.Unmarshal(r, &dec); err != nil {
			return []string{"value is not valid JSON: " + err.Error()}
		}
		v = dec
	}
	if s.Const != nil {
		var want any
		_ = json.Unmarshal(*s.Const, &want)
		if !jsonEqual(v, want) {
			errs = append(errs, fmt.Sprintf("must equal %s", trim(string(*s.Const))))
		}
	}
	if len(s.Enum) > 0 {
		ok := false
		for _, e := range s.Enum {
			var want any
			_ = json.Unmarshal(e, &want)
			if jsonEqual(v, want) {
				ok = true
				break
			}
		}
		if !ok {
			errs = append(errs, "value not in enum")
		}
	}
	if s.Type != "" {
		if e := validateType(s.Type, v, s); e != "" {
			errs = append(errs, e)
		}
	}
	return errs
}

// validateType checks the type-specific constraints and recurses.
func validateType(typ string, v any, s *Schema) string {
	switch typ {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return "expected object"
		}
		for _, req := range s.Required {
			if _, has := m[req]; !has {
				return fmt.Sprintf("missing required property %q", req)
			}
		}
		// Disallow unknown properties when additionalProperties is false.
		if s.AdditionalProperties != nil {
			var ap any
			_ = json.Unmarshal(*s.AdditionalProperties, &ap)
			if ap == false {
				for k := range m {
					if _, known := s.Properties[k]; !known {
						return fmt.Sprintf("additional property %q not allowed", k)
					}
				}
			}
		}
		for k, val := range m {
			if sub, ok := s.Properties[k]; ok && sub != nil {
				if e := validateValue(sub, val); len(e) > 0 {
					return fmt.Sprintf("property %q: %s", k, strings.Join(e, "; "))
				}
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return "expected array"
		}
		if len(arr) < s.MinItems {
			return fmt.Sprintf("array shorter than minItems %d", s.MinItems)
		}
		if s.MaxItems > 0 && len(arr) > s.MaxItems {
			return fmt.Sprintf("array longer than maxItems %d", s.MaxItems)
		}
		if s.Items != nil {
			for i, el := range arr {
				if e := validateValue(s.Items, el); len(e) > 0 {
					return fmt.Sprintf("item[%d]: %s", i, strings.Join(e, "; "))
				}
			}
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			return "expected string"
		}
		if len(str) < s.MinLength {
			return fmt.Sprintf("string shorter than minLength %d", s.MinLength)
		}
		if s.MaxLength > 0 && len(str) > s.MaxLength {
			return fmt.Sprintf("string longer than maxLength %d", s.MaxLength)
		}
	case "integer":
		if !isInteger(v) {
			return "expected integer"
		}
		if err := checkBounds(s, v); err != "" {
			return err
		}
	case "number":
		if !isNumber(v) {
			return "expected number"
		}
		if err := checkBounds(s, v); err != "" {
			return err
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return "expected boolean"
		}
	case "null":
		if v != nil {
			return "expected null"
		}
	}
	return ""
}

func checkBounds(s *Schema, v any) string {
	f, ok := toFloat(v)
	if !ok {
		return ""
	}
	if s.Minimum != nil && f < *s.Minimum {
		return fmt.Sprintf("value below minimum %v", *s.Minimum)
	}
	if s.Maximum != nil && f > *s.Maximum {
		return fmt.Sprintf("value above maximum %v", *s.Maximum)
	}
	return ""
}

func isInteger(v any) bool {
	switch v.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case float64:
		f := v.(float64)
		return f == float64(int64(f))
	}
	return false
}

func isNumber(v any) bool {
	switch v.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return true
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// jsonEqual compares two decoded JSON values structurally.
func jsonEqual(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			if !jsonEqual(v, bv[k]) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

func trim(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		return s[:77] + "..."
	}
	return s
}

// sortedKeys is a small helper for stable iteration.
func sortedKeys(m map[string]*Schema) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
