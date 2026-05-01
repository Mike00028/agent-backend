package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OllamaClient makes structured-output chat requests to Ollama.
type OllamaClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewOllamaClient creates an OllamaClient.
func NewOllamaClient(baseURL string) *OllamaClient {
	return &OllamaClient{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// Message is a single chat turn.
type Message struct {
	Role    string `json:"role"` // "system" | "user" | "assistant"
	Content string `json:"content"`
}

// chatRequest is the Ollama /api/chat request body.
type chatRequest struct {
	Model    string      `json:"model"`
	Messages []Message   `json:"messages"`
	Format   interface{} `json:"format,omitempty"` // JSON Schema for structured output
	Stream   bool        `json:"stream"`
}

// chatResponse is the Ollama /api/chat response body (non-streaming).
type chatResponse struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Done bool `json:"done"`
}

// Chat sends a chat request to Ollama.
// Pass a non-nil schema to force structured JSON output.
func (c *OllamaClient) Chat(ctx context.Context, model string, messages []Message, schema interface{}) (string, error) {
	body := chatRequest{
		Model:    model,
		Messages: messages,
		Stream:   false,
	}
	if schema != nil {
		body.Format = schema
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("ollama HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	return result.Message.Content, nil
}

// ChatInto sends a structured-output chat request and unmarshals the result into dst.
// dst must be a pointer to a struct that matches the registered schema.
func (c *OllamaClient) ChatInto(ctx context.Context, model string, messages []Message, dst interface{}) error {
	schema := SchemaOf(dst)
	content, err := c.Chat(ctx, model, messages, schema)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(content), dst); err != nil {
		return fmt.Errorf("unmarshal structured output: %w\nraw: %s", err, content)
	}
	return nil
}
