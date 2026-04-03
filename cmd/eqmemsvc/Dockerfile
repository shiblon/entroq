# Build inside a Go container.
FROM golang:1.25-alpine3.23 AS builder

ENV CGO_ENABLED=0

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download -x

COPY . .
RUN go build -o /bin/eqmemsvc ./cmd/eqmemsvc

# Switch to a smaller runtime container.
FROM alpine:3.23

RUN apk --no-cache add ca-certificates openssl curl bash jq
RUN mkdir -p /data/entroq/journal && chmod -R a+rwx /data/entroq

COPY --from=builder /bin/eqmemsvc /bin/eqmemsvc

RUN adduser -S -D -H -h /src -u 100 appuser
USER appuser

# Journal location
VOLUME /data/entroq

# gRPC endpoint
EXPOSE 37706
# Prometheus endpoint
EXPOSE 9100

ENTRYPOINT ["/bin/eqmemsvc"]
CMD ["--journal", "/data/entroq/journal", "--mkdir", "--periodic_snapshot", "1h", "--journal_cleanup"]
