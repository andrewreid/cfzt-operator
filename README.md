# cfzt-operator

Kubernetes operator for publishing selected workloads through Cloudflare Tunnel,
DNS, and Access. The MVP exposes two CRDs:

- `CloudflareTunnel` creates or tracks a remotely-managed Cloudflare tunnel,
  stores its token in Kubernetes, and runs a `cloudflared` DaemonSet.
- `CloudflareExposure` publishes one hostname through that tunnel, optionally
  creating the proxied DNS CNAME and Cloudflare Access application.

The CRD contract and implementation decisions live in [spec.md](spec.md).

## Status

MVP slices are implemented for first GitHub CI validation. Live Cloudflare smoke
testing still requires a Kubernetes cluster, Cloudflare account credentials, and
a hostname in a zone the token can manage.

## Install

The Helm chart is hand-written under `charts/cfzt-operator/` and includes CRDs
in `charts/cfzt-operator/crds/`.

```sh
helm install cfzt-operator charts/cfzt-operator \
  --namespace cfzt-system \
  --create-namespace
```

Create the credentials Secret in the namespace used by
`CloudflareTunnel.spec.cloudflared.namespace`:

```sh
kubectl -n cfzt-system create secret generic cloudflare-credentials \
  --from-literal=accountId='<cloudflare-account-id>' \
  --from-literal=apiToken='<cloudflare-api-token>'
```

Minimum token scopes:

- Account / Cloudflare Tunnel: Edit
- Account / Access: Apps and Policies: Edit
- Zone / DNS: Edit, only when `dns.manage: true`

## Example

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareTunnel
metadata:
  name: homelab
spec:
  tunnelName: homelab-rke2
  credentialsSecretRef:
    name: cloudflare-credentials
  cloudflared:
    namespace: cfzt-system
---
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: jellyfin
  namespace: media
spec:
  tunnelRef:
    name: homelab
  hostname: jellyfin.example.com
  origin:
    protocol: http
    host: jellyfin.media.svc.cluster.local
    port: 8096
  access:
    enabled: true
    policyRef:
      uuid: 00000000-0000-4000-8000-000000000001
```

## Development

Use `rtk` in this repository:

```sh
rtk make lint
rtk go test ./...
rtk make test
rtk helm lint charts/cfzt-operator
rtk helm template cfzt-operator charts/cfzt-operator --namespace cfzt-system
```

`rtk make test` regenerates manifests/deepcopy, runs `go fmt`, `go vet`, and
envtest-backed unit tests. Generated drift must be committed with API changes.

## CRD Upgrades

CRDs are installed from `charts/cfzt-operator/crds/`. Helm 3 does not upgrade
installed CRDs during `helm upgrade`. While the API is `v1alpha1`, breaking CRD
changes are allowed without conversion webhooks; export manifests, uninstall,
install the new chart, then reapply resources.

## License

Apache-2.0. See [LICENSE](LICENSE) when present, or the source file headers.
