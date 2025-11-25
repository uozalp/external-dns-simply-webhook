.PHONY: build run test docker-build docker-push clean

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
lint:
	@echo "Linting code..."
	@golangci-lint run

# Run in development mode with live reload
dev:
	@echo "Running in development mode..."
	@go run ./cmd/server/main.go
