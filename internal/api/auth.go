package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

// internalAuthHeader is the HMAC authentication header vidra-core presents on
// every /internal/v1 call.
const internalAuthHeader = "X-Vidra-Internal-Auth"

// internalAuthSkew bounds the accepted clock skew between the two services.
const internalAuthSkew = 120 * time.Second

// InternalSignature computes the hex HMAC-SHA256 over "ts\nMETHOD\nPATH" using
// the shared secret. Exported so tests (and a reference client) can build valid
// headers with the exact same construction the middleware verifies.
func InternalSignature(secret string, ts int64, method, path string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10) + "\n" + method + "\n" + path))
	return hex.EncodeToString(mac.Sum(nil))
}

// BuildInternalAuthHeader returns the full "v1:{ts}:{sig}" header value.
func BuildInternalAuthHeader(secret string, ts int64, method, path string) string {
	return "v1:" + strconv.FormatInt(ts, 10) + ":" + InternalSignature(secret, ts, method, path)
}

// internalAuth authenticates /internal/v1 requests. It parses the versioned
// header, rejects a stale timestamp (replay window ±120s), and constant-time
// compares the signature computed over ts\nMETHOD\nPATH. Any failure is a 401
// with the standard envelope; the reason is never disclosed to the caller.
func internalAuth(secret string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			raw := c.Request().Header.Get(internalAuthHeader)
			if raw == "" {
				return unauthorized(c)
			}
			parts := strings.SplitN(raw, ":", 3)
			if len(parts) != 3 || parts[0] != "v1" {
				return unauthorized(c)
			}
			ts, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return unauthorized(c)
			}
			skew := time.Since(time.Unix(ts, 0))
			if skew < 0 {
				skew = -skew
			}
			if skew > internalAuthSkew {
				return unauthorized(c)
			}
			expected := InternalSignature(secret, ts, c.Request().Method, c.Request().URL.Path)
			if !hmac.Equal([]byte(parts[2]), []byte(expected)) {
				return unauthorized(c)
			}
			return next(c)
		}
	}
}

func unauthorized(c echo.Context) error {
	return writeError(c, http.StatusUnauthorized, "unauthorized", "invalid or missing internal authentication")
}
