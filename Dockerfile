# =========================
# Multi-stage Dockerfile for Drip Server
# =========================
# Build:   docker build -t drip-server .
# Run:     docker run -p 80:80 -p 8443:8443 -e DRIP_DOMAIN=tunnel.example.com drip-server
# =========================

# =========================
# Builder stage
# =========================
FROM golang:1.25-alpine AS builder

# Install build dependencies
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    git

WORKDIR /build

# Copy go mod files and download dependencies
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Copy source code
COPY . .

# Build args for version info
ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown

# Build for multiple architectures (automatic with buildx)
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# Build the binary
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags "-s -w \
        -X main.Version=${VERSION} \
        -X main.GitCommit=${GIT_COMMIT} \
        -X main.BuildTime=${BUILD_TIME}" \
    -o /build/drip \
    ./cmd/drip

# Verify the binary
RUN /build/drip version --short

# =========================
# Runtime stage
# =========================
FROM alpine:latest

# Install runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    && update-ca-certificates

# Copy timezone data and SSL certificates from builder
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# Copy the binary
COPY --from=builder /build/drip /usr/local/bin/drip

# Create non-root user
RUN addgroup -g 1000 drip && \
    adduser -D -u 1000 -G drip drip && \
    mkdir -p /etc/drip /var/lib/drip/certs && \
    chown -R drip:drip /etc/drip /var/lib/drip

# Switch to non-root user
USER drip

# Set working directory
WORKDIR /var/lib/drip

# Expose ports
# 80: HTTP reverse proxy
# 8443: Main tunnel server port (TLS)
# 443: Alternative HTTPS port
# 20000-40000: Dynamic TCP tunnel ports (configure with --tcp-port-min/max)
EXPOSE 80 443 8443 20000-40000

# Environment variables with defaults
ENV DRIP_PORT=8443 \
    DRIP_DOMAIN=tunnel.localhost \
    DRIP_TOKEN="" \
    DRIP_TCP_PORT_MIN=20000 \
    DRIP_TCP_PORT_MAX=40000 \
    DRIP_TRANSPORTS="tcp,wss" \
    DRIP_TUNNEL_TYPES="http,https,tcp"

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD drip version --short || exit 1

# Default command: start server
# Override with environment variables or command-line flags
ENTRYPOINT ["drip"]
CMD ["server", \
    "--port", "${DRIP_PORT}", \
    "--domain", "${DRIP_DOMAIN}", \
    "--token", "${DRIP_TOKEN}", \
    "--tcp-port-min", "${DRIP_TCP_PORT_MIN}", \
    "--tcp-port-max", "${DRIP_TCP_PORT_MAX}", \
    "--transports", "${DRIP_TRANSPORTS}", \
    "--tunnel-types", "${DRIP_TUNNEL_TYPES}"]
