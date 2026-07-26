package guest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrn-dk/latigo/abi"
)

// snapshotArgs marshals empty args for the fake "snap" tool used below.
func snapshotArgs() string {
	b, _ := json.Marshal(map[string]string{})
	return string(b)
}

// addSnapTool registers a fake tool that always returns a RichResult with one
// image content part alongside its text, exercising the guest.Tool.InvokeRich
// hook (the "screenshot/plot tool" the spec describes).
func addSnapTool(a *Agent) {
	a.Tools().Add(Tool{
		Name:        "snap",
		Description: "take a fake screenshot",
		Schema:      json.RawMessage(`{"type":"object","properties":{}}`),
		InvokeRich: func(ctx context.Context, args json.RawMessage) (RichResult, error) {
			return RichResult{
				Text: "captured",
				Parts: []abi.ContentPart{
					{Type: "image", Image: &abi.ImageData{MediaType: "image/png", Data: []byte{0x89, 0x50, 0x4e, 0x47}}},
				},
			}, nil
		},
	})
}

// TestRichToolPartsSurfaceOnToolMessage verifies a fake tool returning image
// Parts (via InvokeRich) surfaces them on the resulting "tool"-role message
// when the host has negotiated the Multimodal capability.
func TestRichToolPartsSurfaceOnToolMessage(t *testing.T) {
	doneArgs, _ := json.Marshal(map[string]string{"summary": "ok"})
	ft := &fakeTransport{llmTurns: []abi.LLMMessage{
		{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "1", Name: "snap", Arguments: snapshotArgs()}}},
		{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "2", Name: "done", Arguments: string(doneArgs)}}},
	}}
	client := NewClient(ft)
	agent := NewAgent(Config{Goal: "g", MaxTurns: 8, Capabilities: abi.Capabilities{Multimodal: true}}, client)
	addSnapTool(agent)

	if _, err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	var toolMsg *abi.LLMMessage
	for i := range agent.messages {
		if agent.messages[i].ToolCallID == "1" {
			toolMsg = &agent.messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("no tool message recorded for call 1")
	}
	if toolMsg.Content != "captured" {
		t.Errorf("tool message content = %q, want %q", toolMsg.Content, "captured")
	}
	if len(toolMsg.Parts) != 1 || toolMsg.Parts[0].Type != "image" {
		t.Fatalf("tool message parts = %+v, want exactly one image part", toolMsg.Parts)
	}
	if toolMsg.Parts[0].Image == nil || toolMsg.Parts[0].Image.MediaType != "image/png" {
		t.Errorf("tool message image = %+v, want image/png", toolMsg.Parts[0].Image)
	}
}

// TestRichToolPartsDegradeWithoutMultimodal verifies that when the host has
// NOT negotiated the Multimodal capability, an image part returned by a tool
// is dropped and replaced with the text placeholder instead of ever being
// emitted — the guest must never surface image parts to a non-multimodal
// host.
func TestRichToolPartsDegradeWithoutMultimodal(t *testing.T) {
	doneArgs, _ := json.Marshal(map[string]string{"summary": "ok"})
	ft := &fakeTransport{llmTurns: []abi.LLMMessage{
		{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "1", Name: "snap", Arguments: snapshotArgs()}}},
		{Role: "assistant", ToolCalls: []abi.LLMToolCall{{ID: "2", Name: "done", Arguments: string(doneArgs)}}},
	}}
	client := NewClient(ft)
	// No Multimodal capability granted.
	agent := NewAgent(Config{Goal: "g", MaxTurns: 8}, client)
	addSnapTool(agent)

	if _, err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	var toolMsg *abi.LLMMessage
	for i := range agent.messages {
		if agent.messages[i].ToolCallID == "1" {
			toolMsg = &agent.messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("no tool message recorded for call 1")
	}
	if len(toolMsg.Parts) != 1 {
		t.Fatalf("tool message parts = %+v, want exactly one (degraded) part", toolMsg.Parts)
	}
	if toolMsg.Parts[0].Type != "text" || toolMsg.Parts[0].Text != imageOmittedPlaceholder {
		t.Errorf("degraded part = %+v, want a text placeholder %q", toolMsg.Parts[0], imageOmittedPlaceholder)
	}
	for _, p := range toolMsg.Parts {
		if p.Type == "image" {
			t.Errorf("an image part leaked to a non-multimodal host: %+v", toolMsg.Parts)
		}
	}
}

