# 1. Build inside a Go container.
FROM golang:1.25-bookworm AS builder

ENV CGO_ENABLED=0

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download -x

ARG VERSION=dev
COPY . .
RUN go build -ldflags "-X github.com/shiblon/entroq/pkg/version.Version=${VERSION}" -o /bin/eqmem ./cmd/eqmem

# Create data directory for persistence with correct non-root permissions.
RUN mkdir -p /data/entroq/journal && chown -R 65532:65532 /data

# 2. Switch to a secure, minimal runtime container.
FROM gcr.io/distroless/static-debian12:debug

LABEL org.opencontainers.image.source="https://github.com/shiblon/entroq"

USER 65532:65532

COPY --from=builder --chown=65532:65532 /data /data
COPY --from=builder /bin/eqmem /bin/eqmem

VOLUME /data/entroq

# gRPC endpoint
EXPOSE 37706
# Prometheus / HTTP endpoint
EXPOSE 9100

ENTRYPOINT ["/bin/eqmem"]
CMD ["serve", "--journal", "/data/entroq/journal", "--mkdir", "--periodic_snapshot", "1h", "--journal_cleanup"]
