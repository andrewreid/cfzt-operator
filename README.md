# cfzt-operator

Kubernetes operator for managing Cloudflare Tunnel, public hostname exposure, Cloudflare Access, managed DNS, tunnel private-network routes, and optional active-passive multi-cluster DR failover from custom resources.

## What It Manages

| CRD | Scope | Short name | Purpose |
|---|---|---|---|
| `CloudflareTunnel` | Cluster | `cft` | Creates or adopts a remotely managed Cloudflare Tunnel, stores its token, deploys `cloudflared`, and writes the tunnel ingress config. |
| `CloudflareExposure` | Namespaced | `cfe` | Publishes one hostname through a tunnel, optionally creating a proxied DNS CNAME and one or more Access applications. |
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
| Zone / DNS: Edit | When `dns.manage: true`; also used for the DR failover lease (no extra scope) |

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

### Site identity (`site.id`)

Each operator process runs with a `--site-id`, surfaced as the Helm value `site.id` (default `cfzt-default-site`). The default is safe for single-site installs and lets existing installs upgrade without a values change.

If you use DR failover across clusters, **set a distinct `site.id` per cluster** — the value identifies which cluster holds the lease, so two clusters sharing the default cannot fail over correctly:

```sh
helm install cfzt-operator ... --set site.id=homelab-primary   # cluster A
helm install cfzt-operator ... --set site.id=homelab-dr        # cluster B
```

An empty `site.id` is a fatal start-up error.

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
    applications:
      - name: root
        domains:
          - jellyfin.example.com
        policies:
          - policyRef:
              uuid: 00000000-0000-4000-8000-000000000001
```

That reconciles the Cloudflare Tunnel, token Secret, `cloudflared` DaemonSet, tunnel ingress rule, proxied DNS CNAME, and Access applications.

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
    applications:
      - name: root
        domains:
          - jellyfin.example.com
        policies:
          - policyRef:
              name: family-only
      - name: admin
        domains:
          - jellyfin.example.com/admin
        policies:
          - policyRef:
              name: family-only
```

Root plus specific path overrides are the recommended pattern. A bare host and `host/*` describe the same coverage, so do not list them both.

