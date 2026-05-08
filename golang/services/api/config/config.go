package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	HTTPAddr       string
	PythonGRPCAddr string
	GRPCPoolSize   int
	GRPCTimeout    time.Duration
	GinMode        string

	// LLM provider — "gemini" or "ollama" (default)
	LLMProvider  string
	GeminiAPIKey string

	// Ollama (Go-native planner + evaluator + embeddings)
	OllamaBaseURL string
	PlannerModel  string
	ChatModel     string // tool execution: chat_agent, summarize_agent
	EvalModel     string
	EmbedModel    string

	// Agent spec
	AgentSystemPrompt string
	AgentMaxIter      int
	AgentTools        []string

	// Postgres — conversations, MCP tools, agent registry (optional for local dev)
	PostgresDSN string

	// DAG orchestration tuning
	RefinementMaxGeneration int
	MessageBatchThreshold   int
	MemoryFlushIntervalSec  int

	// Observability
	LangfuseOTLPEndpoint string
	LangfusePublicKey    string
	LangfuseSecretKey    string
	OTELServiceName      string
}

// Load reads configuration from environment variables.
// Returns an error if any required variable is missing.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:       getEnv("HTTP_ADDR", ":8080"),
		PythonGRPCAddr: getEnv("PYTHON_GRPC_ADDR", "localhost:50051"),
		GRPCPoolSize:   getEnvInt("GRPC_POOL_SIZE", 5),
		GRPCTimeout:    getEnvDuration("GRPC_TIMEOUT_MS", 5000) * time.Millisecond,
		GinMode:        getEnv("GIN_MODE", "debug"),

		LLMProvider:  getEnv("LLM_PROVIDER", "gemini"),
		GeminiAPIKey: getEnv("GEMINI_API_KEY", ""),

		OllamaBaseURL: getEnv("OLLAMA_BASE_URL", "http://localhost:11434"),
		PlannerModel:  os.Getenv("PLANNER_MODEL"),
		ChatModel:     os.Getenv("CHAT_MODEL"),
		EvalModel:     os.Getenv("EVAL_MODEL"),
		EmbedModel:    getEnv("EMBED_MODEL", "nomic-embed-text"),

		AgentSystemPrompt: getEnv("AGENT_SYSTEM_PROMPT", "You are a helpful assistant."),
		AgentMaxIter:      getEnvInt("AGENT_MAX_ITERATIONS", 3),
		AgentTools:        strings.Fields(getEnv("AGENT_TOOLS", "chat_agent math_agent rag_agent summarize_agent text_agent")),

		PostgresDSN: getEnv("POSTGRES_DSN", ""),

		RefinementMaxGeneration: getEnvInt("REFINEMENT_MAX_GENERATION", 2),
		MessageBatchThreshold:   getEnvInt("MESSAGE_BATCH_THRESHOLD", 15),
		MemoryFlushIntervalSec:  getEnvInt("MEMORY_FLUSH_INTERVAL_SEC", 1800),

		LangfuseOTLPEndpoint: getEnv("LANGFUSE_OTLP_ENDPOINT", ""),
		LangfusePublicKey:    getEnv("LANGFUSE_PUBLIC_KEY", ""),
		LangfuseSecretKey:    getEnv("LANGFUSE_SECRET_KEY", ""),
		OTELServiceName:      getEnv("OTEL_SERVICE_NAME", "go-orchestrator"),
	}

	var missing []string
	for _, req := range []struct{ name, val string }{
		{"PLANNER_MODEL", cfg.PlannerModel},
		{"CHAT_MODEL", cfg.ChatModel},
		{"EVAL_MODEL", cfg.EvalModel},
	} {
		if req.val == "" {
			missing = append(missing, req.name)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvDuration(key string, fallbackMs int64) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Duration(n)
		}
	}
	return time.Duration(fallbackMs)
}
