#!/bin/bash

cd "$(dirname "$0")"
protoc -I. --go-grpc_out=. ./*.proto
