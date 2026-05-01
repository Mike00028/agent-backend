package main

import (
"context"
"log/slog"
"os"

"github.com/gin-gonic/gin"
"github.com/mike00028/golang-backend/services/api/config"
"github.com/mike00028/golang-backend/services/api/handler"
"github.com/mike00028/golang-backend/services/api/internal/agentstore"
"github.com/mike00028/golang-backend/services/api/internal/dag"
"github.com/mike00028/golang-backend/services/api/internal/grpcclient"
"github.com/mike00028/golang-backend/services/api/internal/hitl"
"github.com/mike00028/golang-backend/services/api/internal/memory"
"github.com/mike00028/golang-backend/services/api/router"
)

func main() {
cfg := config.Load()
gin.SetMode(cfg.GinMode)

pool, err := grpcclient.New(cfg.GRPCPoolSize)
if err != nil {
slog.Error("failed to create gRPC pool", "error", err)
os.Exit(1)
}
defer pool.Close()

var (
cp         dag.CheckpointWriter = dag.NoopCheckpoint{}
agentStore *agentstore.Store
memorySvc  *memory.Service
)

if cfg.PostgresDSN != "" {
db, err := openPgPool(context.Background(), cfg.PostgresDSN)
if err != nil {
slog.Error("postgres connect failed", "error", err)
os.Exit(1)
}
defer db.Close()

cp = dag.NewPgCheckpoint(db)
agentStore = agentstore.New(db)
memorySvc = memory.New(db, cfg.OllamaBaseURL, cfg.EmbedModel)
slog.Info("postgres connected")
} else {
slog.Warn("POSTGRES_DSN not set; running with noop checkpoint and no memory/agent-store")
agentStore = agentstore.New(noopDB{})
}

hitlStore := hitl.NewStore()
chatHandler := handler.NewChatHandler(
pool, cp, agentStore, memorySvc,
hitlStore,
cfg.OllamaBaseURL, cfg.PlannerModel, cfg.EvalModel,
)
approveHandler := handler.NewApproveHandler(hitlStore)
r := router.New(chatHandler, approveHandler)

slog.Info("starting API server", "addr", cfg.HTTPAddr)
if err := r.Run(cfg.HTTPAddr); err != nil {
slog.Error("server error", "error", err)
os.Exit(1)
}
}
