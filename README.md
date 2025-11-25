# ExternalDNS Webhook Provider for Simply.com

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

A webhook provider for [ExternalDNS](https://github.com/kubernetes-sigs/external-dns) that enables automatic DNS record management in [Simply.com](https://www.simply.com/).

## Overview

This webhook adapter allows ExternalDNS to manage DNS records in Simply.com via their public REST API. ExternalDNS automatically creates, updates, and deletes DNS records based on Kubernetes resources like Services and Ingresses.

## Features

- Full support for ExternalDNS webhook interface
- Manages A, CNAME, TXT, and other DNS record types
- Handles record creation, updates, and deletions
- Supports TTL management
- Domain filtering support
- Kubernetes-native deployment

## Prerequisites

- Kubernetes cluster (1.19+)
- ExternalDNS (v0.14.0+)
- Simply.com account with API access
- Simply.com API key

## Installation

### 1. Get Simply.com Credentials

1. Log in to your Simply.com account
2. Navigate to API settings
3. Get your account name and API key

### 2. Deploy the Webhook

#### Using kubectl

```bash
# Clone the repository
git clone https://github.com/uozalp/external-dns-simply-webhook.git
cd external-dns-simply-webhook

# Create namespace
kubectl create namespace external-dns

# Create secret with your Simply.com credentials
kubectl create secret generic simply-credentials \
  --from-literal=account-name='YOUR_ACCOUNT_NAME' \
  --from-literal=api-key='YOUR_API_KEY' \
  -n external-dns

# Deploy the webhook
kubectl apply -f deploy/deployment.yaml

# Deploy ExternalDNS
kubectl apply -f deploy/external-dns.yaml
```

**Note:** You can optionally edit `deploy/deployment.yaml` and `deploy/external-dns.yaml` to set specific domain(s) in the `DOMAIN_FILTER` and `--domain-filter` settings. If not set, all domains managed by Simply.com will be used.


### 3. Configure ExternalDNS

The webhook is configured via environment variables in the deployment:

| Variable | Description | Required | Default |
|----------|-------------|----------|---------|
| `SIMPLY_ACCOUNT_NAME` | Simply.com account name | Yes | - |
| `SIMPLY_API_KEY` | Simply.com API key | Yes | - |
| `DOMAIN_FILTER` | Comma-separated list of domains to manage | No | All domains |
| `LOG_LEVEL` | Logging level (debug, info, warn, error) | No | `info` |
| `PORT` | HTTP server port | No | `8888` |

ExternalDNS configuration:

```yaml
args:
  - --source=service              # Watch Services
  - --source=ingress              # Watch Ingresses
  - --domain-filter=example.com   # Only manage this domain
  - --provider=webhook
  - --webhook-provider-url=http://external-dns-simply-webhook.external-dns.svc.cluster.local:8888
  - --policy=upsert-only          # Don't delete records (or use 'sync')
  - --registry=txt                # Use TXT registry for ownership
  - --txt-owner-id=my-cluster-id  # Unique identifier for this cluster
  - --interval=1m                 # Sync interval
```

## API Endpoints

The webhook exposes the following endpoints:

| Method | Path | Description |
|--------|------|-------------|
| GET | `/records` | Returns current DNS records |
| POST | `/records` | Applies DNS record changes |
| POST | `/adjustendpoints` | Normalizes endpoints (optional) |
| GET | `/healthz` | Health check endpoint |

All endpoints use `Content-Type: application/external.dns.webhook+json;version=1`

## Development

### Prerequisites

- Go 1.21+
- Docker (for building images)
- kubectl (for testing)

### Build

```bash
# Install dependencies
make deps

# Build binary
make build

# Run locally
export SIMPLY_ACCOUNT_NAME='your-account-name'
export SIMPLY_API_KEY='your-api-key'
export DOMAIN_FILTER='example.com'
make run
```

### Test

```bash
# Run tests
make test

# Format code
make fmt

# Lint code
make lint
```

### Build Docker Image

```bash
# Build image
make docker-build VERSION=v1.0.0

# Push image
make docker-push VERSION=v1.0.0
```

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

## Acknowledgments

- [ExternalDNS](https://github.com/kubernetes-sigs/external-dns) - Kubernetes DNS automation
- [Simply.com](https://www.simply.com/) - DNS provider

## Support

For issues and questions:
- Open an issue on [GitHub](https://github.com/uozalp/external-dns-simply-webhook/issues)
- Check ExternalDNS documentation: https://kubernetes-sigs.github.io/external-dns/