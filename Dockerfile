# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o external-dns-simply-webhook ./cmd/server

# Runtime stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /app

# Copy the binary from builder
COPY --from=builder --chmod=0755 /app/external-dns-simply-webhook /app/external-dns-simply-webhook

# Use non-root user
USER 65534:65534

# Expose the default port
EXPOSE 8888

# Run the application
CMD ["/app/external-dns-simply-webhook"]
