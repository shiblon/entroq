FROM python:3-slim-buster

ENV protoc_version="3.14.0"
ENV grpc_version="1.35.0"

RUN apt-get update \
 && apt-get install -y sed

RUN python -m pip install --upgrade pip \
 && python -m pip install \
     "grpcio==${grpc_version}" \
     "grpcio-tools==${grpc_version}" \
     "grpcio-health-checking==${grpc_version}" \
     "protobuf==${protoc_version}"

WORKDIR /src
VOLUME /src
