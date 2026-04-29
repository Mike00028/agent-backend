package main

import (
	"log/slog"
	"os"

	"github.com/mike00028/golang-backend/services/api/config"
	"github.com/mike00028/golang-backend/services/api/handler"
	"github.com/mike00028/golang-backend/services/api/internal/grpcclient"
	"github.com/mike00028/golang-backend/services/api/router"

	"github.com/gin-gonic/gin"
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

	chatHandler := handler.NewChatHandler(pool)
	r := router.New(chatHandler)

	slog.Info("starting API server", "addr", cfg.HTTPAddr)
	if err := r.Run(cfg.HTTPAddr); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}
