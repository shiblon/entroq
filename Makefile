
sources := $(shell find . -name '*.go') proto/entroq.pb.go

black	:= $(shell tput setaf 0)
red	:= $(shell tput setaf 1)
green	:= $(shell tput setaf 2)
yellow	:= $(shell tput setaf 3)
blue	:= $(shell tput setaf 4)
magenta	:= $(shell tput setaf 5)
cyan	:= $(shell tput setaf 6)
white	:= $(shell tput setaf 7)
bold	:= $(shell tput bold)
uline	:= $(shell tput smul)
reset	:= $(shell tput sgr0)

help: ## Print help for targets with comments.
	@echo "Usage:"
	@echo "  make [target...]"
	@echo ""
	@echo "Useful commands:"
	@grep -Eh '^[a-zA-Z._-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  $(cyan)%-30s$(reset) %s\n", $$1, $$2}'
	@echo ""
	@echo "Useful variables:"
	@awk 'BEGIN { FS = ":=" } /^## /{x = substr($$0, 4); getline; if (NF >= 2) printf "  $(cyan)%-30s$(reset) %s\n", $$1, x}' $(MAKEFILE_LIST) | sort
	@echo ""
	@echo "Typical usage:"
	@printf "  $(cyan)%s$(reset)\n    %s\n\n" \
		"make test" "Run all unit tests." \
		"make genproto" "Re-build protobuf libraries" \
		"make build" "Build $(PROJECT_NAME) binaries" \
		"make install" "Install $(PROJECT_NAME) binary to local GOBIN directory" \
		"make image" "Build Docker images for the $(PROJECT_NAME)" \
		"make clean" "Clear protobuf libraries, build artifacts, and application binary"


proto/entroq.pb.go: proto/entroq.proto
	proto/protoc.sh

# default task to run the full suite
.PHONY: default
default: build test

.PHONY: build
build: genproto
	go build -v ./...

.PHONY: test
test:
	go test -timeout 20m -race -v ./...

.PHONY: install
install:
	go install -v ./...

.PHONY: clean
clean:
	go clean -i -x  ./...
	rm -f proto/*.pb.go

.PHONY: image
image: Dockerfile $(sources)
	docker build -f $< -t 'entroq:latest' .

.PHONY: genproto
genproto: proto/entroq.pb.go

# disallow any parallelism (-j) for Make. This is necessary since some
# commands during the build process create temporary files that collide
# under parallel conditions.
.NOTPARALLEL:
