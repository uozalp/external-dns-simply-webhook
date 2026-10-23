# Build stage
FROM golang:1.27-alpine AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o external-dns-simply-webhook ./cmd/server

# Runtime stage: static binary, CA certificates included, no shell
FROM gcr.io/distroless/static-debian13:nonroot

WORKDIR /app

# Copy the binary from builder
COPY --from=builder --chmod=0755 /app/external-dns-simply-webhook /app/external-dns-simply-webhook

# Expose the default port
EXPOSE 8888

# Run the application
CMD ["/app/external-dns-simply-webhook"]
