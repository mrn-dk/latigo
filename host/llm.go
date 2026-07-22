package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mrn-dk/latigo/abi"
)

// LLMClient talks to an OpenAI-compatible /chat/completions endpoint. Mortise
// and other OpenAI-shaped gateways work unchanged.
type LLMClient struct {
	BaseURL    string
	APIKey     string
	Model      string
	HTTPClient *http.Client
}

// NewLLMClient builds a client. baseURL should be the API root, e.g.
// https://api.openai.com/v1 or a Mortise endpoint.
func NewLLMClient(baseURL, apiKey, model string) *LLMClient {
	return &LLMClient{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		APIKey:     apiKey,
		Model:      model,
		HTTPClient: &http.Client{Timeout: 120 * time.Second},
	}
}

// LLM registers the llm.call handler against this client.
func (h *Host) LLM(c *LLMClient) {
	h.Handle(abi.OpLLMCall, handler(c.call))
}

// ----- wire types (OpenAI chat completions) -----

type oaiToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type oaiTool struct {
	Type     string          `json:"type"`
	Function oaiToolFunction `json:"function"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content"`
	Name       string        `json:"name,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
}

type oaiRequest struct {
	Model       string       `json:"model"`
	Messages    []oaiMessage `json:"messages"`
	Tools       []oaiTool    `json:"tools,omitempty"`
	Temperature float64      `json:"temperature,omitempty"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
}

type oaiResponse struct {
	Choices []struct {
		Message      oaiMessage `json:"message"`
		FinishReason string     `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (c *LLMClient) call(ctx context.Context, req abi.LLMCallRequest) (abi.LLMCallResponse, error) {
	model := req.Model
	if model == "" {
		model = c.Model
	}
	wire := oaiRequest{
		Model:       model,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}
	for _, m := range req.Messages {
		om := oaiMessage{Role: m.Role, Content: m.Content, Name: m.Name, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			oc := oaiToolCall{ID: tc.ID, Type: "function"}
			oc.Function.Name = tc.Name
			oc.Function.Arguments = tc.Arguments
			om.ToolCalls = append(om.ToolCalls, oc)
		}
		wire.Messages = append(wire.Messages, om)
	}
	for _, t := range req.Tools {
		wire.Tools = append(wire.Tools, oaiTool{
			Type:     "function",
			Function: oaiToolFunction{Name: t.Name, Description: t.Description, Parameters: json.RawMessage(t.Parameters)},
		})
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return abi.LLMCallResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return abi.LLMCallResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return abi.LLMCallResponse{}, Errorf(abi.ErrInternal, "llm request: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return abi.LLMCallResponse{}, Errorf(abi.ErrInternal, "llm http %d: %s", resp.StatusCode, truncate(string(respBody), 400))
	}

	var out oaiResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return abi.LLMCallResponse{}, Errorf(abi.ErrInternal, "llm decode: %v", err)
	}
	if out.Error != nil {
		return abi.LLMCallResponse{}, Errorf(abi.ErrInternal, "llm error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return abi.LLMCallResponse{}, Errorf(abi.ErrInternal, "llm returned no choices")
	}
	choice := out.Choices[0]
	msg := abi.LLMMessage{Role: choice.Message.Role, Content: choice.Message.Content}
	for _, tc := range choice.Message.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, abi.LLMToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return abi.LLMCallResponse{
		Message:      msg,
		FinishReason: choice.FinishReason,
		InputTokens:  out.Usage.PromptTokens,
		OutputTokens: out.Usage.CompletionTokens,
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

var _ = fmt.Sprintf
