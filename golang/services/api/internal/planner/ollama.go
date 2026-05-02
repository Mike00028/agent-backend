package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mike00028/golang-backend/services/api/internal/llm"
	"github.com/mike00028/golang-backend/services/api/internal/telemetry"
)

// OllamaClient makes structured-output chat requests to Ollama.
type OllamaClient struct {
	BaseURL    string
	HTTPClient *http.Client
	tracer     telemetry.Tracer
}

// NewOllamaClient creates an OllamaClient.
func NewOllamaClient(baseURL string) *OllamaClient {
	return &OllamaClient{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		tracer: telemetry.NewTracer("planner.ollama"),
	}
}

// chatRequest is the Ollama /api/chat request body.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []llm.Message `json:"messages"`
	Format   interface{}   `json:"format,omitempty"` // JSON Schema for structured output
	Stream   bool          `json:"stream"`
}

// chatResponse is the Ollama /api/chat response body (non-streaming).
type chatResponse struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Done               bool `json:"done"`
	PromptEvalCount    int  `json:"prompt_eval_count"`
	EvalCount          int  `json:"eval_count"`
}

// Chat sends a chat request to Ollama.
// Pass a non-nil schema to force structured JSON output.
func (c *OllamaClient) Chat(ctx context.Context, model string, messages []llm.Message, schema interface{}) (string, error) {
	inputText := ""
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			inputText = messages[i].Content
			break
		}
	}
	ctx, span := c.tracer.Start(ctx, "ollama.chat",
		telemetry.StringAttr("gen_ai.request.model", model),
		telemetry.StringAttr("gen_ai.system", "ollama"),
		telemetry.IntAttr("llm.messages", len(messages)),
		telemetry.BoolAttr("llm.structured", schema != nil),
		telemetry.StringAttr("langfuse.input", inputText),
	)
	defer span.End()
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
		err := fmt.Errorf("ollama HTTP %d: %s", resp.StatusCode, string(body))
		span.RecordError(err)
		span.SetError(err.Error())
		return "", err
	}

	var result chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		span.RecordError(err)
		span.SetError(err.Error())
		return "", fmt.Errorf("decode response: %w", err)
	}

	span.SetAttr(
		telemetry.IntAttr("llm.response.bytes", len(result.Message.Content)),
		telemetry.IntAttr("gen_ai.usage.input_tokens", result.PromptEvalCount),
		telemetry.IntAttr("gen_ai.usage.output_tokens", result.EvalCount),
		telemetry.StringAttr("langfuse.output", result.Message.Content),
	)
	return result.Message.Content, nil
}

// ChatInto sends a structured-output chat request and unmarshals the result into dst.
// dst must be a pointer to a struct that matches the registered schema.
func (c *OllamaClient) ChatInto(ctx context.Context, model string, messages []llm.Message, dst interface{}) error {
	schema := llm.SchemaOf(dst)
	content, err := c.Chat(ctx, model, messages, schema)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(content), dst); err != nil {
		return fmt.Errorf("unmarshal structured output: %w\nraw: %s", err, content)
	}
	return nil
}

// OllamaSummarizer satisfies dag.SummarizerClient using a direct LLM call.
type OllamaSummarizer struct {
	client llm.Client
	model  string
}

// NewSummarizer creates an OllamaSummarizer backed by any llm.Client.
func NewSummarizer(client llm.Client, model string) *OllamaSummarizer {
	return &OllamaSummarizer{client: client, model: model}
}

// Summarize combines multiple task outputs into one coherent response.
func (s *OllamaSummarizer) Summarize(ctx context.Context, userMessage string, taskOutputs []string) (string, error) {
	combined := strings.Join(taskOutputs, "\n\n---\n\n")
	sys := "You are a synthesis assistant. Combine the task results below into a single, " +
		"clear, well-structured response for the user. Be concise. Do not repeat yourself."
	user := "Original question: " + userMessage + "\n\nTask results:\n" + combined
	return s.client.Chat(ctx, s.model, []llm.Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: user},
	}, nil)
}