`access.applications[].domains` is ordered. The first value becomes the Cloudflare Access application's primary domain, and the full list is written to Cloudflare as `self_hosted_domains` in the same order. Model path-specific behaviour as a root application for `host` plus more-specific path applications such as `host/admin`, `host/alerts-*`, or `host/v1/health`. Do not add a redundant `host/*` app beside the root app; the controller treats that as duplicate coverage and reports `Ready=False, Reason=HostnameConflict` without writing Cloudflare resources.

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
| `spec.access.enabled` | Enables Cloudflare Access applications. |
| `spec.access.applications[]` | Defines the Access applications to create. |
| `spec.access.applications[].domains[]` | Ordered Access targets for that app; first entry is the primary Cloudflare app domain. |
| `spec.access.applications[].policies[].policyRef.uuid` or `name` | Exactly one is required for each policy binding. |
| `spec.failover.group` | Opts into DR failover; cross-cluster logical-exposure identity (see [Disaster Recovery](#disaster-recovery-dr-failover)). |
| `spec.failover.leaseSeconds` | Lease TTL; default 60, min 30, max 600. The primary renews at half this interval. |

Status records `status.cloudflare.accessApplications[]`, `status.cloudflare.dnsRecordId`, `status.cloudflare.publicHostnameRouteHash`, `status.conditions`, and — when `spec.failover` is set — `status.failover` (role, lease owner, expiry; see below).

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
| Exposure Access apps | Access tags with `managed-by=cfzt-operator` and chunked `source-uid-<n>=...` values | `ForeignResource` or `HostnameConflict` |
| Exposure DNS CNAME | DNS record comment with `managed-by=cfzt-operator source-uid=<exposure-uid>` | `ForeignResource` or `HostnameConflict` |
| Access policy | `CloudflareAccessPolicy.status.policyId` | `ForeignPolicy` |
| Tunnel route | `CloudflareTunnelRoute.status.routeId` plus comment `managed-by=cfzt source-uid=<route-uid>` | `ForeignRoute` |

Ingress rules inside the tunnel config are not tagged individually; the tunnel controller rewrites the complete config from current Kubernetes state. Repeated reconciles are intended to be idempotent, and dashboard drift on owned resources is corrected.

For a failover Exposure (`spec.failover`) the shared Access app set and DNS CNAME carry the **failover-group ID** as their `source-uid` instead of the per-CR uid, so either cluster's operator recognizes them as owned. A promoted standby re-lists the group-owned Access applications remotely and discovers the live shared set from Cloudflare rather than trusting stale local status. Deletion of a failover Exposure proves **current** lease ownership before tearing down the shared CNAME / Access set: a standby — or a former primary whose peer has taken over — only removes its own lease record and leaves the shared resources for the live owner.

Deletion is finalizer-driven:

| Delete | What happens |
|---|---|
| `CloudflareExposure` | Removes owned Access applications, DNS CNAME, and tunnel ingress rule. |
| `CloudflareTunnelRoute` | Deletes the owned Cloudflare private-network route. |
| `CloudflareAccessPolicy` | Blocks with `BlockedByExposures` until no Exposure references it. |
| `CloudflareTunnel` | Blocks with `BlockedByExposures` or `BlockedByRoutes` until dependants are gone, then removes `cloudflared`, token Secret, and the Cloudflare Tunnel. |

## Disaster Recovery (DR failover)

Active-passive DR lets the **same** `CloudflareExposure`, applied to two (or more) clusters, cooperate over one public hostname. Exactly one cluster is **Primary** and serves traffic; the others stay **Standby** (warm — their tunnel and `cloudflared` keep running). If the primary fails, a standby auto-promotes; when the former primary returns, it stands down. It is opt-in per Exposure via `spec.failover`, and changes nothing for Exposures without it.

### How it works

Each cluster runs its own `CloudflareTunnel` (own tunnel ID, own `cloudflared`) and its own copy of the Exposure with a matching `spec.failover.group`. Coordination uses a single Cloudflare **DNS TXT lease record** at `_cfzt-lease.<hash8(group)>.<zone>`. The lease holder is Primary and alone writes the shared public CNAME and Access application set; the CNAME points at the holder's tunnel. Promotion is automatic when the lease TTL expires. When a standby promotes, it re-discovers the group-owned Access applications remotely and continues from the live Cloudflare state instead of stale status.

The lease is **best-effort, not a distributed lock.** Cloudflare DNS offers no conditional write, so correctness does not rely on the lease being atomic — it rests on the data plane: there is one CNAME (so two sites can never serve different origins at once), and a failed primary's `cloudflared` drops its edge connection within seconds, so Cloudflare stops routing to it regardless of the lease. The lease's job is to elect a single writer and stop the CNAME flapping between healthy sites.

### Prerequisites

- A **distinct `--site-id` per cluster** (`--set site.id=...`; see [Install](#site-identity-siteid)). A failover Exposure on the chart-default site id reports `Ready=False, Reason=FailoverRequiresDistinctSiteID`.
- The referenced `CloudflareTunnel` must have `dns.manage: true` (the default). Otherwise `Ready=False, Reason=FailoverRequiresManagedDNS`.
- The same `spec.failover.group` must **not** be reused by another Exposure in the same cluster (`Reason=FailoverGroupConflict`). Across clusters, sharing the group is exactly how the two sides find each other.

### Example

Apply the same Exposure to both clusters. Only `tunnelRef` differs (each cluster has its own tunnel); `hostname` and `failover.group` match.

```yaml
# Identical in both clusters except tunnelRef.name.
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: jellyfin
  namespace: media
spec:
  hostname: jellyfin.example.com
  tunnelRef:
    name: homelab            # cluster A: "homelab"; cluster B: its own tunnel
  origin:
    protocol: http
    host: jellyfin.media.svc.cluster.local
    port: 8096
  failover:
    group: jellyfin-dr       # same in both clusters
    leaseSeconds: 60         # optional; default 60
```

Exactly one cluster reports `status.failover.role: Primary`; the other reports `Standby` with `leaseOwner` set to the primary's site id.

```sh
kubectl get cfe -A          # the ROLE column shows Primary / Standby
```

The lease record name is `_cfzt-lease.<hash8(group)>.<zone>` (the group is hashed to keep it bounded and out of public DNS). The exact name appears in `LeaseConflict` event messages; once known you can inspect it directly, e.g. `dig +short TXT _cfzt-lease.<hash>.example.com @1.1.1.1`.

### Operating

- **Observe role:** `kubectl get cfe` (the `Role` print column) or `kubectl get cfe jellyfin -n media -o jsonpath='{.status.failover}'`.
- **Force promotion (emergency):** annotate the standby with a *new* token value. The controller acquires the lease regardless of expiry, records the token in `status.failover.lastForcePromoteToken`, and emits `ForcePromoted`. It does **not** remove the annotation, so committing it to Git is safe — the same token never re-fires; change the value to force again.

  ```sh
  kubectl annotate cfe jellyfin -n media cfzt.reid.ee/force-promote="$(date +%s)" --overwrite
  ```

  Force-promote bypasses the expiry guard; the split-brain risk during the bypass is the operator's responsibility.
- **Returning primary:** a recovered former primary that finds the lease held by a peer self-demotes (`DemotedToStandby`) and performs no writes for the shared hostname. It never auto-steals the lease back.

### Conditions, events, metrics

- Reasons: `Standby`, `LeaseConflict`, `FailoverRequiresManagedDNS`, `FailoverRequiresDistinctSiteID`, `FailoverGroupConflict`.
- Events: `PromotedToPrimary`, `DemotedToStandby`, `LeaseAcquired`, `LeaseRenewed`, `LeaseLost`, `LeaseConflict`, `SplitBrainDetected`, `ForcePromoted`.
- Metrics (when enabled): `cfzt_failover_role` (0=Unknown, 1=Standby, 2=Primary), `cfzt_failover_lease_renew_total`, `cfzt_failover_promotion_total` — all labelled with `site_id`.

### Limits

- Active-passive only — exactly one Primary per group. No active-active.
- Single Cloudflare account, single zone per hostname. No cross-zone or cross-account failover.
- A recovered primary stays Standby until it legitimately re-acquires an expired lease (no automatic fail-back).
- Worst-case dual-writer window is bounded by `leaseSeconds` + jitter (default 60s ±5s); lower `leaseSeconds` to shrink it.

## Operations

CRDs live in `charts/cfzt-operator/crds/`. Helm 3 installs CRDs but does not upgrade them on `helm upgrade`. For breaking `v1alpha1` CRD changes, export existing `CloudflareTunnel`, `CloudflareExposure`, `CloudflareAccessPolicy`, and `CloudflareTunnelRoute` resources, uninstall, install the new chart, then reapply.

Metrics are disabled by default. Enable the controller-runtime metrics endpoint with:

```yaml
metrics:
  enabled: true
```

Common Events include `CreatedTunnel`, `CreatedAccessApp`, `CreatedAccessPolicy`, `UpdatedAccessPolicy`, `CreatedRoute`, `DeletedRoute`, `TokenRotated`, `HostnameConflict`, `ForeignTunnel`, `ForeignRoute`, `BlockedByExposures`, and `BlockedByRoutes`. DR failover adds `PromotedToPrimary`, `DemotedToStandby`, `LeaseAcquired`, `LeaseRenewed`, `LeaseLost`, `LeaseConflict`, `SplitBrainDetected`, and `ForcePromoted` (see [Disaster Recovery](#disaster-recovery-dr-failover)).

## Not Supported

- Annotation-driven UX
- Ingress `sourceRef`
- WARP client-side routing, split-tunnel client config, private DNS, or packet-level validation
- Cloudflare Gateway policy management
- Active-active multi-cluster, automatic fail-back, cross-zone/cross-account failover (active-passive DR within one account is supported — see [Disaster Recovery](#disaster-recovery-dr-failover))
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
