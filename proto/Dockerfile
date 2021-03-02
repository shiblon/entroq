FROM golang:1.16-buster AS gotools

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
 && go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${go_grpc_version}"

FROM debian:buster-slim

RUN mkdir -p /bin
COPY --from=gotools /bin/* /bin/

WORKDIR /src
VOLUME /src
