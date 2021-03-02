#!/bin/bash

cd "$(dirname "$0")"

imgname=entroq-pyprotoc:local
imgid="$(docker image ls -q "$imgname")"
if [[ "$imgid" == "" ]]; then
  docker build -t "$imgname" .
fi

target_dir=contrib/py/entroq

cd ../../..
echo "Now in directory $PWD, running docker"

docker run --rm -it -v "$PWD":/src "$imgname" \
  bash -c "python -m grpc_tools.protoc -Iproto --python_out="$target_dir"/ --grpc_python_out="$target_dir"/ proto/*.proto && sed -i'' -e 's/^import entroq_pb2 as/from . import entroq_pb2 as/' '$target_dir/entroq_pb2_grpc.py'"