// TestInitialUserMessageWithImagesMultimodal verifies host-attached images
// (Config.Images, as latigo-local's -image flag delivers) land as image parts
// on the initial user turn when Multimodal is negotiated.
func TestInitialUserMessageWithImagesMultimodal(t *testing.T) {
	client := NewClient(&fakeTransport{})
	agent := NewAgent(Config{
		Goal:         "look at this",
		MaxTurns:     1,
		Capabilities: abi.Capabilities{Multimodal: true},
		Images:       []abi.ImageData{{MediaType: "image/jpeg", Data: []byte{1, 2, 3}}},
	}, client)

	msg := agent.initialUserMessage()
	if msg.Role != "user" {
		t.Fatalf("role = %q, want user", msg.Role)
	}
	if len(msg.Parts) != 2 {
		t.Fatalf("parts = %+v, want [text, image]", msg.Parts)
	}
	if msg.Parts[0].Type != "text" || msg.Parts[0].Text != "look at this" {
		t.Errorf("parts[0] = %+v, want the goal as a leading text part", msg.Parts[0])
	}
	if msg.Parts[1].Type != "image" || msg.Parts[1].Image == nil || msg.Parts[1].Image.MediaType != "image/jpeg" {
		t.Errorf("parts[1] = %+v, want the attached image", msg.Parts[1])
	}
}

// TestInitialUserMessageWithImagesDegrades verifies the same host-attached
// images degrade to a text placeholder (no image part reaches the message)
// when the host has not negotiated Multimodal.
func TestInitialUserMessageWithImagesDegrades(t *testing.T) {
	client := NewClient(&fakeTransport{})
	agent := NewAgent(Config{
		Goal:     "look at this",
		MaxTurns: 1,
		Images:   []abi.ImageData{{MediaType: "image/jpeg", Data: []byte{1, 2, 3}}},
	}, client)

	msg := agent.initialUserMessage()
	if len(msg.Parts) != 2 {
		t.Fatalf("parts = %+v, want [text, placeholder-text]", msg.Parts)
	}
	for _, p := range msg.Parts {
		if p.Type == "image" {
			t.Errorf("an image part leaked to a non-multimodal host: %+v", msg.Parts)
		}
	}
	if msg.Parts[1].Type != "text" || msg.Parts[1].Text != imageOmittedPlaceholder {
		t.Errorf("parts[1] = %+v, want the placeholder %q", msg.Parts[1], imageOmittedPlaceholder)
	}
}

// TestAttachImageBuiltin verifies the attach_image built-in reads a file from
// the VFS and surfaces it as an image content part via InvokeRich, degrading
// per the Multimodal capability exactly like a third-party rich tool would.
func TestAttachImageBuiltin(t *testing.T) {
	client := NewClient(&fakeTransport{})
	agent := NewAgent(Config{Goal: "g", MaxTurns: 1, Capabilities: abi.Capabilities{Multimodal: true}}, client)
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0xde, 0xad, 0xbe, 0xef}
	if err := agent.VFS().WriteFile("/work/shot.png", png); err != nil {
		t.Fatal(err)
	}

	args, _ := json.Marshal(map[string]string{"path": "/work/shot.png"})
	text, parts, isErr := agent.Tools().Invoke(context.Background(), "attach_image", args)
	if isErr {
		t.Fatalf("attach_image returned an error: %s", text)
	}
	if len(parts) != 1 || parts[0].Type != "image" || parts[0].Image == nil {
		t.Fatalf("parts = %+v, want exactly one image part", parts)
	}
	if parts[0].Image.MediaType != "image/png" {
		t.Errorf("media type = %q, want image/png", parts[0].Image.MediaType)
	}
	if string(parts[0].Image.Data) != string(png) {
		t.Errorf("image data = %x, want %x", parts[0].Image.Data, png)
	}

	// Same file, but on a host that has not negotiated Multimodal: degrade.
	agent2 := NewAgent(Config{Goal: "g", MaxTurns: 1}, client)
	if err := agent2.VFS().WriteFile("/work/shot.png", png); err != nil {
		t.Fatal(err)
	}
	_, parts2, isErr2 := agent2.Tools().Invoke(context.Background(), "attach_image", args)
	if isErr2 {
		t.Fatal("attach_image should not itself error when degrading")
	}
	if len(parts2) != 1 || parts2[0].Type != "text" || parts2[0].Text != imageOmittedPlaceholder {
		t.Errorf("degraded parts = %+v, want a single placeholder text part", parts2)
	}
}

