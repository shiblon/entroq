# 1. Build inside a Go container.
FROM golang:1.24-bookworm AS builder

ENV CGO_ENABLED=0

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download -x

COPY . .
RUN go build -o /bin/eqmemsvc ./cmd/eqmemsvc

# Create data directory for persistency with correct non-root permissions
RUN mkdir -p /data/entroq/journal && chown -R 65532:65532 /data

# 2. Switch to a secure, minimal runtime container.
FROM gcr.io/distroless/static-debian12:debug

# Non-root user ID 65532:65532 (default nonroot)
USER 65532:65532

# Copy data directory with correct ownership
COPY --from=builder --chown=65532:65532 /data /data
COPY --from=builder /bin/eqmemsvc /bin/eqmemsvc

# Journal location
VOLUME /data/entroq

# gRPC endpoint
EXPOSE 37706
# Prometheus endpoint
EXPOSE 9100

ENTRYPOINT ["/bin/eqmemsvc"]
CMD ["--journal", "/data/entroq/journal", "--mkdir", "--periodic_snapshot", "1h", "--journal_cleanup"]
