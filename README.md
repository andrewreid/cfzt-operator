# cfzt-operator

Kubernetes operator for managing Cloudflare Tunnel, public hostname exposure, Cloudflare Access, managed DNS, and tunnel private-network routes from custom resources.

## What It Manages

| CRD | Scope | Short name | Purpose |
|---|---|---|---|
| `CloudflareTunnel` | Cluster | `cft` | Creates or adopts a remotely managed Cloudflare Tunnel, stores its token, deploys `cloudflared`, and writes the tunnel ingress config. |
| `CloudflareExposure` | Namespaced | `cfe` | Publishes one hostname through a tunnel, optionally creating a proxied DNS CNAME and Access application. |
| `CloudflareAccessPolicy` | Cluster | `cfap` | Manages one reusable account-level Access policy that Exposures can reference by name. |
| `CloudflareTunnelRoute` | Cluster | `cftr` | Registers one private-network CIDR route on a tunnel. |

Non-Kubernetes origins work too. If a host is reachable from the `cloudflared` pods, set it as an explicit `origin`.

## Requirements

- Kubernetes 1.27+
- Helm 3
- Cloudflare API token

Minimum Cloudflare token permissions:

| Permission | When |
|---|---|
| Account / Cloudflare Tunnel: Edit | Always; includes tunnel private-network routes |
| Account / Access: Apps and Policies: Edit | When using Access applications or managed Access policies |
| Zone / Zone: Read | When `dns.manage: true` so the operator can resolve hostnames to Cloudflare zone IDs |
| Zone / DNS: Edit | When `dns.manage: true` |

## Install

```sh
helm install cfzt-operator oci://ghcr.io/andrewreid/charts/cfzt-operator \
  --namespace cfzt-system \
  --create-namespace \
  --version <version>
```

From a checkout:

```sh
helm install cfzt-operator charts/cfzt-operator \
  --namespace cfzt-system \
  --create-namespace
```

Create the credentials Secret in the `cloudflared` namespace, which defaults to `cfzt-system`:

```sh
kubectl -n cfzt-system create secret generic cloudflare-credentials \
  --from-literal=accountId='<cloudflare-account-id>' \
  --from-literal=apiToken='<cloudflare-api-token>'
```

## Quick Start

Create a tunnel and publish one hostname:

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
  hostname: jellyfin.example.com
  tunnelRef:
    name: homelab
  origin:
    protocol: http
    host: jellyfin.media.svc.cluster.local
    port: 8096
  access:
    enabled: true
    policyRef:
      uuid: 00000000-0000-4000-8000-000000000001
```

That reconciles the Cloudflare Tunnel, token Secret, `cloudflared` DaemonSet, tunnel ingress rule, proxied DNS CNAME, and Access application.

Use a managed Access policy when you want a reusable policy CR instead of a raw Cloudflare policy UUID:

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareAccessPolicy
metadata:
  name: family-only
spec:
  credentialsSecretRef:
    namespace: cfzt-system
    name: cloudflare-credentials
  policyName: Family Only
  decision: allow
  rules:
    include:
      - emailDomain: example.com
  sessionDuration: 24h
---
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: jellyfin
  namespace: media
spec:
  hostname: jellyfin.example.com
  tunnelRef:
    name: homelab
  origin:
    protocol: http
    host: jellyfin.media.svc.cluster.local
    port: 8096
  access:
    enabled: true
    policyRef:
      name: family-only
```

Register a private-network route on the same tunnel:

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareTunnelRoute
metadata:
  name: homelab-lan
spec:
  tunnelRef:
    name: homelab
  network: 192.168.20.0/24
  comment: homelab LAN
