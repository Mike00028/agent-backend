package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/mike00028/golang-backend/services/api/internal/telemetry"
)

// Logger returns a gin middleware that logs method, path, status, and latency.
func Logger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		// Start an OTel span for the full HTTP request.
		// The span is a child of any incoming traceparent header automatically.
		spanName := c.Request.Method + " " + c.FullPath()
		if spanName == " " {
			spanName = c.Request.Method + " " + c.Request.URL.Path
		}
		ctx, span := telemetry.NewTracer("middleware").Start(c.Request.Context(), spanName)
		c.Request = c.Request.WithContext(ctx)

		c.Next()

		latency := time.Since(start)
		status := c.Writer.Status()

		attrs := []telemetry.Attr{
			telemetry.StringAttr("http.method", c.Request.Method),
			telemetry.StringAttr("http.path", c.Request.URL.Path),
			telemetry.IntAttr("http.status_code", status),
			telemetry.StringAttr("http.client_ip", c.ClientIP()),
			telemetry.Int64Attr("http.latency_ms", latency.Milliseconds()),
		}
		if v, ok := c.Get("langfuse.input"); ok {
			if s, ok := v.(string); ok && s != "" {
				attrs = append(attrs, telemetry.StringAttr("langfuse.trace.input", s))
			}
		}
		span.SetAttr(attrs...)
		if status >= 500 {
			span.SetError(c.Errors.String())
		}
		span.End()

		slog.Info("request",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", status,
			"latency", latency.String(),
			"ip", c.ClientIP(),
		)
	}
}
