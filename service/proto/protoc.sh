#!/bin/bash

cd "$(dirname "$0")"
protoc -I. --go_out=plugins=grpc:. ./*.proto
