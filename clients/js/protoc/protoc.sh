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

protos=$(ls api/*.proto)
docker run --rm -v "$PWD":/src "$imgname" pbjs -t static-module -p /src/api -w commonjs -o /src/clients/js/entroqpb.js $protos
docker run --rm -v "$PWD":/src "$imgname" pbts -o /src/clients/js/entroqpb.d.ts /src/clients/js/entroqpb.js

