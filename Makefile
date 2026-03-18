# Variables
REPO=registry.k8shell.io
REPORTS_DIR := reports
VENV := .venv
SERVICE_NAME := provisioner

# Default target
all: build

init:  ##@ Initialize Go module
       ##@ Ensures go.mod and go.sum are up to date with dependencies
	@echo "Initializing Go module..."
	go mod tidy
	go mod download

install-test-deps: ##@ Install test dependencies
                   ##@ Installs golangci-lint and gosec for static analysis
	@echo "Installing test dependencies..."
	@echo "Installing golangci-lint..."
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	@echo "Installing gosec..."
	go install github.com/securego/gosec/v2/cmd/gosec@latest
	@echo "Installing go-junit-report..."
	go install github.com/jstemmer/go-junit-report/v2@latest
	@mkdir -p $(REPORTS_DIR)

test-static: ##@ Run static analysis
             ##@ Runs linting and security checks on Go code
             ##@ Used in CI/CD workflow to catch code quality and security issues
test-static: install-test-deps
	@echo "Running golangci-lint..."
	golangci-lint run ./...
	@echo "Running gosec security scan for HIGH severity issues only..."
	gosec -exclude-generated -fmt=junit-xml -out=$(REPORTS_DIR)/gosec-junit.xml -severity high -quiet ./...
	@if [ ! -s $(REPORTS_DIR)/gosec-junit.xml ]; then echo '<?xml version="1.0" encoding="UTF-8"?><testsuites></testsuites>' > $(REPORTS_DIR)/gosec-junit.xml; fi
	@echo "Static analysis passed!"

test:       ##@ Run unit tests with coverage
            ##@ Validates code correctness through unit tests
            ##@ -count=1 disables test caching to ensure fresh execution in CI/CD
test: install-test-deps
	@echo "Running unit tests..."
	go test ./... -cover -coverprofile=$(REPORTS_DIR)/coverage.out -count=1 -v 2>&1 | go-junit-report -set-exit-code > $(REPORTS_DIR)/unit-junit.xml
	@echo "Unit tests passed!"

build:      ##@ Build binaries
	@echo "Building binaries..."
	go build -o bin/$(SERVICE_NAME) main.go
	@echo "Build complete!"

test-binary: ##@ Run binary smoke tests
             ##@ Validates that built binaries execute successfully (basic sanity check)
test-binary: build
	@echo "Running binary smoke tests..."
	@./bin/$(SERVICE_NAME) -h > /dev/null 2>&1 || (echo "$(SERVICE_NAME) help failed" && exit 1)
	@echo "$(SERVICE_NAME) smoke tests passed!"

test-self:  ##@ Run all self-tests
            ##@ Executes static analysis, unit tests, build, and binary smoke tests
            ##@ Validation of code quality and functionality (ran by self-tests CI workflow)
test-self: test-static build test-binary
	@echo "All self-tests passed!"

vendor:  ##@ Vendor Go modules
         ##@ Downloads and vendors all Go module dependencies into the vendor/ directory
	@echo "Vendoring Go modules..."
	@go mod vendor

image:  ##@ Build Docker image
        ##@ Builds container image with version tagging
        ##@ Accepts VERSION, COMMIT_ID, IMAGE_TAG from environment or auto-detects from git
image: vendor
	@echo "Building $(SERVICE_NAME) docker image..."
	@if ! command -v git >/dev/null 2>&1; then echo "Git not found. Please install Git."; exit 1; fi
	@VERSION=$${VERSION:-$$(git describe --tags --match 'v*' | sed 's/-g.*//')} && \
	COMMIT_ID=$${COMMIT_ID:-$$(git rev-parse --short HEAD)} && \
	IMAGE_TAG=$${IMAGE_TAG:-$$VERSION} && \
	docker build --build-arg VERSION=$$VERSION \
		--build-arg COMMIT_ID=$$COMMIT_ID -t $(REPO)/$$(cat docker/$(SERVICE_NAME)/BUILD):$$IMAGE_TAG . \
		-f docker/$(SERVICE_NAME)/Dockerfile

coverage:  ##@ Calculate test coverage percentage from coverage.out
	@go tool cover -func=$(REPORTS_DIR)/coverage.out | grep total | awk '{print $$3}'

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

##@
##@ Misc commands
##@

clean: ##@ Clean up generated files
	rm -rf $(REPORTS_DIR)
	rm -f bin/$(SERVICE_NAME)
	rm -rf vendor/

clean-all: ##@ Remove all generated files
clean-all: clean

help: ##@ (Default) Print listing of key targets with their descriptions
	@printf "\nUsage: make <command>\n"
	@grep -F -h "##@" $(MAKEFILE_LIST) | grep -F -v grep -F | sed -e 's/\\$$//' | awk 'BEGIN {FS = ":*[[:space:]]*##@[[:space:]]*"}; \
	{ \
		if($$2 == "") \
			printf ""; \
		else if($$0 ~ /^#/) \
			printf "\n%s\n", $$2; \
		else if($$1 == "") \
			printf "     %-20s%s\n", "", $$2; \
		else \
			printf "\n    \033[34m%-20s\033[0m %s\n", $$1, $$2; \
	}'
