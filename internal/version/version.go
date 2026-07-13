// Package version exposes build metadata for the vidra-search binary. The values
// are overridable at build time via -ldflags, e.g.:
//
//	go build -ldflags "-X github.com/vidra/vidra-search/internal/version.Version=1.2.3 \
//	  -X github.com/vidra/vidra-search/internal/version.Commit=$(git rev-parse --short HEAD) \
//	  -X github.com/vidra/vidra-search/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// They default to sensible development values so the binary is always
// self-describing even without ldflags.
package version

import "runtime"

var (
	// Version is the semantic version of the build.
	Version = "0.1.0"
	// Commit is the short git SHA the build was produced from.
	Commit = "unknown"
	// Date is the UTC build timestamp (RFC 3339).
	Date = "unknown"
)

// GoVersion reports the Go toolchain version the binary was compiled with.
func GoVersion() string { return runtime.Version() }