// TestSniffImageMediaType checks the extension- and magic-byte-based media
// type inference used by attach_image.
func TestSniffImageMediaType(t *testing.T) {
	cases := []struct {
		path string
		data []byte
		want string
	}{
		{"a.png", nil, "image/png"},
		{"a.jpg", nil, "image/jpeg"},
		{"a.jpeg", nil, "image/jpeg"},
		{"noext", []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, "image/png"},
		{"noext", []byte{0xFF, 0xD8, 0xFF, 0xE0}, "image/jpeg"},
		{"noext", []byte("GIF89a...."), "image/gif"},
		{"noext", []byte("not an image"), "application/octet-stream"},
	}
	for _, tc := range cases {
		if got := sniffImageMediaType(tc.path, tc.data); got != tc.want {
			t.Errorf("sniffImageMediaType(%q, %v) = %q, want %q", tc.path, tc.data, got, tc.want)
		}
	}
}

// TestInitialUserMessageNoImagesUnchanged verifies the zero-images case is
// byte-for-byte the pre-multimodal shape: Content only, no Parts at all.
func TestInitialUserMessageNoImagesUnchanged(t *testing.T) {
	client := NewClient(&fakeTransport{})
	agent := NewAgent(Config{Goal: "plain goal", MaxTurns: 1}, client)

	msg := agent.initialUserMessage()
	if msg.Content != "plain goal" || len(msg.Parts) != 0 {
		t.Errorf("initialUserMessage() = %+v, want plain Content-only message", msg)
	}
}

// TestAttachImageRefusesOversized covers the guest-side attachment cap. Unlike
// host-attached images (capped by the host before the guest sees them), this
// path is model-driven, and an oversized attachment would be re-sent — and
// re-recorded — on every subsequent turn.
func TestAttachImageRefusesOversized(t *testing.T) {
	client := NewClient(&fakeTransport{})
	agent := NewAgent(Config{
		Goal:           "g",
		MaxTurns:       4,
		MaxAttachBytes: 64,
		Capabilities:   abi.Capabilities{Multimodal: true},
	}, client)

	big := make([]byte, 65)
	if err := agent.vfs.WriteFile("/work/big.png", big); err != nil {
		t.Fatal(err)
	}
	small := []byte("\x89PNG\r\n\x1a\n small")
	if err := agent.vfs.WriteFile("/work/ok.png", small); err != nil {
		t.Fatal(err)
	}

	args, _ := json.Marshal(map[string]string{"path": "/work/big.png"})
	out, parts, isErr := agent.tools.Invoke(context.Background(), "attach_image", args)
	if !isErr {
		t.Errorf("oversized attach: isErr = false, want true (out=%q)", out)
	}
	if len(parts) != 0 {
		t.Errorf("oversized attach returned %d parts, want 0", len(parts))
	}
	if !strings.Contains(out, "attachment cap") {
		t.Errorf("error = %q, want it to mention the cap", out)
	}

	args, _ = json.Marshal(map[string]string{"path": "/work/ok.png"})
	if _, parts, isErr := agent.tools.Invoke(context.Background(), "attach_image", args); isErr || len(parts) != 1 {
		t.Errorf("under-cap attach: isErr=%v parts=%d, want false/1", isErr, len(parts))
	}
}
