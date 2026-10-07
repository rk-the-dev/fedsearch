package nl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LLM is the one capability the NL layer needs: produce an emit_query call.
type LLM interface {
	EmitQuery(ctx context.Context, system string, conv []Message) (*ToolCall, error)
}

type Message struct {
	Role    string  `json:"role"`
	Content []Block `json:"content"`
}

type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type ToolCall struct {
	ID    string
	Input json.RawMessage
}

// Anthropic calls the Messages API with the emit_query tool forced, so the
// model must answer in the IR shape.
type Anthropic struct {
	APIKey  string
	Model   string
	BaseURL string
	HTTP    *http.Client
}

func (a *Anthropic) EmitQuery(ctx context.Context, system string, conv []Message) (*ToolCall, error) {
	base := a.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	client := a.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	body := map[string]any{
		"model":       a.Model,
		"max_tokens":  2048,
		"temperature": 0,
		"system":      system,
		"messages":    conv,
		"tools": []any{map[string]any{
			"name":         toolName,
			"description":  "Emit the FedSearch IR query that answers the analyst's question.",
			"input_schema": toolSchema(),
		}},
		"tool_choice": map[string]any{"type": "tool", "name": toolName},
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/messages", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("anthropic %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	var out struct {
		Content []Block `json:"content"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	for _, c := range out.Content {
		if c.Type == "tool_use" && c.Name == toolName {
			return &ToolCall{ID: c.ID, Input: c.Input}, nil
		}
	}
	return nil, fmt.Errorf("model did not call %s", toolName)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
