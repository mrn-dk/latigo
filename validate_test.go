package main

import (
	"encoding/json"
	"testing"
)

func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestValidateTypeAndRequired(t *testing.T) {
	s := &Schema{
		Type:     "object",
		Required: []string{"command"},
		Properties: map[string]*Schema{
			"command": {Type: "string", MinLength: 1},
		},
		AdditionalProperties: boolFalse(),
	}
	if errs := validateValue(s, rawJSON(t, map[string]any{"command": "ls"})); len(errs) != 0 {
		t.Fatalf("expected valid, got %v", errs)
	}
	if errs := validateValue(s, rawJSON(t, map[string]any{})); len(errs) == 0 {
		t.Fatalf("expected missing-required error")
	}
	if errs := validateValue(s, rawJSON(t, map[string]any{"command": "ls", "extra": 1})); len(errs) == 0 {
		t.Fatalf("expected additionalProperties error")
	}
	if errs := validateValue(s, rawJSON(t, map[string]any{"command": ""})); len(errs) == 0 {
		t.Fatalf("expected minLength error")
	}
}

func TestValidateArrayItems(t *testing.T) {
	s := &Schema{Type: "array", MinItems: 1, MaxItems: 2, Items: &Schema{Type: "integer"}}
	if errs := validateValue(s, rawJSON(t, []any{1, 2})); len(errs) != 0 {
		t.Fatalf("expected valid, got %v", errs)
	}
	if errs := validateValue(s, rawJSON(t, []any{})); len(errs) == 0 {
		t.Fatalf("expected minItems error")
	}
	if errs := validateValue(s, rawJSON(t, []any{1, 2, 3})); len(errs) == 0 {
		t.Fatalf("expected maxItems error")
	}
	if errs := validateValue(s, rawJSON(t, []any{"x"})); len(errs) == 0 {
		t.Fatalf("expected item type error")
	}
}

func TestValidateEnumAndConst(t *testing.T) {
	enum := rawJSON(t, "a")
	s := &Schema{Type: "string", Enum: []json.RawMessage{enum}}
	if errs := validateValue(s, rawJSON(t, "a")); len(errs) != 0 {
		t.Fatalf("expected valid, got %v", errs)
	}
	if errs := validateValue(s, rawJSON(t, "z")); len(errs) == 0 {
		t.Fatalf("expected enum error")
	}
}

func TestValidateNumberBounds(t *testing.T) {
	min := 0.0
	max := 10.0
	s := &Schema{Type: "number", Minimum: &min, Maximum: &max}
	if errs := validateValue(s, rawJSON(t, 5)); len(errs) != 0 {
		t.Fatalf("expected valid, got %v", errs)
	}
	if errs := validateValue(s, rawJSON(t, 11)); len(errs) == 0 {
		t.Fatalf("expected max error")
	}
}

// boolFalse returns a json.RawMessage encoding false, for additionalProperties.
func boolFalse() *json.RawMessage {
	b := json.RawMessage("false")
	return &b
}
