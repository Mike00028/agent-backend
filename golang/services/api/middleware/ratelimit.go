package middleware

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

var (
	limiters sync.Map // map[string]*rate.Limiter keyed by IP
)

func getLimiter(ip string) *rate.Limiter {
	v, _ := limiters.LoadOrStore(ip, rate.NewLimiter(rate.Limit(50), 100))
	return v.(*rate.Limiter)
}

// RateLimit returns a per-IP rate limiter middleware (50 req/s, burst 100).
func RateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !getLimiter(c.ClientIP()).Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded"})
			return
		}
		c.Next()
	}
}
