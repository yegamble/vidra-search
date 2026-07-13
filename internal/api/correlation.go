package api

import (
	"strings"

	"github.com/labstack/echo/v4"
)

// correlationHeader carries a cross-service correlation id. vidra-core sends it
// on every internal call; vidra-search accepts, sanitizes, and echoes it so a UI
// action and its downstream search request share one id. Use this exact header
// name on both sides.
const correlationHeader = "X-Correlation-ID"

// maxCorrelationIDLen bounds an inbound (untrusted) correlation id.
const maxCorrelationIDLen = 128

// correlationID accepts an inbound X-Correlation-ID (minting one from the
// server-generated request id when absent), sanitizes it (untrusted client
// input), echoes it on the response, and stashes it on the request context for
// the request logger. Must run after middleware.RequestID and before the logger.
func correlationID() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			cid := sanitizeCorrelationID(c.Request().Header.Get(correlationHeader))
			if cid == "" {
				cid = c.Response().Header().Get(echo.HeaderXRequestID)
			}
			c.Response().Header().Set(correlationHeader, cid)
			c.Set(correlationContextKey, cid)
			return next(c)
		}
	}
}

// correlationContextKey is the Echo context key under which the sanitized
// correlation id is stored for the request logger.
const correlationContextKey = "correlation_id"

// correlationFromContext returns the correlation id bound to the Echo context.
func correlationFromContext(c echo.Context) string {
	if v, ok := c.Get(correlationContextKey).(string); ok {
		return v
	}
	return ""
}

// sanitizeCorrelationID keeps only URL-safe token characters and bounds the
// length, so an inbound header can never inject CR/LF into a response header or
// arbitrary content into a log line.
func sanitizeCorrelationID(s string) string {
	if len(s) > maxCorrelationIDLen {
		s = s[:maxCorrelationIDLen]
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}
