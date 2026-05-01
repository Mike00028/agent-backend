package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	HTTPAddr       string
	PythonGRPCAddr string
	GRPCPoolSize   int
	GRPCTimeout    time.Duration
	GinMode        string

	// Ollama (Go-native planner + evaluator + embeddings)
	OllamaBaseURL string
	PlannerModel  string
	EvalModel     string
	EmbedModel    string

	// Postgres (DAG checkpointing + memory)
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

// Load reads configuration from environment variables with sensible defaults.
func Load() Config {
	return Config{
		HTTPAddr:       getEnv("HTTP_ADDR", ":8080"),
		PythonGRPCAddr: getEnv("PYTHON_GRPC_ADDR", "localhost:50051"),
		GRPCPoolSize:   getEnvInt("GRPC_POOL_SIZE", 5),
		GRPCTimeout:    getEnvDuration("GRPC_TIMEOUT_MS", 5000) * time.Millisecond,
		GinMode:        getEnv("GIN_MODE", "debug"),

		OllamaBaseURL: getEnv("OLLAMA_BASE_URL", "http://localhost:11434"),
		PlannerModel:  getEnv("PLANNER_MODEL", "qwen2.5:7b"),
		EvalModel:     getEnv("EVAL_MODEL", "qwen2.5:7b"),
		EmbedModel:    getEnv("EMBED_MODEL", "nomic-embed-text"),

		PostgresDSN: getEnv("POSTGRES_DSN", ""),

		RefinementMaxGeneration: getEnvInt("REFINEMENT_MAX_GENERATION", 2),
		MessageBatchThreshold:   getEnvInt("MESSAGE_BATCH_THRESHOLD", 15),
		MemoryFlushIntervalSec:  getEnvInt("MEMORY_FLUSH_INTERVAL_SEC", 1800),

		LangfuseOTLPEndpoint: getEnv("LANGFUSE_OTLP_ENDPOINT", "http://localhost:4318"),
		LangfusePublicKey:    getEnv("LANGFUSE_PUBLIC_KEY", ""),
		LangfuseSecretKey:    getEnv("LANGFUSE_SECRET_KEY", ""),
		OTELServiceName:      getEnv("OTEL_SERVICE_NAME", "go-orchestrator"),
	}
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