```

## CRD Notes

### CloudflareTunnel

Required fields are `spec.tunnelName` and `spec.credentialsSecretRef.name`. Secret keys default to `accountId` and `apiToken`; override them with `spec.credentialsSecretRef.keys`.

Useful optional fields:

| Field | Purpose |
|---|---|
| `spec.dns.manage` | Defaults to `true`; set `false` to skip DNS records for all Exposures on this tunnel. |
| `spec.cloudflared.namespace` | Namespace for token Secret and `cloudflared` DaemonSet; defaults to `cfzt-system`. |
| `spec.cloudflared.image` | Override the pinned default `cloudflared` image; `:latest` is rejected. |
| `spec.cloudflared.resources`, `nodeSelector`, `tolerations`, `affinity` | Pod placement and resource controls. |

Status records `status.tunnelId`, `status.tokenSecretRef.name`, `status.dnsMode`, `status.routes[]`, `status.ingressDocHash`, and `status.conditions`.

If a tunnel with the same Cloudflare name already exists and `status.tunnelId` is empty, the operator refuses with `Reason=ForeignTunnel`. To intentionally adopt a tunnel, patch the status ID out of band:

```sh
kubectl patch cloudflaretunnel homelab \
  --subresource=status --type=merge \
  --patch '{"status":{"tunnelId":"<tunnel-uuid>"}}'
```

### CloudflareExposure

`CloudflareExposure` is namespaced and publishes one hostname. `spec.origin` can point at an in-cluster Service DNS name, LAN hostname, or public host reachable from `cloudflared`.

Field notes:

| Field | Purpose |
|---|---|
| `spec.hostname` | Public hostname; required unless derived from `sourceRef.kind: HTTPRoute`. |
| `spec.tunnelRef.name` | Referenced `CloudflareTunnel`. Immutable. |
| `spec.origin.protocol`, `host`, `port` | Origin target; host and port can be derived only from `sourceRef.kind: Service`. |
| `spec.access.enabled` | Enables a Cloudflare Access application. |
| `spec.access.policyRef.uuid` or `name` | Exactly one is required when Access is enabled. |

Status records `status.cloudflare.accessApplicationId`, `status.cloudflare.dnsRecordId`, `status.cloudflare.publicHostnameRouteHash`, and `status.conditions`.

#### sourceRef

`sourceRef` can derive fields from same-namespace Kubernetes resources and adds an owner reference so deleting the source garbage-collects the Exposure.

For a `Service`, `hostname` is still required, but `origin.host` and `origin.port` can be derived when the Service has exactly one port:

```yaml
spec:
  hostname: jellyfin.example.com
  tunnelRef:
    name: homelab
  sourceRef:
    apiVersion: v1
    kind: Service
    name: jellyfin
  origin:
    protocol: http
```

For an `HTTPRoute`, `hostname` can be derived from the route's single `spec.hostnames` entry, but `origin` remains explicit:

```yaml
spec:
  tunnelRef:
    name: homelab
  sourceRef:
    apiVersion: gateway.networking.k8s.io/v1
    kind: HTTPRoute
    name: jellyfin
  origin:
    protocol: http
    host: jellyfin.media.svc.cluster.local
    port: 8096
