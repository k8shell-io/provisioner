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

