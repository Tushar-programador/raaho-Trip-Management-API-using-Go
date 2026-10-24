# syntax=docker/dockerfile:1

# ---- build ------------------------------------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src

# Download modules in their own layer so code changes don't refetch them.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
# Static binaries (no cgo) so the runtime image needs no libc.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/apikey ./cmd/apikey

# ---- runtime ----------------------------------------------------------------
# Distroless: no shell or package manager, runs as uid 65532 (nonroot).
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/api /out/migrate /out/apikey /app/

USER nonroot:nonroot
ENV HTTP_ADDR=:8080
EXPOSE 8080

# The binary probes its own /readyz, since the image has no curl or wget.
HEALTHCHECK --interval=10s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/app/api", "healthcheck"]

ENTRYPOINT ["/app/api"]
