FROM golang:alpine

RUN mkdir -p /go/{src,pkg,bin}
RUN mkdir -p /go/src/github.com/shiblon/entroq

ENV GOPATH /go
ENV PATH ${PATH}:/go/bin

WORKDIR /go/src/github.com/shiblon/entroq

COPY . .

RUN go get -d -v ./...
RUN go install -v ./...

ENTRYPOINT ["bash", "cmd/eqsvc.sh"]

# Defalts to starting up an in-memory queue service on the default port.
# Other options include "pg" with its associated flags.
# If flags are left off, or the command is left off, the default in-memory
# service is started.
CMD ["mem", "-port", "37706"]
