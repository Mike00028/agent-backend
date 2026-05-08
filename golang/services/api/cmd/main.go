package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/mike00028/golang-backend/services/api/config"
	"github.com/mike00028/golang-backend/services/api/handler"
	"github.com/mike00028/golang-backend/services/api/internal/agentstore"
	"github.com/mike00028/golang-backend/services/api/internal/dag"
	internaldb "github.com/mike00028/golang-backend/services/api/internal/db"
	"github.com/mike00028/golang-backend/services/api/internal/grpcclient"
	"github.com/mike00028/golang-backend/services/api/internal/hitl"
	"github.com/mike00028/golang-backend/services/api/internal/llm"
	"github.com/mike00028/golang-backend/services/api/internal/llm/gemini"
	"github.com/mike00028/golang-backend/services/api/internal/mcptools"
	"github.com/mike00028/golang-backend/services/api/internal/memory"
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
	if err := cfg.Validate(); err != nil {
		slog.Error("config validation failed", "error", err)
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

	// ── Postgres (conversations, agent registry, checkpoints) ────────────────
	var chatStore *internaldb.ChatStore
	var cp dag.CheckpointWriter = dag.NoopCheckpoint{}
	var pgPool *pgAdapter
	pgvectorOK := false
	if cfg.PostgresDSN != "" {
		var err error
		pgPool, err = openPgPool(context.Background(), cfg.PostgresDSN)
		if err != nil {
			slog.Warn("postgres connection failed — running without DB", "error", err)
		} else {
			defer pgPool.Close()
			slog.Info("postgres connected")

			// Run schema migrations on every startup (idempotent).
			// Pass the DSN directly — pool.Ping() already proved credentials work.
			if err := internaldb.Migrate(cfg.PostgresDSN); err != nil {
				slog.Error("db migration failed", "error", err)
				os.Exit(1)
			}

			// Warn if pgvector is missing — memory will be disabled gracefully.
			pgvectorOK = true
			if err := internaldb.CheckPgvector(context.Background(), pgPool.pool); err != nil {
				slog.Warn("pgvector not available — memory service will be disabled", "error", err)
				pgvectorOK = false
			}

			chatStore = internaldb.NewChatStore(&chatDB{pool: pgPool.pool})
			slog.Info("chat store ready")
		}
	}
	_ = chatStore // wired to handlers in a future step

	// ── MCP tool manager (requires Postgres) ──────────────────────────────────
	var mcpMgr *mcptools.Manager
	if pgPool != nil {
		mcpStore := mcptools.NewStore(pgPool.pool)
		embedder := mcptools.NewOllamaEmbedder(cfg.OllamaBaseURL, cfg.EmbedModel)
		mcpMgr = mcptools.NewManager(mcpStore, embedder)
		defer mcpMgr.Close()

		// Start periodic tool sync (every 5 minutes)
		mcpMgr.StartPeriodicSync(context.Background(), 5*time.Minute)
		slog.Info("MCP tool manager ready")
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

	hitlStore := hitl.NewStore()

	// ── Memory service (semantic retrieval + write-back via pgvector) ─────────
	var memorySvc *memory.Service
	if pgPool != nil && pgvectorOK && cfg.EmbedModel != "" && cfg.OllamaBaseURL != "" {
		memorySvc = memory.New(pgPool, cfg.OllamaBaseURL, cfg.EmbedModel)
		slog.Info("memory service ready",
			"embed_model", cfg.EmbedModel,
			"ollama_url", cfg.OllamaBaseURL,
		)
	}

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
		pool, cp, agentStore, memorySvc,
		hitlStore,
		mcpMgr,
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
