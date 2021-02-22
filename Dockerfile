# Inspired by https://www.cloudreach.com/blog/containerize-this-golang-dockerfiles/

# Build inside a Go container.
FROM golang:1.16-alpine3.13 as builder

ENV GOPATH /build
ENV CGO_ENABLED 0

COPY . $GOPATH/src/entrogo.com/entroq
WORKDIR $GOPATH/src/entrogo.com/entroq

RUN apk add git
RUN go get -d -v ./... && go install -v ./...

# Switch to a smaller container without build tools.
FROM alpine:latest

RUN apk --no-cache add ca-certificates openssl curl bash jq
RUN mkdir -p /go/bin

ENV PATH ${PATH}:/go/bin

COPY --from=builder /build/bin/* /go/bin/
COPY cmd/eqsvc.sh /go/bin/
WORKDIR /go/bin

RUN adduser -S -D -H -h /go/src/entrogo.com/entroq -u 100 appuser
USER appuser

# gRPC endpoint
EXPOSE 37706

# Prometheus endpoint
EXPOSE 9100

ENTRYPOINT ["./eqsvc.sh"]

# Defalts to starting up an in-memory queue service on the default port.
# Other options include "pg" with its associated flags.
# If flags are left off, or the command is left off, the default in-memory
# service is started.
CMD ["mem"]
