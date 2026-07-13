package api

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vidra/vidra-search/internal/config"
)

// opsRoutes are the operational probes intentionally excluded from the OpenAPI
// REST contract (mirrors vidra-core's treatment of /metrics and the AP routes).
var opsRoutes = map[string]bool{
	"GET /healthz": true,
	"GET /readyz":  true,
	"GET /version": true,
	"GET /metrics": true,
}

// testConfig builds a minimal, valid config for constructing a Server whose
// routing table the contract test inspects. No dependency is ever invoked.
func testConfig() *config.Config {
	return &config.Config{
		Environment:        "test",
		LogLevel:           "info",
		LogFormat:          "json",
		HTTPHost:           "127.0.0.1",
		HTTPPort:           8080,
		HTTPRequestTimeout: 10 * time.Second,
		HTTPBodyLimit:      "2M",
		DatabaseURL:        "postgres://x/y",
		RedisURL:           "redis://localhost:6379/0",
		InternalSecret:     "test-internal-secret-at-least-32-bytes-long",
	}
}

// TestOpenAPIContract is the documentation stop guard: it fails when the routes
// registered on the Echo router diverge from the operations declared in
// api/openapi.yaml. Ops probes are excluded on both sides. Keep code and
// contract in lock-step in the same change.
func TestOpenAPIContract(t *testing.T) {
	specPath := filepath.Join("..", "..", "api", "openapi.yaml")

	declared := declaredOperations(t, specPath)
	registered := registeredOperations(t)

	for op := range registered {
		if !declared[op] {
			t.Errorf("route %q is registered but NOT documented in api/openapi.yaml — document it in the same change", op)
		}
	}
	for op := range declared {
		if !registered[op] {
			t.Errorf("api/openapi.yaml documents %q but no route is registered — remove it from the spec or restore the route", op)
		}
	}

	if t.Failed() {
		t.Logf("registered routes:\n  %s", strings.Join(sortedKeys(registered), "\n  "))
		t.Logf("documented operations:\n  %s", strings.Join(sortedKeys(declared), "\n  "))
	}
}

var echoParam = regexp.MustCompile(`:([^/]+)`)

// registeredOperations returns the live "METHOD /path" set from the Echo router,
// minus the excluded ops probes, with path params normalised to OpenAPI braces.
func registeredOperations(t *testing.T) map[string]bool {
	t.Helper()
	srv := New(testConfig(), nil, nil, nil, nil, Services{})
	httpMethods := map[string]bool{
		"GET": true, "POST": true, "PUT": true, "PATCH": true,
		"DELETE": true, "HEAD": true, "OPTIONS": true,
	}
	ops := map[string]bool{}
	for _, r := range srv.Handler().Routes() {
		if !httpMethods[r.Method] || strings.Contains(r.Path, "*") {
			continue
		}
		op := r.Method + " " + echoParam.ReplaceAllString(r.Path, "{$1}")
		if opsRoutes[op] {
			continue
		}
		ops[op] = true
	}
	return ops
}

var specMethod = regexp.MustCompile(`^(get|post|put|patch|delete|head|options):\s*$`)

// declaredOperations parses api/openapi.yaml by indentation (no YAML dependency).
func declaredOperations(t *testing.T, specPath string) map[string]bool {
	t.Helper()
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read OpenAPI spec at %s: %v", specPath, err)
	}

	ops := map[string]bool{}
	inPaths := false
	current := ""
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case indent == 0:
			inPaths = trimmed == "paths:"
			current = ""
		case !inPaths:
			// outside the paths block
		case indent == 2 && strings.HasPrefix(trimmed, "/") && strings.HasSuffix(trimmed, ":"):
			current = strings.TrimSuffix(trimmed, ":")
		case indent == 4 && current != "":
			if m := specMethod.FindStringSubmatch(trimmed); m != nil {
				op := strings.ToUpper(m[1]) + " " + current
				if !opsRoutes[op] {
					ops[op] = true
				}
			}
		}
	}
	if len(ops) == 0 {
		t.Fatalf("no operations parsed from %s — check the file's indentation shape", specPath)
	}
	return ops
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
