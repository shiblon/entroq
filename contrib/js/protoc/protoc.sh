#!/bin/bash

cd "$(dirname "$0")"

imgname=entroq-jsprotoc:local
imgid="$(docker image ls -q "$imgname")"
if [[ "$imgid" == "" ]]; then
  echo "building"
  docker build -t "$imgname" .
fi

cd ../../..

echo "Running js/ts docker protoc build in $PWD"

protos=$(ls proto/*.proto)
docker run --rm -it -v "$PWD":/src "$imgname" pbjs -t static-module -p proto -w commonjs -o contrib/js/entroqpb.js $protos
docker run --rm -it -v "$PWD":/src "$imgname" pbts -o contrib/js/entroqpb.d.ts contrib/js/entroqpb.js

