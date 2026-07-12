# Variables
REPO=registry.k8shell.io
REPORTS_DIR := reports
# Auto-derived from the repo directory name. Override if needed: make SERVICE_NAME=myservice
SERVICE_NAME ?= $(shell basename $(CURDIR))
RUNTIME ?= alpine

.PHONY: all init install-test-deps test-static test build test-binary test-self vendor image image-debug image-release reload dlv debug-setup coverage clean help

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
	CGO_ENABLED=0 go build -gcflags="all=-N -l" -o bin/$(SERVICE_NAME) main.go
	@echo "Build complete!"

test-binary: ##@ Run binary smoke tests
             ##@ Validates that built binaries execute successfully (basic sanity check)
test-binary: build
	@echo "Running binary smoke tests..."
	@./bin/$(SERVICE_NAME) -h > /dev/null 2>&1 || (echo "$(SERVICE_NAME) help failed" && exit 1)
	@echo "$(SERVICE_NAME) smoke tests passed!"

test-self:  ##@ Run all self-tests
            ##@ Executes static analysis, unit tests, build, and binary smoke tests
            ##@ Validation of code quality and functionality (ran by CI workflow)
test-self: test-static build test-binary
	@echo "All self-tests passed!"

vendor:  ##@ Vendor Go modules
         ##@ Downloads and vendors all Go module dependencies into the vendor/ directory
	@echo "Vendoring Go modules..."
	@go mod vendor

image-debug: RUNTIME=alpine   ##@ Build debug image (alpine, with debug symbols, while-loop entrypoint)
image-debug: image            ##@ Shorthand for: RUNTIME=alpine make image

image-release: RUNTIME=distroless  ##@ Build release image (distroless, stripped binary)
image-release: image               ##@ Shorthand for: RUNTIME=distroless make image

image:  ##@ Build Docker image
        ##@ Builds container image with version tagging
        ##@ Accepts VERSION, COMMIT_ID, IMAGE_TAG, RUNTIME from environment or auto-detects from git
        ##@ RUNTIME selects the runtime stage: alpine (default) or distroless
        ##@ Loads into local docker by default; set PUSH=1 to push to registry instead
image: vendor
	@echo "Building $(SERVICE_NAME) docker image..."
	@if ! command -v git >/dev/null 2>&1; then echo "Git not found. Please install Git."; exit 1; fi
	@VERSION=$${VERSION:-$$(git describe --tags --match 'v*' | sed 's/-g.*//')} && \
	COMMIT_ID=$${COMMIT_ID:-$$(git rev-parse --short HEAD)} && \
	IMAGE_TAG=$${IMAGE_TAG:-$$VERSION} && \
	OUTPUT_FLAG=$$(if [ "$${PUSH:-0}" = "1" ]; then echo "--push"; else echo "--load"; fi) && \
	docker buildx build \
		$$OUTPUT_FLAG \
		--target $(RUNTIME) \
		--build-arg VERSION=$$VERSION \
		--build-arg COMMIT_ID=$$COMMIT_ID \
		-t $(REPO)/$$(grep -v '^#' docker/$(SERVICE_NAME)/BUILD | tail -1):$$IMAGE_TAG \
		-f docker/$(SERVICE_NAME)/Dockerfile \
		.

reload: build ##@ Hot-swap the running provisioner binary in place (dev only)
              ##@ Finds the container's while-loop entrypoint via /proc, replaces /app/provisioner, and kills the child so the loop respawns it
	@./scripts/reload.sh

dlv: ##@ Attach delve to the running provisioner process for remote debugging
     ##@ Headless debug server on 127.0.0.1:2345 (override with DLV_LISTEN) — connect with `dlv connect` or your IDE
	@./scripts/dlv.sh

debug-setup: ##@ Set up local debug environment
             ##@ Generates go.work (Go version taken from go.mod) and symlinks the common module for local debugging
	@GO_VERSION=$$(grep -m1 '^go ' go.mod | awk '{print $$2}') && \
	printf 'go %s\n\nuse (\n\t.\n\t/opt/shared/common\n)\n' "$$GO_VERSION" > go.work
	ln -sfn /opt/shared/common common

coverage:  ##@ Calculate test coverage percentage from coverage.out
	@go tool cover -func=$(REPORTS_DIR)/coverage.out | grep total | awk '{print $$3}'

##@
##@ Misc commands
##@

clean: ##@ Clean up generated files
	rm -rf $(REPORTS_DIR)
	rm -f bin/$(SERVICE_NAME)
	rm -rf vendor/


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
