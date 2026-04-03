#!/bin/bash

imgname=entroq-goprotoc:local

cd "$(dirname "$0")"

imgid="$(docker image ls -q "$imgname")"
if [[ "$imgid" == "" ]]; then
  docker build -t "$imgname" .
fi

docker run --rm -v "$PWD":/src "$imgname" \
  bash -c "protoc -I/src -I/include \
    --go_out=/src --go_opt=paths=source_relative \
    --go-grpc_out=/src --go-grpc_opt=paths=source_relative \
    --connect-go_out=/src --connect-go_opt=paths=source_relative \
    /src/*.proto"
