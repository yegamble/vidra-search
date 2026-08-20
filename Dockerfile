# syntax=docker/dockerfile:1

# ---- build stage ----
FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache module downloads separately from source for faster rebuilds.
COPY go.mod go.sum ./
RUN go mod download

# The whole tree, migrations included: they are compiled into the binary
# (migrations/embed.go), which is what lets `docker run <image> migrate up` work
# with no golang-migrate CLI and no bind-mounted checkout (the arguments append
# to the ENTRYPOINT below).
COPY . .

# Build metadata baked into internal/version so a running container can answer
# "which build is this?" (GET /version, plus the startup log line).
# The package path and the three variable names MUST stay identical to
# Makefile's LDFLAGS — -ldflags -X matches on the fully-qualified symbol, so a
# typo silently no-ops instead of failing the build. Note BUILD_DATE maps onto
# version.Date (the arg is named for what it is; the symbol is what the linker
# needs). Defaults are EMPTY on purpose: an empty arg is skipped below, so a
# plain `docker build` / `make up` keeps the Go-side defaults in
# internal/version/version.go exactly as before. CI (publish-container.yml)
# supplies the release tag, short SHA and tag-commit timestamp.
ARG VERSION=""
ARG COMMIT=""
ARG BUILD_DATE=""

# Static binary so it runs on a minimal final image.
RUN set -eu; \
    pkg="github.com/vidra/vidra-search/internal/version"; \
    ldflags="-s -w"; \
    if [ -n "$VERSION" ]; then ldflags="$ldflags -X $pkg.Version=$VERSION"; fi; \
    if [ -n "$COMMIT" ]; then ldflags="$ldflags -X $pkg.Commit=$COMMIT"; fi; \
    if [ -n "$BUILD_DATE" ]; then ldflags="$ldflags -X $pkg.Date=$BUILD_DATE"; fi; \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="$ldflags" -o /out/api ./cmd/api

# ---- runtime stage ----
FROM alpine:3.24
RUN apk add --no-cache ca-certificates wget && adduser -D -u 10001 vidra

USER vidra
WORKDIR /app
COPY --from=build /out/api /app/api

EXPOSE 8080
# Liveness check used by Compose/orchestrators.
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/app/api"]
