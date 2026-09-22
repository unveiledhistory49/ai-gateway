# Multi-stage production build for AI Gateway
# Builds a minimal, unprivileged container containing only the compiled product binary.

# Stage 1: Build binary
FROM golang:1.23-alpine AS builder

RUN apk --no-cache add ca-certificates git

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o ai-gateway ./cmd/gateway

# Stage 2: Production runtime image
FROM alpine:3.20

RUN apk --no-cache add ca-certificates tzdata

# Run as non-root unprivileged service user
RUN adduser -D -u 10001 -s /sbin/nologin gateway \
    && mkdir -p /etc/ai-gateway /var/log/ai-gateway \
    && chown -R gateway:gateway /etc/ai-gateway /var/log/ai-gateway

WORKDIR /app

# Copy only the compiled product binary and default configuration
COPY --from=builder /build/ai-gateway /app/ai-gateway
COPY config.example.yaml /etc/ai-gateway/config.yaml

USER gateway

EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider http://127.0.0.1:8080/healthz/liveness || exit 1

ENTRYPOINT ["/app/ai-gateway"]
CMD ["-config", "/etc/ai-gateway/config.yaml"]