```

HTTPRoute support is enabled only when the Gateway API CRD is present at operator startup.

### CloudflareAccessPolicy

`CloudflareAccessPolicy` is cluster-scoped. `spec.policyName` is an optional immutable base name; when omitted the base defaults to `metadata.name`. The Cloudflare-side policy name is always `<base>-cfzt`. `spec.decision` supports `allow`, `deny`, `bypass`, and `non_identity`.

Supported rule item types are `email`, `emailDomain`, `ip`, `everyone: true`, `serviceToken`, and `geoCountryCode`; each item must set exactly one type. Rules are grouped under `include`, `exclude`, and `require`; at least one rule item is required across those groups.

Status records `status.policyId`, `status.observedRulesHash`, `status.referencedBy[]`, `status.referencedByCount`, and `status.conditions`.

Name-colliding Cloudflare policies are not auto-adopted. The operator reports `Reason=ForeignPolicy` and leaves them untouched.

### CloudflareTunnelRoute

`CloudflareTunnelRoute` is cluster-scoped. `spec.network` is a single IPv4 or IPv6 CIDR. The controller canonicalizes it with `net/netip`; invalid CIDRs report `Reason=NetworkInvalid`.

When `spec.virtualNetworkId` is empty, the operator omits Cloudflare's `virtual_network_id` field and lets Cloudflare use the account default VNet. Clearing a previously set `virtualNetworkId` is rejected; delete and recreate the route to return to the default VNet. `spec.comment` is optional user text appended after the compact ownership tag.

Status records `status.routeId`, `status.virtualNetworkId`, and `status.conditions`.

Pre-existing Cloudflare routes with the same CIDR/VNet and no matching ownership comment are not auto-adopted. The operator reports `Reason=ForeignRoute`.

## Ownership, Deletion, and Drift

The operator refuses to mutate Cloudflare resources it cannot prove it owns:

| Resource | Ownership record | Foreign condition |
|---|---|---|
| Tunnel | `CloudflareTunnel.status.tunnelId` | `ForeignTunnel` |
| Exposure Access app | Access tags with `managed-by=cfzt-operator` and chunked `source-uid-<n>=...` values | `ForeignResource` or `HostnameConflict` |
| Exposure DNS CNAME | DNS record comment with `managed-by=cfzt-operator source-uid=<exposure-uid>` | `ForeignResource` or `HostnameConflict` |
| Access policy | `CloudflareAccessPolicy.status.policyId` | `ForeignPolicy` |
| Tunnel route | `CloudflareTunnelRoute.status.routeId` plus comment `managed-by=cfzt source-uid=<route-uid>` | `ForeignRoute` |

Ingress rules inside the tunnel config are not tagged individually; the tunnel controller rewrites the complete config from current Kubernetes state. Repeated reconciles are intended to be idempotent, and dashboard drift on owned resources is corrected.

Deletion is finalizer-driven:

| Delete | What happens |
|---|---|
| `CloudflareExposure` | Removes owned Access app, DNS CNAME, and tunnel ingress rule. |
| `CloudflareTunnelRoute` | Deletes the owned Cloudflare private-network route. |
| `CloudflareAccessPolicy` | Blocks with `BlockedByExposures` until no Exposure references it. |
| `CloudflareTunnel` | Blocks with `BlockedByExposures` or `BlockedByRoutes` until dependants are gone, then removes `cloudflared`, token Secret, and the Cloudflare Tunnel. |

## Operations

CRDs live in `charts/cfzt-operator/crds/`. Helm 3 installs CRDs but does not upgrade them on `helm upgrade`. For breaking `v1alpha1` CRD changes, export existing `CloudflareTunnel`, `CloudflareExposure`, `CloudflareAccessPolicy`, and `CloudflareTunnelRoute` resources, uninstall, install the new chart, then reapply.

Metrics are disabled by default. Enable the controller-runtime metrics endpoint with:

```yaml
metrics:
  enabled: true
```

Common Events include `CreatedTunnel`, `CreatedAccessApp`, `CreatedAccessPolicy`, `UpdatedAccessPolicy`, `CreatedRoute`, `DeletedRoute`, `TokenRotated`, `HostnameConflict`, `ForeignTunnel`, `ForeignRoute`, `BlockedByExposures`, and `BlockedByRoutes`.

## Not Supported

- Annotation-driven UX
- Ingress `sourceRef`
- WARP client-side routing, split-tunnel client config, private DNS, or packet-level validation
- Cloudflare Gateway policy management
- Multi-cluster or multi-account coordination
- Conversion webhooks

## Development

```sh
make manifests generate
make helm-sync-crds
make lint
make test
go test ./...
go test -tags=live ./test/live -run '^TestCloudflarePreflight$' -count=1
helm lint charts/cfzt-operator
helm template cfzt-operator charts/cfzt-operator --namespace cfzt-system
```

Regenerate manifests, deepcopy, and Helm CRDs after any `api/v1alpha1` change, and commit generated output with the API change. CI fails on generated drift.

Live Cloudflare smoke tests are documented in [docs/testing.md](docs/testing.md). Reconciliation design details are in [docs/architecture.md](docs/architecture.md).

## License

Apache-2.0.
