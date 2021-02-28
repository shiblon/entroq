FROM python:3-slim-buster

ENV protoc_version="3.14.0"
ENV grpc_version="1.35.0"

RUN apt-get update \
 && apt-get install -y curl unzip sed \
 && curl -Lo protoc.zip "https://github.com/protocolbuffers/protobuf/releases/download/v${protoc_version}/protoc-${protoc_version}-linux-x86_64.zip" \
 && unzip protoc.zip

RUN python -m pip install --upgrade pip \
 && python -m pip install \
     "grpcio==${grpc_version}" \
     "grpcio-tools==${grpc_version}" \
     "grpcio-health-checking==${grpc_version}"

WORKDIR /src
VOLUME /src
