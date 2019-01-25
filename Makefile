
sources := $(shell find . -name '*.go') proto/entroq.pb.go

proto/entroq.pb.go: proto/entroq.proto
	proto/protoc.sh

.PHONY: image
image: Dockerfile $(sources)
	docker build -f $< -t 'entroq:latest' .

.PHONY: proto
proto: proto/entroq.pb.go
