// Package agent implements the framework-free agent loop: the LLM plans via
// strict JSON tool-calls, Go executes typed tools, results feed back until
// the model emits a final reply. A deterministic planner takes over if the
// LLM fails — the demo never dies.
package agent

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

// Msg mirrors session.Msg without an import (kept local for client ergonomics).
type Msg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client is anything that can chat. Mocked in tests.
type Client interface {
	Chat(ctx context.Context, system string, msgs []Msg) (string, error)
}

// HFClient talks to an OpenAI-compatible chat endpoint (HuggingFace router).
type HFClient struct {
	BaseURL string // e.g. https://router.huggingface.co
	APIKey  string
	Model   string
	HTTP    *http.Client
}

type hfChatRequest struct {
	Model       string        `json:"model"`
	Messages    []hfMessage   `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
	Stop        []string      `json:"stop,omitempty"`
}

type hfMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type hfChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error string `json:"error,omitempty"`
}

// Chat sends system + messages and returns the assistant content.
func (h *HFClient) Chat(ctx context.Context, system string, msgs []Msg) (string, error) {
	if h.HTTP == nil {
		h.HTTP = &http.Client{Timeout: 45 * time.Second}
	}
	all := make([]hfMessage, 0, len(msgs)+1)
	all = append(all, hfMessage{Role: "system", Content: system})
	for _, m := range msgs {
		all = append(all, hfMessage{Role: m.Role, Content: m.Content})
	}
	body, err := json.Marshal(hfChatRequest{
		Model:       h.Model,
		Messages:    all,
		MaxTokens:   700,
		Temperature: 0.2,
		Stop:        []string{"\n\nuser:", "</s>"},
	})
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(h.BaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+h.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm status %d: %.200s", resp.StatusCode, raw)
	}
	var out hfChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("llm decode: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("llm error: %s", out.Error)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("llm returned no choices")
	}
	content := strings.TrimSpace(out.Choices[0].Message.Content)
	if content == "" {
		return "", fmt.Errorf("llm returned empty content (finish=%s)", out.Choices[0].FinishReason)
	}
	return content, nil
}

// MockClient returns scripted responses; used by the agent tests.
type MockClient struct {
	Fn func(system string, msgs []Msg) (string, error)
}

// Chat implements Client.
func (m *MockClient) Chat(_ context.Context, system string, msgs []Msg) (string, error) {
	return m.Fn(system, msgs)
}
