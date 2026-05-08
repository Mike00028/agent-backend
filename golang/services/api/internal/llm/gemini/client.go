// Package gemini implements llm.Client using the Google Gemini REST API.
//
// Model: gemini-2.5-flash — cheapest Gemini model with native reasoning
// (thinking) and function-calling support.
//
// Wire-up (main.go):
//
//	client := gemini.New(cfg.GeminiAPIKey)
//	handler.NewChatHandler(..., client, ...)
package gemini

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

const apiBase = "https://generativelanguage.googleapis.com/v1beta/models"

// Client implements llm.Client via the Gemini generateContent REST API.
// It is safe for concurrent use.
type Client struct {
	apiKey         string
	httpClient     *http.Client
	tracer         telemetry.Tracer
	thinkingBudget int // 0 = disabled (default); set via WithThinkingBudget
}

// New creates a Gemini Client with thinking disabled (fastest, cheapest).
func New(apiKey string) *Client {
	return &Client{
		apiKey:         apiKey,
		httpClient:     &http.Client{Timeout: 120 * time.Second},
		tracer:         telemetry.NewTracer("llm.gemini"),
		thinkingBudget: 0,
	}
}

// WithThinkingBudget returns a copy of the client with thinking tokens capped at n.
// Only applies to gemini-2.5-* models. Use when step-by-step reasoning helps (e.g. complex planning).
func (c *Client) WithThinkingBudget(n int) *Client {
	copy := *c
	copy.thinkingBudget = n
	return &copy
}

// ── Gemini REST wire types ────────────────────────────────────────────────────

type part struct {
	Text string `json:"text"`
}

type gContent struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type generationConfig struct {
	ResponseMimeType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   interface{}     `json:"responseSchema,omitempty"`
	ThinkingConfig   *thinkingConfig `json:"thinkingConfig,omitempty"`
}

type thinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget"`
}

type generateRequest struct {
	SystemInstruction *gContent         `json:"system_instruction,omitempty"`
	Contents          []gContent        `json:"contents"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

type candidate struct {
	Content gContent `json:"content"`
}

type usageMeta struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
}

type generateResponse struct {
	Candidates    []candidate `json:"candidates"`
	UsageMetadata usageMeta   `json:"usageMetadata"`
	// Error is non-nil when Gemini returns a non-200 body as JSON.
	Error *struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"error,omitempty"`
}

// ── llm.Client implementation ─────────────────────────────────────────────────

// Chat sends messages to the Gemini generateContent API and returns the text.
// Pass a non-nil schema (llm.JSONSchema) to force structured JSON output.
func (c *Client) Chat(ctx context.Context, model string, messages []llm.Message, schema interface{}) (string, error) {
	ctx, span := c.tracer.Start(ctx, "gemini.chat",
		telemetry.StringAttr("gen_ai.request.model", model),
		telemetry.StringAttr("gen_ai.system", "gemini"),
		telemetry.IntAttr("llm.messages", len(messages)),
		telemetry.BoolAttr("llm.structured", schema != nil),
	)
	defer span.End()

	reqBody := c.buildRequest(model, messages, schema)
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal gemini request: %w", err)
	}

	url := fmt.Sprintf("%s/%s:generateContent", apiBase, model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build gemini request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		span.RecordError(err)
		span.SetError(err.Error())
		return "", fmt.Errorf("gemini request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read gemini response: %w", err)
	}

	var result generateResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode gemini response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("gemini HTTP %d", resp.StatusCode)
		if result.Error != nil {
			msg = fmt.Sprintf("gemini error %d: %s", result.Error.Code, result.Error.Message)
		}
		err := fmt.Errorf("%s", msg)
		span.RecordError(err)
		span.SetError(err.Error())
		return "", err
	}

	if len(result.Candidates) == 0 || len(result.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini returned empty response")
	}

	output := result.Candidates[0].Content.Parts[0].Text

	// Format full prompt for Langfuse input — system + user messages are both
	// valuable for debugging; only recording the last user message hides the
	// system prompt entirely, making it impossible to understand agent behaviour.
	var inputParts []string
	for _, m := range messages {
		inputParts = append(inputParts, "["+m.Role+"]: "+m.Content)
	}
	inputText := strings.Join(inputParts, "\n\n")

	span.SetAttr(
		telemetry.StringAttr("gen_ai.request.model", model),
		telemetry.IntAttr("gen_ai.usage.input_tokens", result.UsageMetadata.PromptTokenCount),
		telemetry.IntAttr("gen_ai.usage.output_tokens", result.UsageMetadata.CandidatesTokenCount),
		telemetry.IntAttr("gen_ai.usage.thoughts_tokens", result.UsageMetadata.ThoughtsTokenCount),
		telemetry.StringAttr("langfuse.observation.input", inputText),
		telemetry.StringAttr("langfuse.observation.output", output),
	)

	return output, nil
}

// ChatInto calls Chat with a JSON schema derived from dst, then unmarshals
// the response into dst.
func (c *Client) ChatInto(ctx context.Context, model string, messages []llm.Message, dst interface{}) error {
	schema := llm.SchemaOf(dst)
	text, err := c.Chat(ctx, model, messages, schema)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(text), dst); err != nil {
		return fmt.Errorf("unmarshal gemini structured output: %w\nraw: %s", err, text)
	}
	return nil
}

// ── Request builder ───────────────────────────────────────────────────────────

// buildRequest converts llm.Messages into the Gemini wire format.
// "system" role is lifted into system_instruction; "assistant" is mapped to "model".
// thinkingConfig is always sent for gemini-2.5-* using the client's budget (0 = disabled).
func (c *Client) buildRequest(model string, messages []llm.Message, schema interface{}) generateRequest {
	req := generateRequest{}

	var contents []gContent
	for _, m := range messages {
		switch m.Role {
		case "system":
			req.SystemInstruction = &gContent{
				Parts: []part{{Text: m.Content}},
			}
		default:
			role := m.Role
			if role == "assistant" {
				role = "model"
			}
			contents = append(contents, gContent{
				Role:  role,
				Parts: []part{{Text: m.Content}},
			})
		}
	}
	req.Contents = contents

	if schema != nil || strings.HasPrefix(model, "gemini-2.5") {
		gc := &generationConfig{}
		if schema != nil {
			gc.ResponseMimeType = "application/json"
			gc.ResponseSchema = sanitizeSchema(schema)
		}
		if strings.HasPrefix(model, "gemini-2.5") {
			gc.ThinkingConfig = &thinkingConfig{ThinkingBudget: c.thinkingBudget}
		}
		req.GenerationConfig = gc
	}
	return req
}

// sanitizeSchema removes fields unsupported by the Gemini response schema API.
// Gemini rejects `additionalProperties` — strip it recursively via map round-trip.
func sanitizeSchema(schema interface{}) interface{} {
	// Marshal → generic map → strip → return cleaned map.
	b, err := json.Marshal(schema)
	if err != nil {
		return schema
	}
	var m interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		return schema
	}
	return stripUnsupported(m)
}

func stripUnsupported(v interface{}) interface{} {
	switch node := v.(type) {
	case map[string]interface{}:
		delete(node, "additionalProperties")
		for k, val := range node {
			node[k] = stripUnsupported(val)
		}
		return node
	case []interface{}:
		for i, item := range node {
			node[i] = stripUnsupported(item)
		}
		return node
	default:
		return v
	}
}
