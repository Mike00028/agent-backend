// Package llm defines the provider-agnostic LLM interface used by this service.
//
// Dependency-inversion rule: all consumer packages (planner, evaluator, handler)
// import only this package.  Concrete backends live in sub-packages (ollama,
// gemini) and are wired in main.go — consumer code never references them.
package llm

import "context"

// Message is a single chat turn shared by all LLM providers.
type Message struct {
	Role    string `json:"role"`    // "system" | "user" | "assistant"
	Content string `json:"content"` // Message text
}

// Client abstracts LLM text-generation providers.
// All implementations must be safe for concurrent use.
type Client interface {
	// Chat sends a conversation and returns the raw text response.
	// Pass a non-nil schema to request structured JSON output.
	Chat(ctx context.Context, model string, messages []Message, schema interface{}) (string, error)

	// ChatInto calls Chat with a JSON schema derived from dst, then unmarshals
	// the structured JSON response into dst (must be a pointer).
	ChatInto(ctx context.Context, model string, messages []Message, dst interface{}) error
}
