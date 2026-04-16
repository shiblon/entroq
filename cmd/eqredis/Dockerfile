# 1. Build inside a Go container.
FROM golang:1.25-bookworm AS builder

ENV CGO_ENABLED=0

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download -x

ARG VERSION=dev
COPY . .
RUN go build -ldflags "-X github.com/shiblon/entroq/pkg/version.Version=${VERSION}" -o /bin/eqredis ./cmd/eqredis

# 2. Switch to a secure, minimal runtime container.
FROM gcr.io/distroless/static-debian12:debug

LABEL org.opencontainers.image.source="https://github.com/shiblon/entroq"

USER 65532:65532

COPY --from=builder /bin/eqredis /bin/eqredis

# gRPC endpoint
EXPOSE 37706
# Prometheus / HTTP endpoint
EXPOSE 9100

ENTRYPOINT ["/bin/eqredis"]
CMD ["serve"]
