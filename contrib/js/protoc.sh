#!/bin/bash

cd "$(dirname "$0")"
protodir='../../proto'

npm install protoc-gen-grpc ts-protoc-gen grpc

tsgen='node_modules/ts-protoc-gen/bin/protoc-gen-ts'
rpcgen='node_modules/protoc-gen-grpc/bin/protoc-gen-grpc.js'
rpctsgen='node_modules/protoc-gen-grpc/bin/protoc-gen-grpc-ts.js'

protoc -I "$protodir" --plugin="protoc-gen-ts=$tsgen" --js_out=import_style=commonjs,binary:. --ts_out=. "$protodir"/*.proto
$rpcgen --js_out=import_style=commonjs,binary:. --grpc_out=. --proto_path="$protodir" "$protodir"/*.proto
$rpctsgen --ts_out=service=true:. --proto_path="$protodir" "$protodir"/*.proto
