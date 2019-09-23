#!/bin/bash

cd "$(dirname "$0")"
protodir='../../proto'

tmp=$(mktemp -d)
python3 -m venv "$tmp"
source "$tmp/bin/activate"
pip install --upgrade pip
pip install 'grpcio==1.15.0'
pip install 'grpcio-health-checking==1.15.0'
pip install 'grpcio-tools==1.15.0'
pip install 'protobuf==3.6.1'

python -m grpc_tools.protoc -I"${protodir}" --python_out=./entroq/ --grpc_python_out=./entroq/ "${protodir}"/*.proto
