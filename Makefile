.PHONY: build run test docker-build docker-push clean deps fmt lint golangci-lint

# Variables
BINARY_NAME=external-dns-simply-webhook
DOCKER_IMAGE=ghcr.io/uozalp/external-dns-simply-webhook
VERSION?=latest

# Build the application
build:
	@go build -o ./bin/$(BINARY_NAME) ./cmd/server

# Run the application
run:
	@go run ./cmd/server/main.go

# Run tests
test:
	@go test -v ./...

# Build Docker image
docker-build:
	@docker build -t $(DOCKER_IMAGE):$(VERSION) .

# Push Docker image
docker-push:
	@docker push $(DOCKER_IMAGE):$(VERSION)

# Clean build artifacts
clean:
	@rm -f ./bin/$(BINARY_NAME)
	@go clean

# Install dependencies
deps:
	@go mod download
	@go mod tidy

# Format code
fmt:
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
