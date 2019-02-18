#!/bin/bash

cd "$(dirname "$0")"
protoc -I. --go_out=plugins=grpc:. ./*.proto
cp entrogo.com/entroq/proto/entroq.pb.go .
rm -rf entrogo.com
