#!/bin/bash

imgname=entroq-goprotoc:local

cd "$(dirname "$0")"

imgid="$(docker image ls -q "$imgname")"
if [[ "$imgid" == "" ]]; then
  docker build -t "$imgname" .
fi

docker run --rm -it -v "$PWD":/src "$imgname" \
  bash -c "protoc -I/src --go_out=/src --go-grpc_out=/src /src/*.proto"
