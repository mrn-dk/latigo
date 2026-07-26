package host

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrn-dk/latigo/abi"
)

// TestOAIContentParts is the table test the spec asks for: translating ABI
// content parts into the OpenAI chat/completions content-array dialect. The
// function under test (oaiContentParts) is dialect-specific but takes the
// same neutral abi.ContentPart input a second (e.g. Anthropic) translator
// would, so adding a dialect later is exactly this shape of table again.
func TestOAIContentParts(t *testing.T) {
	png := []byte{0x89, 0x50, 0x4e, 0x47}
	cases := []struct {
		name  string
		parts []abi.ContentPart
		want  []oaiContentPart
	}{
		{
			name:  "text only",
			parts: []abi.ContentPart{{Type: "text", Text: "hello"}},
			want:  []oaiContentPart{{Type: "text", Text: "hello"}},
		},
		{
			name:  "inline image data becomes a data URI",
			parts: []abi.ContentPart{{Type: "image", Image: &abi.ImageData{MediaType: "image/png", Data: png}}},
			want: []oaiContentPart{
				{Type: "image_url", ImageURL: &oaiImageURL{URL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)}},
			},
		},
		{
			name:  "image URL passed through unchanged",
			parts: []abi.ContentPart{{Type: "image", Image: &abi.ImageData{MediaType: "image/jpeg", URL: "https://example.com/a.jpg"}}},
			want: []oaiContentPart{
				{Type: "image_url", ImageURL: &oaiImageURL{URL: "https://example.com/a.jpg"}},
			},
		},
		{
			name: "mixed text then image, in order",
			parts: []abi.ContentPart{
				{Type: "text", Text: "look at this"},
				{Type: "image", Image: &abi.ImageData{MediaType: "image/png", Data: png}},
			},
			want: []oaiContentPart{
				{Type: "text", Text: "look at this"},
				{Type: "image_url", ImageURL: &oaiImageURL{URL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)}},
			},
		},
		{
			name:  "image part with neither data nor URL is dropped",
			parts: []abi.ContentPart{{Type: "image", Image: &abi.ImageData{MediaType: "image/png"}}},
			want:  []oaiContentPart{},
		},
		{
			name:  "nil Image on an image-typed part is dropped",
			parts: []abi.ContentPart{{Type: "image"}},
			want:  []oaiContentPart{},
		},
		{
			name:  "empty text part is dropped",
			parts: []abi.ContentPart{{Type: "text", Text: ""}},
			want:  []oaiContentPart{},
		},
		{
			name:  "unrecognised type degrades to text",
			parts: []abi.ContentPart{{Type: "audio", Text: "a transcript"}},
			want:  []oaiContentPart{{Type: "text", Text: "a transcript"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := oaiContentParts(tc.parts)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("oaiContentParts(%+v) = %s, want %s", tc.parts, gotJSON, wantJSON)
			}
		})
	}
}

// TestOAIContent verifies the Content/Parts precedence rule: an empty Parts
// falls back to the plain string Content field (the common, backward
// compatible case); a non-empty Parts is authoritative and Content is not
// separately re-emitted.
func TestOAIContent(t *testing.T) {
	t.Run("no parts uses plain content string", func(t *testing.T) {
		got := oaiContent(abi.LLMMessage{Content: "hi there"})
		s, ok := got.(string)
		if !ok || s != "hi there" {
			t.Errorf("oaiContent = %#v (%T), want the string %q", got, got, "hi there")
		}
	})
	t.Run("parts present is authoritative", func(t *testing.T) {
		got := oaiContent(abi.LLMMessage{
			Content: "ignored shorthand",
			Parts:   []abi.ContentPart{{Type: "text", Text: "actual"}},
		})
		arr, ok := got.([]oaiContentPart)
		if !ok {
			t.Fatalf("oaiContent = %#v (%T), want []oaiContentPart", got, got)
		}
		if len(arr) != 1 || arr[0].Text != "actual" {
			t.Errorf("oaiContent parts = %+v, want [{text actual}]", arr)
		}
	})
}

// TestLLMClientSendsContentArrayForImages is an end-to-end check (through the
// real HTTP path, not just the pure translation function) that a message
// with an image Part is actually serialised as a "content" array on the wire
// to an OpenAI-compatible /chat/completions endpoint.
func TestLLMClientSendsContentArrayForImages(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(chatOKBody("ok"))
	}))
	defer srv.Close()

	c, _ := newTestLLMClient(srv, LLMRetry{MaxAttempts: 1})
	req := abi.LLMCallRequest{
		Messages: []abi.LLMMessage{
			{Role: "user", Content: "ignored", Parts: []abi.ContentPart{
				{Type: "text", Text: "what is this?"},
				{Type: "image", Image: &abi.ImageData{MediaType: "image/png", Data: []byte{1, 2, 3}}},
			}},
		},
	}
	if _, err := c.call(context.Background(), req); err != nil {
		t.Fatalf("call: %v", err)
	}

	var wire oaiRequest
	if err := json.Unmarshal(gotBody, &wire); err != nil {
		t.Fatalf("decode sent body: %v (body=%s)", err, gotBody)
	}
	if len(wire.Messages) != 1 {
		t.Fatalf("sent %d messages, want 1", len(wire.Messages))
	}
	arr, ok := wire.Messages[0].Content.([]any)
	if !ok {
		t.Fatalf("sent content = %#v (%T), want a JSON array", wire.Messages[0].Content, wire.Messages[0].Content)
	}
	if len(arr) != 2 {
		t.Fatalf("sent content array len = %d, want 2 (text + image_url)", len(arr))
	}
	second, _ := json.Marshal(arr[1])
	if !bytes.Contains(second, []byte(`"type":"image_url"`)) || !bytes.Contains(second, []byte("data:image/png;base64,")) {
		t.Errorf("second content part = %s, want an image_url with a data: URI", second)
	}
}
