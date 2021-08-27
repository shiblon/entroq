# Inspired by https://www.cloudreach.com/blog/containerize-this-golang-dockerfiles/

# Build inside a Go container.
FROM golang:1.16-alpine3.13 as builder

ENV GOBIN /build/bin
ENV CGO_ENABLED 0

RUN mkdir -p /src/github.com/shiblon/entroq
WORKDIR /src/github.com/shiblon/entroq

COPY go.mod go.sum /src/github.com/shiblon/entroq/
RUN go mod download -x

COPY . /src/github.com/shiblon/entroq/
RUN go install -v ./...

# Switch to a smaller container without build tools.
FROM alpine:latest

RUN apk --no-cache add ca-certificates openssl curl bash jq
RUN mkdir -p /go/bin

ENV PATH ${PATH}:/go/bin

COPY --from=builder /build/bin/* /go/bin/
COPY cmd/eqsvc.sh /go/bin/
WORKDIR /go/bin

# Memory journal location, if needed.
RUN mkdir -p /data/entroq/journal && chmod -R a+rwx /data

RUN adduser -S -D -H -h /go/src/github.com/shiblon/entroq -u 100 appuser
USER appuser

VOLUME /data/entroq

# gRPC endpoint
EXPOSE 37706

# Prometheus endpoint
EXPOSE 9100

ENTRYPOINT ["./eqsvc.sh"]

# Defalts to starting up an in-memory queue service on the default port.
# Other options include "pg" with its associated flags.
# If flags are left off, or the command is left off, the default in-memory
# service is started.
CMD ["mem", "--journal", "/data/entroq/journal", "--mkdir", "--periodic_snapshot", "1h", "--journal_cleanup"]
