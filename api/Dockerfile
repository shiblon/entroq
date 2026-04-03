FROM --platform=linux/amd64 golang:1.24-bookworm AS gotools

ENV GOBIN=/bin
ENV protoc_version="3.14.0"
ENV grpc_version="1.35.0"

ENV go_proto_version="v1.23.0"
ENV go_grpc_version="v1.1.0"

WORKDIR /

RUN apt-get update \
 && apt-get install -y curl unzip \
 && curl -Lo protoc.zip "https://github.com/protocolbuffers/protobuf/releases/download/v${protoc_version}/protoc-${protoc_version}-linux-x86_64.zip" \
 && unzip protoc.zip \
 && go install "google.golang.org/protobuf/cmd/protoc-gen-go@${go_proto_version}" \
 && go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${go_grpc_version}" \
 && go install "connectrpc.com/connect/cmd/protoc-gen-connect-go@latest" \
 && mkdir -p /include/google/api \
 && curl -Lo /include/google/api/annotations.proto https://raw.githubusercontent.com/googleapis/googleapis/master/google/api/annotations.proto \
 && curl -Lo /include/google/api/http.proto https://raw.githubusercontent.com/googleapis/googleapis/master/google/api/http.proto

FROM --platform=linux/amd64 debian:bookworm-slim

RUN mkdir -p /bin /include
COPY --from=gotools /bin/* /bin/
COPY --from=gotools /include /include

WORKDIR /src
VOLUME /src
