FROM golang:1.24-bookworm AS gotools

ENV GOBIN=/bin
ENV protoc_version="3.14.0"
ENV grpc_version="1.35.0"

ENV go_proto_version="v1.23.0"
ENV go_grpc_version="v1.1.0"

WORKDIR /

RUN apt-get update \
 && apt-get install -y curl unzip \
 && arch=$(uname -m) \
 && if [ "$arch" = "x86_64" ]; then protoc_arch="x86_64"; else protoc_arch="aarch_64"; fi \
 && curl -Lo protoc.zip "https://github.com/protocolbuffers/protobuf/releases/download/v${protoc_version}/protoc-${protoc_version}-linux-${protoc_arch}.zip" \
 && unzip protoc.zip \
 && go install "google.golang.org/protobuf/cmd/protoc-gen-go@${go_proto_version}" \
 && go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${go_grpc_version}" \
 && go install "connectrpc.com/connect/cmd/protoc-gen-connect-go@latest" \
 && go install "github.com/google/gnostic/cmd/protoc-gen-openapi@v0.7.0" \
 && mkdir -p /include/google/api \
 && curl -Lo /include/google/api/annotations.proto https://raw.githubusercontent.com/googleapis/googleapis/master/google/api/annotations.proto \
 && curl -Lo /include/google/api/http.proto https://raw.githubusercontent.com/googleapis/googleapis/master/google/api/http.proto

FROM debian:bookworm-slim

RUN mkdir -p /bin /include
COPY --from=gotools /bin/* /bin/
COPY --from=gotools /include /include

WORKDIR /src
VOLUME /src
