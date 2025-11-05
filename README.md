# ExternalDNS Webhook Provider for Simply.com

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

A webhook provider for [ExternalDNS](https://github.com/kubernetes-sigs/external-dns) that enables automatic DNS record management in Simply.com.

## Overview

This webhook adapter allows ExternalDNS to manage DNS records in Simply.com via their public REST API. ExternalDNS automatically creates, updates, and deletes DNS records based on Kubernetes resources like Services and Ingresses.

## Features

- ✅ Full support for ExternalDNS webhook interface
- ✅ Manages A, CNAME, TXT, and other DNS record types
- ✅ Handles record creation, updates, and deletions
- ✅ Supports TTL management
- ✅ Domain filtering support
- ✅ Kubernetes-native deployment
- ✅ Health check endpoint

## Prerequisites

- Kubernetes cluster (1.19+)
- ExternalDNS (v0.14.0+)
- Simply.com account with API access
- Simply.com API key

## Installation

### 1. Get Simply.com API Key

1. Log in to your Simply.com account
2. Navigate to API settings
3. Generate a new API key
4. Save the key securely

### 2. Deploy the Webhook

#### Using kubectl

```bash
# Clone the repository
git clone https://github.com/uozalp/external-dns-simply-webhook.git
cd external-dns-simply-webhook

# Create namespace
kubectl create namespace external-dns

# Create secret with your Simply.com API key
kubectl create secret generic simply-api-key \
  --from-literal=api-key='YOUR_SIMPLY_API_KEY' \
  -n external-dns

# Deploy the webhook
kubectl apply -f deploy/deployment.yaml
kubectl apply -f deploy/service.yaml

# Deploy ExternalDNS
kubectl apply -f deploy/external-dns.yaml
```

**Important:** Edit `deploy/deployment.yaml` and `deploy/external-dns.yaml` to set your domain(s) in the `DOMAIN_FILTER` and `--domain-filter` settings.

#### Using Helm (coming soon)

```bash
helm repo add external-dns-simply https://uozalp.github.io/external-dns-simply-webhook
helm install external-dns-simply external-dns-simply/external-dns-simply-webhook \
  --set simply.apiKey='YOUR_SIMPLY_API_KEY' \
  --set domainFilter='example.com'
```

### 3. Configure ExternalDNS

The webhook is configured via environment variables in the deployment:

| Variable | Description | Required | Default |
|----------|-------------|----------|---------|
| `SIMPLY_API_KEY` | Simply.com API key | Yes | - |
| `DOMAIN_FILTER` | Comma-separated list of domains to manage | No | All domains |
| `LOG_LEVEL` | Logging level (debug, info, warn, error) | No | `info` |
| `PORT` | HTTP server port | No | `8080` |

ExternalDNS configuration:

```yaml
args:
  - --source=service              # Watch Services
  - --source=ingress              # Watch Ingresses
  - --domain-filter=example.com   # Only manage this domain
  - --provider=webhook
  - --webhook-provider-url=http://external-dns-simply-webhook.external-dns.svc.cluster.local:8080
  - --policy=upsert-only          # Don't delete records (or use 'sync')
  - --registry=txt                # Use TXT registry for ownership
  - --txt-owner-id=my-cluster-id  # Unique identifier for this cluster
  - --interval=1m                 # Sync interval
```

## Usage

### Annotate Services

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-service
  annotations:
    external-dns.alpha.kubernetes.io/hostname: myapp.example.com
    external-dns.alpha.kubernetes.io/ttl: "300"
spec:
  type: LoadBalancer
  ports:
  - port: 80
    targetPort: 8080
  selector:
    app: myapp
```

### Annotate Ingresses

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: my-ingress
  annotations:
    external-dns.alpha.kubernetes.io/hostname: myapp.example.com
    external-dns.alpha.kubernetes.io/ttl: "300"
spec:
  rules:
  - host: myapp.example.com
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: my-service
            port:
              number: 80
```

ExternalDNS will automatically create/update DNS records in Simply.com.

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

## Architecture

```
┌─────────────────┐
│   ExternalDNS   │
│  (K8s cluster)  │
└────────┬────────┘
         │ HTTP
         ▼
┌─────────────────┐
│  Simply Webhook │
│   (This app)    │
└────────┬────────┘
         │ HTTPS
         ▼
┌─────────────────┐
│  Simply.com API │
│   (DNS records) │
└─────────────────┘
```

### Request Flow

1. **ExternalDNS** watches Kubernetes resources (Services, Ingresses)
2. When changes are detected, it calls the **webhook** with desired DNS state
3. **Webhook** compares current vs desired state
4. **Webhook** calls Simply.com API to create/update/delete records
5. DNS records are synchronized in Simply.com

### Record Reconciliation

The webhook implements smart reconciliation:

- **Creates**: New DNS names not in Simply.com
- **Updates**: Existing records with different targets or TTL
- **Deletes**: Records in Simply.com but not in desired state
- **Type Changes**: A → CNAME is handled as delete + create

## Troubleshooting

### Check webhook logs

```bash
kubectl logs -n external-dns -l app=external-dns-simply-webhook
```

### Check ExternalDNS logs

```bash
kubectl logs -n external-dns -l app=external-dns
```

### Common Issues

**"Failed to list domains"**
- Verify your API key is correct
- Check network connectivity to Simply.com API

**"Could not determine domain for X"**
- Ensure the domain exists in your Simply.com account
- Check `DOMAIN_FILTER` includes the domain

**Records not created**
- Verify ExternalDNS can reach the webhook
- Check ExternalDNS policy is not `upsert-only` if you need deletions
- Ensure annotations are correct on Services/Ingresses

### Debug Mode

Enable debug logging:

```yaml
env:
- name: LOG_LEVEL
  value: "debug"
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

## Related Projects

- [ExternalDNS](https://github.com/kubernetes-sigs/external-dns) - Main project
- [ExternalDNS Webhook Guide](https://github.com/kubernetes-sigs/external-dns/blob/master/docs/tutorials/webhook-provider.md) - Webhook provider documentation