# Build inside a Go container.
FROM golang:alpine as builder

RUN mkdir -p /build/{src,pkg,bin}
RUN mkdir -p /build/src/github.com/shiblon/entroq

ENV GOPATH /build

WORKDIR /build/src/github.com/shiblon/entroq

COPY . .

RUN go get -d -v ./...
RUN go install -v ./...

# Switch to a smaller container without build tools.
FROM alpine

#RUN adduser -S -D -H -h /go/src/github.com/shiblon/entroq appuser
#USER appuser

RUN mkdir -p /go/bin

ENV PATH ${PATH}:/go/bin

COPY --from=builder /build/bin/* /go/bin/
COPY cmd/eqsvc.sh /go/bin/
WORKDIR /go/bin

ENTRYPOINT ["./eqsvc.sh"]

# Defalts to starting up an in-memory queue service on the default port.
# Other options include "pg" with its associated flags.
# If flags are left off, or the command is left off, the default in-memory
# service is started.
CMD ["mem", "-port", "37706"]
