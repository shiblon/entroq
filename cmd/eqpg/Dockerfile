# 1. Build inside a modern Go container.
FROM golang:1.25-bookworm AS builder

ENV CGO_ENABLED=0

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download -x

ARG VERSION=dev
COPY . .
RUN go build -ldflags "-X github.com/shiblon/entroq/pkg/version.Version=${VERSION}" -o /bin/eqpg ./cmd/eqpg

# 2. Switch to a secure, minimal runtime container.
# The ":debug" version includes a busybox shell for easier "docker exec" debugging.
FROM gcr.io/distroless/static-debian12:debug

# Note: Distroless images already include CA bundles and common timezone data.
# The distroless "nonroot" user ID is 65532.
# However, the "static" image doesn't define a default USER, so we can set it.
USER 65532:65532

COPY --from=builder /bin/eqpg /bin/eqpg

# gRPC endpoint
EXPOSE 37706
# Prometheus endpoint
EXPOSE 9100

ENTRYPOINT ["/bin/eqpg"]
