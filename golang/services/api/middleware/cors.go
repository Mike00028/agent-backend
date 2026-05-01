package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// CORS enables basic cross-origin access for browser clients.
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		allowed := map[string]bool{
			"http://localhost:8000": true,
			"http://127.0.0.1:8000": true,
			"http://localhost:8080": true,
			"http://127.0.0.1:8080": true,
		}
		if !allowed[origin] {
			origin = ""
		}

		header := c.Writer.Header()
		if origin != "" {
			header.Set("Access-Control-Allow-Origin", origin)
		}
		header.Set("Vary", "Origin")
		header.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		header.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-ID")
		header.Set("Access-Control-Allow-Credentials", "true")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}
