
sources := $(shell find . -name '*.go') proto/entroq.pb.go

help: ## Print help for targets with comments.
	@echo "Usage:"
	@echo "  make [target...]"
	@echo ""
	@echo "Useful commands:"
	@grep -Eh '^[a-zA-Z._-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  $(cyan)%-30s$(term-reset) %s\n", $$1, $$2}'
	@echo ""
	@echo "Useful variables:"
	@awk 'BEGIN { FS = ":=" } /^## /{x = substr($$0, 4); getline; if (NF >= 2) printf "  $(cyan)%-30s$(term-reset) %s\n", $$1, x}' $(MAKEFILE_LIST) | sort
	@echo ""
	@echo "Typical usage:"
	@printf "  $(cyan)%s$(term-reset)\n    %s\n\n" \
		"make test" "Run all unit tests." \
		"make build-proto" "Re-build protobuf libraries" \
		"make build" "Build $(PROJECT_NAME) binaries" \
		"make install" "Install $(PROJECT_NAME) binary to local GOBIN directory" \
		"make image" "Build Docker images for the $(PROJECT_NAME)" \
		"make clean" "Clear protobuf libraries, build artifacts, and application binary"


proto/entroq.pb.go: proto/entroq.proto
	proto/protoc.sh

# default task to run the full suite
default: get-deps build test

get-deps:
	dep ensure --update

build: build-proto
	go build -v ./...

test:
	go test -v ./...

install:
	go install -v ./...

clean:
	go clean -i -x  ./...
	rm -f proto/*.pb.go

image: Dockerfile $(sources)
	docker build -f $< -t 'entroq:latest' .

build-proto: proto/entroq.pb.go

# disallow any parallelism (-j) for Make. This is necessary since some
# commands during the build process create temporary files that collide
# under parallel conditions.
.NOTPARALLEL:

.PHONY: default get-deps build build-proto test clean help image