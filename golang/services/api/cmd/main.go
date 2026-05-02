package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/mike00028/golang-backend/services/api/config"
	"github.com/mike00028/golang-backend/services/api/handler"
	"github.com/mike00028/golang-backend/services/api/internal/agentstore"
	"github.com/mike00028/golang-backend/services/api/internal/dag"
	"github.com/mike00028/golang-backend/services/api/internal/grpcclient"
	"github.com/mike00028/golang-backend/services/api/internal/hitl"
	"github.com/mike00028/golang-backend/services/api/internal/llm"
	"github.com/mike00028/golang-backend/services/api/internal/llm/gemini"
	"github.com/mike00028/golang-backend/services/api/internal/planner"
	"github.com/mike00028/golang-backend/services/api/internal/telemetry"
	"github.com/mike00028/golang-backend/services/api/router"
)

func main() {
	// Load .env then allow .env.local to override for local development
	// (e.g. swap Docker-internal hostnames for localhost).
	// Both files are optional — no-op if missing.
	_ = godotenv.Load("../../../.env")
	_ = godotenv.Overload("../../../.env.local")

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config error", "error", err)
		os.Exit(1)
	}
	gin.SetMode(cfg.GinMode)

	// ── Observability ────────────────────────────────────────────────────────
	if cfg.LangfuseOTLPEndpoint != "" {
		shutdown, err := telemetry.Init(
			context.Background(),
			cfg.OTELServiceName,
			cfg.LangfuseOTLPEndpoint,
			cfg.LangfusePublicKey,
			cfg.LangfuseSecretKey,
		)
		if err != nil {
			slog.Warn("OTel init failed — tracing disabled", "error", err)
		} else {
			defer shutdown(context.Background())
			slog.Info("OTel tracing enabled", "endpoint", cfg.LangfuseOTLPEndpoint)
		}
	}

	pool, err := grpcclient.New(cfg.GRPCPoolSize)
	if err != nil {
		slog.Error("failed to create gRPC pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	// ── Postgres (optional — required for conversations, MCP tools, agent registry) ──
	if cfg.PostgresDSN != "" {
		db, err := openPgPool(context.Background(), cfg.PostgresDSN)
		if err != nil {
			slog.Warn("postgres connection failed — running without DB", "error", err)
		} else {
			defer db.Close()
			slog.Info("postgres connected")
			_ = db // TODO: wire to conversation store, MCP tool registry
		}
	}

	// ── Agent spec from config ────────────────────────────────────────────────
	agentSpec := &agentstore.AgentSpec{
		ID:               "default",
		Name:             "Default Agent",
		AgentType:        "react",
		SystemPrompt:     cfg.AgentSystemPrompt,
		MaxIterations:    cfg.AgentMaxIter,
		Tools:            cfg.AgentTools,
		EvaluatorEnabled: true,
		IsPublic:         true,
		MemoryPolicy:     agentstore.DefaultMemoryPolicy,
	}
	agentStore := agentstore.New(agentSpec)
	cp := dag.CheckpointWriter(dag.NoopCheckpoint{})

	hitlStore := hitl.NewStore()

	// ── LLM provider (DIP — Planner/Evaluator depend on llm.Client interface) ──
	var llmClient llm.Client
	switch cfg.LLMProvider {
	case "ollama":
		llmClient = planner.NewOllamaClient(cfg.OllamaBaseURL)
		slog.Info("LLM provider: ollama", "url", cfg.OllamaBaseURL)
	default: // "gemini"
		if cfg.GeminiAPIKey == "" {
			slog.Error("GEMINI_API_KEY is required when LLM_PROVIDER=gemini")
			os.Exit(1)
		}
		llmClient = gemini.New(cfg.GeminiAPIKey)
		slog.Info("LLM provider: gemini", "model", cfg.PlannerModel)
	}

	chatHandler := handler.NewChatHandler(
		pool, cp, agentStore, nil,
		hitlStore,
		llmClient, cfg.PlannerModel, cfg.ChatModel, cfg.EvalModel,
	)
	approveHandler := handler.NewApproveHandler(hitlStore)
	r := router.New(chatHandler, approveHandler)

	slog.Info("starting API server", "addr", cfg.HTTPAddr)
	if err := r.Run(cfg.HTTPAddr); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
