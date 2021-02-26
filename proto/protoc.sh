#!/bin/bash

cd "$(dirname "$0")"
protoc -I. --go_out=. --go-grpc_out=. ./*.proto
