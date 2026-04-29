package router

import (
	"github.com/gin-gonic/gin"
	"github.com/mike00028/golang-backend/services/api/handler"
	"github.com/mike00028/golang-backend/services/api/middleware"
)

// New builds and returns the gin.Engine with all routes registered.
func New(chat *handler.ChatHandler) *gin.Engine {
	r := gin.New()
	r.Use(middleware.Logger())
	r.Use(gin.Recovery())
	r.Use(middleware.RateLimit())

	// Public
	r.GET("/healthz", handler.Health)

	// Authenticated API routes
	api := r.Group("/", middleware.Auth())
	{
		// SSE streaming chat endpoint
		api.POST("/chat", chat.Stream)

		// Unary JSON invoke endpoint
		api.POST("/agent/invoke", chat.Invoke)
	}

	return r
}
