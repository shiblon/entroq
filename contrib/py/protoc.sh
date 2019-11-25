#!/bin/bash

cd "$(dirname "$0")"
protodir='../../proto'

tmp=$(mktemp -d)
python3 -m venv "$tmp"
source "$tmp/bin/activate"
pip install --upgrade pip
pip install 'grpcio==1.25.0'
pip install 'grpcio-health-checking==1.25.0'
pip install 'grpcio-tools==1.25.0'
pip install 'protobuf==3.10.0'

python -m grpc_tools.protoc -I"${protodir}" --python_out=./entroq/ --grpc_python_out=./entroq/ "${protodir}"/*.proto
# Fix up the stupid non-relative import that grpc output makes.
sed -i '' -e 's/import entroq_pb2 as/from . import entroq_pb2 as/' ./entroq/entroq_pb2_grpc.py
