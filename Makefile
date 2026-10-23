.PHONY: build run test docker-build docker-push clean deps fmt lint golangci-lint

# Variables
BINARY_NAME=external-dns-simply-webhook
DOCKER_IMAGE=ghcr.io/uozalp/external-dns-simply-webhook
VERSION?=latest

# Build the application
build:
	@echo "Building..."
	@go build -o $(BINARY_NAME) ./cmd/server

# Run the application
run:
	@echo "Running..."
	@go run ./cmd/server/main.go

# Run tests
test:
	@echo "Running tests..."
	@go test -v ./...

# Build Docker image
docker-build:
	@echo "Building Docker image..."
	@docker build -t $(DOCKER_IMAGE):$(VERSION) .

# Push Docker image
docker-push:
	@echo "Pushing Docker image..."
	@docker push $(DOCKER_IMAGE):$(VERSION)

# Clean build artifacts
clean:
	@echo "Cleaning..."
	@rm -f $(BINARY_NAME)
	@go clean

# Install dependencies
deps:
	@echo "Installing dependencies..."
	@go mod download
	@go mod tidy

# Format code
fmt:
	@echo "Formatting code..."
	@go fmt ./...

# Lint code
lint: golangci-lint
	@echo "Linting code..."
	@$(GOLANGCI_LINT) run --timeout=3m

# Download golangci-lint locally, built with the Go toolchain required by go.mod
LOCALBIN ?= $(CURDIR)/bin
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.13.2

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT):
	@echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)..."
	@mkdir -p $(LOCALBIN)
	@GOBIN=$(LOCALBIN) GOTOOLCHAIN=$$(go env GOVERSION) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
