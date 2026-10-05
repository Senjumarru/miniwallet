# syntax=docker/dockerfile:1

# Stage 1: Build binary
FROM golang:alpine AS builder

WORKDIR /src

# Allow compiling even if go.mod toolchain specifies a local/custom version
ENV GOTOOLCHAIN=local \
    CGO_ENABLED=0 \
    GOOS=linux

# Install build dependencies
RUN apk add --no-cache ca-certificates

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build statically-linked binary
RUN go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /app/miniwallet \
    ./cmd/server

# Stage 2: Minimal runtime image
FROM alpine:3.21

# Install ca-certificates and tzdata for TLS and timezone handling
RUN apk add --no-cache ca-certificates tzdata

# Create dedicated non-root user
RUN addgroup -g 10001 -S appgroup && \
    adduser -u 10001 -S appuser -G appgroup

WORKDIR /app

# Create data directory for SQLite database with proper permissions
RUN mkdir -p /app/data && chown -R appuser:appgroup /app

# Copy binary and migrations from builder
COPY --from=builder /app/miniwallet /app/miniwallet
COPY --from=builder /src/migrations /app/migrations

# Ensure appuser owns files
RUN chown -R appuser:appgroup /app

USER 10001:10001

ENV HOST="0.0.0.0" \
    PORT="8080" \
    DB_PATH="/app/data/miniwallet.db"

EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/ready > /dev/null || exit 1

ENTRYPOINT ["/app/miniwallet"]
