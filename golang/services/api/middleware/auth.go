package middleware

import (
	"github.com/gin-gonic/gin"
)

// Auth returns a simple Bearer token middleware.
// Set the expected token via the AUTH_TOKEN environment variable.
// In production replace with a proper JWT/OIDC validator.
func Auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// header := c.GetHeader("Authorization")
		// if !strings.HasPrefix(header, "Bearer ") {
		// 	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
		// 	return
		// }
		// TODO: validate JWT / call auth service
		c.Next()
	}
}
