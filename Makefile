# Variables
GOOS_LIST := linux 
GOARCH_LIST := amd64 arm64
REPO=fitcr.ksi.in.fit.cvut.cz

# Default target
all: build

# Initialize Go module
init:
	@echo "Initializing Go module..."
	go mod tidy

image:
	@echo "Provisioner docker image"
	@rm -fr docker/provisioner/files
	@mkdir -p docker/provisioner/files
	@echo "Downloading vendor modules..."
	@go mod vendor -o docker/provisioner/files/vendor
	@echo "Building image..."
	@version=$$(git describe --tags --match '*' | sed 's/-g.*//') && \
	cp -r go.mod go.sum pkg internal main.go docker/provisioner/files && \
	cd docker/provisioner && docker build --build-arg VERSION=$$version \
		--build-arg COMMIT_ID=$$(git rev-parse --short HEAD) -t $(REPO)/$$(cat ./BUILD):$$version .

COMMON_DIR := $(shell go list -m -f '{{.Dir}}' github.com/k8shell-io/common)

proto-setup:
	mkdir -p .proto_deps
	@rm -f .proto_deps/common
	ln -s $(COMMON_DIR) .proto_deps/common

protoc:
	@echo "Generating Go code from proto files..."
	rm -rf pkg/api/provisionerpb
	protoc -I . -I .proto_deps \
	  --go_out=. --go_opt=module=github.com/k8shell-io/provisioner \
	  --go-grpc_out=. --go-grpc_opt=module=github.com/k8shell-io/provisioner \
	  pkg/api/provisioner.proto