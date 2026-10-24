# ExternalDNS Webhook Provider for Simply.com

[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A webhook provider for [ExternalDNS](https://github.com/kubernetes-sigs/external-dns) that enables automatic DNS record management in [Simply.com](https://www.simply.com/).

## Overview

This webhook adapter allows ExternalDNS to manage DNS records in Simply.com via their public REST API. ExternalDNS automatically creates, updates, and deletes DNS records based on Kubernetes resources like Services and Ingresses.

## Features

- Full support for ExternalDNS webhook interface
- Manages A, AAAA, CNAME, TXT, MX and SRV records (MX targets are written as `<priority> <mail server>`, SRV targets as `<priority> <weight> <port> <target>.` with a trailing dot). See [Supported record types](#supported-record-types)
- Handles record creation, updates, and deletions
- Supports TTL management (see [TTL behavior](#ttl-behavior))
- Domain filtering support; the webhook refreshes its own domain list from Simply.com every 5 minutes. This does not change the filter ExternalDNS read at startup, so restart ExternalDNS after you add or remove a domain
- Works with IDN domains and with subdomain products (such as `www.example.com` as its own product at Simply.com)
- Retries reads, updates and deletes on Simply.com rate limits (`429`, honouring `Retry-After`), `5xx` answers and network errors; creates are never retried (see [API retries](#api-retries))
- Kubernetes-native deployment, runs as non-root with a read-only root filesystem

## Prerequisites

- Kubernetes cluster. The manifests and chart use only stable APIs and set no minimum version (no `kubeVersion` in the chart); 1.19 is the oldest version they are written for, and CI does not test against a cluster. The practical floor is whatever your ExternalDNS release supports
- ExternalDNS (v0.14.0+, the first release with webhook providers)
- Simply.com account with API access
- Simply.com API key

## Installation

### 1. Get Simply.com Credentials

1. Log in to the Simply.com control panel
2. Go to **Account → API keys** and create a key
3. Keep the key; you store it in a Kubernetes Secret below (see the [Simply.com API docs](https://www.simply.com/en/docs/api/))

The API key alone identifies the account. An account name is not needed: `SIMPLY_ACCOUNT_NAME` and an `account-name` secret key from older versions are ignored and can be removed.

### 2. Deploy the Webhook

> **Warning: start with `--policy=upsert-only`.** With `--policy=sync` ExternalDNS also *deletes* records, and a wrong domain filter or TXT owner ID can remove records you care about. The webhook does not check ownership itself; it deletes whatever ExternalDNS asks for. Switch to `sync` only after you have:
>
> 1. run with `upsert-only` for a few intervals and confirmed that the TXT ownership records (`--registry=txt`) appear next to the records ExternalDNS created,
> 2. set `--domain-filter` in ExternalDNS (and `DOMAIN_FILTER` on the webhook) to exactly the domains you want managed,
> 3. checked that `--txt-owner-id` is unique per cluster, and
> 4. done a `--dry-run` pass (see below) and read the planned changes.
>
> **Audit mode.** The webhook has no dry-run switch of its own. Run ExternalDNS with `--dry-run`: it computes and logs the changes but never calls `POST /records`, so nothing is written to Simply.com.
>
> **Recovery.** The webhook keeps no backup. Before enabling `sync`, save a copy of each zone, for example from the Simply.com control panel or with `GET https://api.simply.com/2/my/products/<domain>/dns/records/`. If records were deleted by mistake: set the policy back to `upsert-only`, fix the filter or owner ID, then let ExternalDNS re-create records that still have a source (it does so on the next `--interval`). Records that came from outside Kubernetes, such as hand-made MX or TXT records, must be restored from your saved copy. Records the webhook creates carry the comment `Managed by External-DNS`, which helps you tell them apart in the control panel.

The webhook runs as a sidecar container in the ExternalDNS pod, either with the manifest in this repo or with the official ExternalDNS chart. ExternalDNS then reaches it on `localhost`, and with no Service nothing outside the pod is routed to it. The bundled Helm chart is the exception: it runs the webhook as a standalone Deployment with its own Service.

#### Using kubectl (sidecar)

```bash
git clone https://github.com/uozalp/external-dns-simply-webhook.git
cd external-dns-simply-webhook

kubectl create namespace external-dns

kubectl create secret generic simply-credentials \
  --from-literal=api-key='YOUR_API_KEY' \
  -n external-dns

# Edit DOMAIN_FILTER and --domain-filter first if you want to limit the domains,
# and --txt-owner-id (must be unique per cluster)
kubectl apply -f deploy/external-dns.yaml
```

The manifest creates the namespace, RBAC, and one Deployment with two containers (`simply-webhook` and `external-dns`). It pins the webhook image to release `0.3.0`; change the tag to upgrade. If you leave `DOMAIN_FILTER` and `--domain-filter` unset, all domains managed by Simply.com are used.

Check that it works:

```bash
kubectl -n external-dns get pods
kubectl -n external-dns logs deploy/external-dns -c simply-webhook   # should list "Managing domains"
```

#### Using Helm (standalone webhook)

This chart deploys only the webhook (a Deployment and a Service). It does **not** install ExternalDNS; you install and configure that separately. Use it only if you want to run ExternalDNS separately.

```bash
kubectl create namespace external-dns

kubectl create secret generic simply-credentials \
  --from-literal=api-key='YOUR_API_KEY' \
  -n external-dns

helm install external-dns-simply \
  oci://ghcr.io/uozalp/helm/external-dns-simply \
  -n external-dns \
  --set domainFilter=example.com
```

| Value | Description | Default |
|-------|-------------|---------|
| `domainFilter` | Comma-separated domains the webhook manages (sets `DOMAIN_FILTER`; it does not configure ExternalDNS) | all domains |
| `simply.existingSecret` | Secret that holds the API key | `simply-credentials` |
| `simply.apiKeyKey` | Key inside that secret | `api-key` |
| `webhook.image.tag` | Image tag | chart `appVersion` |
| `webhook.port` | Webhook port | `8888` |
| `webhook.logLevel` | `debug`, `info`, `warn` or `error` | `info` |

Then install ExternalDNS separately and point it to the webhook with
`--webhook-provider-url=http://external-dns-simply.external-dns.svc.cluster.local:8888`.
Set a compatible `--domain-filter` on ExternalDNS too, as `domainFilter` only affects the webhook. The chart listens on `8888`, so the URL above uses that port. Run a single replica of the webhook: it serializes writes inside one process only.

#### Using the official ExternalDNS Helm chart (sidecar)

```yaml
# values.yaml
provider:
  name: webhook
  webhook:
    image:
      repository: ghcr.io/uozalp/external-dns-simply-webhook
      tag: "0.3.0"   # pin a release tag
    env:
    - name: SIMPLY_API_KEY
      valueFrom:
        secretKeyRef:
          name: simply-credentials
          key: api-key
    - name: PORT
      value: "8080"  # the chart's probes expect port 8080 (the webhook itself defaults to 8888)
    - name: DOMAIN_FILTER
      value: example.com
domainFilters:
- example.com
policy: upsert-only   # use 'sync' only after reading the warning above
registry: txt
txtOwnerId: my-cluster-id   # unique per cluster
sources:
- service
- ingress
extraArgs:
- --webhook-provider-url=http://localhost:8080
# To manage MX or SRV, add --source=crd and list every type, for example:
# - --managed-record-types=A
# - --managed-record-types=AAAA
# - --managed-record-types=CNAME
# - --managed-record-types=MX
```

```bash
helm repo add external-dns https://kubernetes-sigs.github.io/external-dns/
helm install external-dns external-dns/external-dns -n external-dns -f values.yaml
```

Create the `simply-credentials` secret first, as above.

### 3. Configure

The webhook is configured via environment variables in the deployment:

| Variable | Description | Required | Default |
|----------|-------------|----------|---------|
| `SIMPLY_API_KEY` | Simply.com API key | Yes (the webhook exits without it) | - |
| `DOMAIN_FILTER` | Comma-separated list of domains to manage. Unicode or punycode names both work. | No | All domains |
| `LOG_LEVEL` | Logging level (debug, info, warn, error) | No | `info` |
| `PORT` | HTTP server port. The kubectl manifest and the standalone chart use `8888`; the official ExternalDNS chart's webhook probes expect `8080`, so set `PORT=8080` there and match `--webhook-provider-url` | No | `8888` |

At startup the webhook lists your Simply.com domains. `DOMAIN_FILTER` limits what the webhook reads and writes, while ExternalDNS's `--domain-filter` limits what ExternalDNS plans; set both, to compatible values. Names in `DOMAIN_FILTER` that Simply.com does not manage are logged as errors and skipped. If none match, the webhook exits. Logs are JSON on stdout.

ExternalDNS configuration (the `external-dns` container args):

```yaml
args:
  - --source=service              # Watch Services
  - --source=ingress              # Watch Ingresses
  - --source=crd                  # Only needed for DNSEndpoint resources (MX, SRV, ...)
  - --domain-filter=example.com   # Only manage this domain
  - --provider=webhook
  - --webhook-provider-url=http://localhost:8888
  - --policy=upsert-only          # Never delete records; use 'sync' only after the checks above
  - --managed-record-types=A      # ExternalDNS only manages A, AAAA and CNAME by default;
  - --managed-record-types=AAAA   # repeat the flag and add MX and SRV to manage those too
  - --managed-record-types=CNAME
  - --managed-record-types=MX
  - --managed-record-types=SRV
  - --registry=txt                # Use TXT registry for ownership
  - --txt-owner-id=my-cluster-id  # Unique identifier for this cluster
  - --interval=1m                 # Sync interval
```

### MX and SRV records

Services and Ingresses can't produce MX or SRV records, so create them with a `DNSEndpoint` resource. This needs the `DNSEndpoint` CRD installed, `--source=crd`, and `--managed-record-types=MX` and/or `--managed-record-types=SRV` (plus A, AAAA and CNAME if you still want those) in the ExternalDNS args. For MX, write each target as `<priority> <mail server>`:

```yaml
apiVersion: externaldns.k8s.io/v1alpha1
kind: DNSEndpoint
metadata:
  name: example-mx
spec:
  endpoints:
  - dnsName: example.com
    recordType: MX
    recordTTL: 300
    targets:
    - "10 mail.example.com"
    - "20 mail2.example.com"
```

The webhook sends the priority to Simply.com in its own field and returns it the same way on read.

SRV records use the form `<priority> <weight> <port> <target>`. ExternalDNS requires the target host to end with a dot, and skips the endpoint as "invalid configuration" otherwise:

```yaml
apiVersion: externaldns.k8s.io/v1alpha1
kind: DNSEndpoint
metadata:
  name: example-srv
spec:
  endpoints:
  - dnsName: _sip._tcp.example.com
    recordType: SRV
    recordTTL: 300
    targets:
    - "10 60 5060 sip1.example.com."
    - "20 40 5060 sip2.example.com."
```

### Supported record types

The webhook creates, updates, reads and deletes **A, AAAA, CNAME, TXT, MX and SRV** records. MX and SRV targets must start with a priority (see above). Other types, such as NS or CAA, are not supported: the webhook neither validates nor transforms them, so whether Simply.com accepts them is untested. It reads every record type in a managed domain, but ExternalDNS only acts on the types in `--managed-record-types` (A, AAAA and CNAME by default).

### TTL behavior

- A create without a TTL (ExternalDNS sends `0`) gets `3600` seconds.
- An update without a TTL keeps the TTL of the existing record; `3600` applies only if the update has to add a new record.
- Any other TTL is sent to Simply.com unchanged. The webhook does not check minimum or maximum values, and a negative or out-of-range value is rejected by Simply.com, which fails the request with `500`.

### API retries

| Operation | Simply.com call | Retried |
|-----------|-----------------|---------|
| List domains and records | `GET` | Yes |
| Update a record | `PUT` | Yes |
| Delete a record | `DELETE` | Yes; a `404` on a retry counts as success |
| Create a record | `POST` | No, so a lost reply cannot create a duplicate |

A call is tried at most 3 times and is retried on `429`, `5xx` answers and network errors or timeouts (30 seconds per attempt). Other `4xx` answers fail at once. The wait starts at 500 ms and doubles, or follows `Retry-After` if that is longer, and is capped at 10 seconds per wait.

## API Endpoints

The webhook exposes the following endpoints:

| Method | Path | Description |
|--------|------|-------------|
| GET | `/` | Negotiation: returns the domain filter |
| GET | `/records` | Returns current DNS records |
| POST | `/records` | Applies DNS record changes |
| POST | `/adjustendpoints` | Normalizes endpoints (optional) |
| GET | `/healthz` | Health check; answers `OK` as long as the process runs (does not call Simply.com) |

The ExternalDNS endpoints answer with `Content-Type: application/external.dns.webhook+json;version=1`. `POST /records` answers `204 No Content`; errors are plain text with a `4xx` or `5xx` status.

## How it works

- Records are written to Simply.com with the comment `Managed by External-DNS`.
- A failed write returns `500` to ExternalDNS, which retries on its next `--interval`. Record creation (`POST`) is not retried inside the webhook, so a lost reply can never create a record twice. Updates and deletes are idempotent and are retried (see [API retries](#api-retries)).
- If listing records fails for any domain, `GET /records` fails as a whole. A partial list would make ExternalDNS re-create the missing records.
- Changes are applied one request at a time, so two requests cannot both create the same record.
- Records of a domain in Simply.com that is not `managed` (DNS hosted elsewhere) are not touched.

## Troubleshooting

| Symptom | Cause and fix |
|---------|---------------|
| Webhook exits with `SIMPLY_API_KEY environment variable is required` | The secret or its key name is wrong. The key must be `api-key` (or set `simply.apiKeyKey` in the Helm chart). |
| `API request failed with status 401` | The API key is wrong or revoked. Create a new one under **Account → API keys**. |
| Webhook exits with `no valid domains found in DOMAIN_FILTER` | None of the names is a DNS-managed product at Simply.com. Check spelling. |
| `no managed Simply.com domain found for ...` | The record name is outside `DOMAIN_FILTER`. Add the domain, then restart ExternalDNS. |
| ExternalDNS ignores a new domain | ExternalDNS reads the domain filter once at startup. Restart it. |
| MX or SRV records are never created | Add `--source=crd` and the matching `--managed-record-types` flags. See [MX and SRV records](#mx-and-srv-records). |
| `must start with a priority` | An MX or SRV target is missing the leading number. Use `10 mail.example.com`. |
| `429` in the logs | Simply.com rate limit. The webhook backs off and retries reads, updates and deletes (not creates). If it persists, raise ExternalDNS `--interval`. |

## Development

### Prerequisites

- Go 1.27+
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

The webhook talks to your real Simply.com account, even when run locally. `GET /records` only reads, but `POST /records` changes the live zone, so test with a throwaway name and a `DOMAIN_FILTER`. Run `ExternalDNS` locally with `--webhook-provider-url=http://localhost:8888` and `--dry-run` to try the full loop.

### Test

```bash
# Run tests
make test

# Format code
make fmt

# Lint code (downloads golangci-lint into ./bin on first run)
make lint
```

CI (`.github/workflows/test.yml`) runs `go vet ./...` and `go test -race ./...` on every pull request and push to `main`.

### Build Docker Image

```bash
# Build image
make docker-build VERSION=dev

# Push image
make docker-push VERSION=dev
```

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Acknowledgments

- [ExternalDNS](https://github.com/kubernetes-sigs/external-dns) - Kubernetes DNS automation
- [Simply.com](https://www.simply.com/) - DNS provider

## Support

For issues and questions:
- Open an issue on [GitHub](https://github.com/uozalp/external-dns-simply-webhook/issues)
- Check ExternalDNS documentation: https://kubernetes-sigs.github.io/external-dns/