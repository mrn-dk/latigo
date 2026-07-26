package abi

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestLLMMessagePartsRoundTrip verifies a message with mixed text+image
// content parts survives a JSON encode/decode cycle byte-for-byte on the
// fields that matter, including the raw image bytes (which encoding/json
// carries as base64 for a []byte field). This is the shape every hostcall
// request/response and every event-log entry goes through, so a lossy
// round-trip here would silently corrupt images on replay.
func TestLLMMessagePartsRoundTrip(t *testing.T) {
	imgBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0x01, 0x02, 0x03}
	msg := LLMMessage{
		Role:    "user",
		Content: "look at this",
		Parts: []ContentPart{
			{Type: "text", Text: "here is a screenshot"},
			{Type: "image", Image: &ImageData{MediaType: "image/png", Data: imgBytes}},
			{Type: "image", Image: &ImageData{MediaType: "image/jpeg", URL: "https://example.com/a.jpg"}},
		},
	}

	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got LLMMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Role != msg.Role || got.Content != msg.Content {
		t.Fatalf("role/content = %q/%q, want %q/%q", got.Role, got.Content, msg.Role, msg.Content)
	}
	if len(got.Parts) != 3 {
		t.Fatalf("got %d parts, want 3", len(got.Parts))
	}
	if got.Parts[0].Type != "text" || got.Parts[0].Text != "here is a screenshot" {
		t.Errorf("part 0 = %+v, want the text part unchanged", got.Parts[0])
	}
	if got.Parts[1].Type != "image" || got.Parts[1].Image == nil {
		t.Fatalf("part 1 = %+v, want an image part", got.Parts[1])
	}
	if got.Parts[1].Image.MediaType != "image/png" {
		t.Errorf("part 1 media type = %q, want image/png", got.Parts[1].Image.MediaType)
	}
	if !bytes.Equal(got.Parts[1].Image.Data, imgBytes) {
		t.Errorf("part 1 image bytes = %x, want %x (base64 round-trip must be lossless)", got.Parts[1].Image.Data, imgBytes)
	}
	if got.Parts[2].Image == nil || got.Parts[2].Image.URL != "https://example.com/a.jpg" {
		t.Errorf("part 2 = %+v, want the URL-only image preserved", got.Parts[2])
	}

	// A message with no Parts must omit the field entirely (omitempty), so
	// existing text-only event logs/hosts never see a "parts" key appear.
	plain, err := json.Marshal(LLMMessage{Role: "user", Content: "hi"})
	if err != nil {
		t.Fatalf("Marshal plain: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(plain, &raw); err != nil {
		t.Fatalf("Unmarshal plain: %v", err)
	}
	if _, ok := raw["parts"]; ok {
		t.Errorf("plain text message JSON = %s, want no \"parts\" key", plain)
	}
}

// TestNegotiateMultimodal verifies Multimodal is ANDed like the other
// optional capabilities: it is only effective when both the guest wants it
// and the host offers it.
func TestNegotiateMultimodal(t *testing.T) {
	cases := []struct {
		name       string
		want, have bool
		wantEff    bool
	}{
		{"both on", true, true, true},
		{"guest wants, host lacks", true, false, false},
		{"host offers, guest doesn't ask", false, true, false},
		{"both off", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eff := Negotiate(Capabilities{Multimodal: tc.want}, Capabilities{Multimodal: tc.have})
			if eff.Multimodal != tc.wantEff {
				t.Errorf("Negotiate(want=%v, have=%v).Multimodal = %v, want %v", tc.want, tc.have, eff.Multimodal, tc.wantEff)
			}
		})
	}
}
