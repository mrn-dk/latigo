// Package main: openai.go defines the OpenAI-compatible wire types Latigo uses
// to talk to any OpenAI-compatible endpoint. Latigo speaks only this dialect;// the endpoint translates to whatever backends it fronts, if any.
package main

import "encoding/json"

// ContentPart is one element of a message's structured content: a text span or
// an image. This is the provider-agnostic content model — both OpenAI and
// Anthropic represent a message's content as an array of typed parts, so it
// maps onto whichever dialect the endpoint speaks.
type ContentPart struct {
	Type     string    `json:"type"`                // "text" | "image_url"
	Text     string    `json:"text,omitempty"`      // when Type=="text"
	ImageURL *ImageURL `json:"image_url,omitempty"` // when Type=="image_url"
}

// ImageURL is the OpenAI image_url content part shape. Data may be a remote
// URL or a data: URL carrying inline base64 bytes.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"` // "auto" | "low" | "high"
}

// Message is one chat message in an OpenAI-compatible exchange.
//
// Content is the plain-text shorthand, always populated for text-only
// messages. Parts is the optional structured multimodal form: when non-empty
// it is authoritative and takes precedence over Content. Parts is omitempty so
// text-only exchanges and event logs are unaffected.
type Message struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
	Parts      []ContentPart `json:"parts,omitempty"`
	Name       string        `json:"name,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
}

// ToolCall is a model-requested tool invocation.
type ToolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type,omitempty"` // "function"
	Function FuncCall `json:"function"`
}

// FuncCall carries the called function name and its raw JSON arguments.
type FuncCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded arguments
}

// ToolSpec advertises a callable tool to the model.
type ToolSpec struct {
	Type     string   `json:"type"` // "function"
	Function FuncSpec `json:"function"`
}

// FuncSpec is the function declaration sent to the model.
type FuncSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
}

// chatRequest is the POST /chat/completions body.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []messageWire `json:"messages"`
	Tools       []ToolSpec    `json:"tools,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

// messageWire is the on-the-wire message shape. Tool-role messages carry
// content + tool_call_id; assistant messages with tool calls carry
// tool_calls; ordinary messages carry either a string content or an array of
// content parts (multimodal).
type messageWire struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// chatResponse is the relevant subset of the OpenAI completions response.
type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Model string `json:"model"`
	// Error is populated by the endpoint on failure (OpenAI error shape). Empty on
	// success.
	Error *struct {
		Type    string `json:"type,omitempty"`
		Message string `json:"message,omitempty"`
		Code    string `json:"code,omitempty"`
	} `json:"error,omitempty"`
}
