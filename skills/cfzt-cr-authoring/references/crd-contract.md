# CFZT CRD Contract

Use this as the stable authoring contract for `cfzt.reid.ee/v1alpha1` manifests.

## Resources

| Kind | Scope | Short name | Purpose |
|---|---|---|---|
| `CloudflareTunnel` | Cluster | `cft` | Creates or adopts one remotely managed Cloudflare Tunnel, stores its token, and runs `cloudflared`. |
| `CloudflareExposure` | Namespaced | `cfe` | Publishes one hostname through a tunnel, with optional proxied DNS and Cloudflare Access. |
| `CloudflareAccessPolicy` | Cluster | `cfap` | Manages one reusable account-level Cloudflare Access policy. |
| `CloudflareTunnelRoute` | Cluster | `cftr` | Registers one private-network CIDR route on a tunnel. |

Only `CloudflareExposure` gets `metadata.namespace`. Cluster-scoped CFZT CRs must not include a namespace.

## Ownership And Conflicts

The operator refuses to mutate Cloudflare resources it cannot prove it owns.

| Resource | Ownership record | Foreign or conflict reason |
|---|---|---|
| Tunnel | `CloudflareTunnel.status.tunnelId` | `ForeignTunnel` |
| Exposure Access app | Access tags with `managed-by=cfzt-operator` and source UID chunks | `ForeignResource` or `HostnameConflict` |
| Exposure DNS CNAME | DNS record comment with `managed-by=cfzt-operator source-uid=<exposure-uid>` | `ForeignResource` or `HostnameConflict` |
| Access policy | `CloudflareAccessPolicy.status.policyId` | `ForeignPolicy` |
| Tunnel route | `CloudflareTunnelRoute.status.routeId` plus route comment `managed-by=cfzt source-uid=<route-uid>` | `ForeignRoute` |

Ingress rules inside the tunnel config are not individually tagged. The tunnel controller rewrites the full tunnel config from current Kubernetes state.

For an Exposure with `spec.failover`, the shared Access app and DNS CNAME carry the **failover-group ID** as their `source-uid` instead of the per-CR uid, so either cluster's operator recognizes them as owned. Coordination uses a best-effort Cloudflare DNS TXT lease at `_cfzt-lease.<hash8(group)>.<zone>` (no extra token scope; reuses `Zone:DNS:Edit`).

## Conditions

Every CFZT CR uses only `Ready` and `Progressing` conditions. Detailed state appears in `Reason` and `Message`.

Common waiting or conflict reasons include:

- `CredentialsMissing`
- `TunnelNotReady`
- `PolicyNotReady`
- `HostnameConflict`
- `ForeignResource`
- `ForeignTunnel`
- `ForeignPolicy`
- `ForeignRoute`
- `BlockedByExposures`
- `BlockedByRoutes`
- `NetworkInvalid`

Failover Exposures (`spec.failover`) add:

- `Standby` — healthy but not the lease holder (warm standby).
- `LeaseConflict` — lease ambiguous (duplicate/foreign/unparseable); failing closed.
- `FailoverRequiresManagedDNS` — referenced tunnel has `dns.manage: false`.
- `FailoverRequiresDistinctSiteID` — operator is running the chart-default `--site-id`.
- `FailoverGroupConflict` — another Exposure in this cluster shares the group.

## Deletion

Deletion is finalizer-driven with finalizer `cfzt.reid.ee/finalizer`.

| Delete | Behavior |
|---|---|
| `CloudflareExposure` | Removes owned Access app, DNS CNAME, and tunnel ingress rule. |
| `CloudflareTunnelRoute` | Deletes the owned Cloudflare private-network route. |
| `CloudflareAccessPolicy` | Blocks with `BlockedByExposures` until no Exposure references it. |
| `CloudflareTunnel` | Blocks with `BlockedByExposures` or `BlockedByRoutes` until dependants are gone, then removes `cloudflared`, token Secret, and Cloudflare Tunnel. |

A failover Exposure's deletion proves *live* lease ownership first: it tears down the shared CNAME/Access only when this cluster currently holds the lease, otherwise it removes just its own lease record and leaves the shared resources for the live owner. Deleting a standby cluster's Exposure does not disturb the serving primary.

For cleanup plans, delete Exposures and TunnelRoutes before deleting their Tunnel. Delete Exposures before deleting an AccessPolicy they reference.

## Upgrade Caveat

The API is `v1alpha1` and has no conversion webhook. Breaking changes are handled by export, uninstall, install the new chart, and reapply. Helm CRDs live under `charts/cfzt-operator/crds/`; Helm installs CRDs but does not upgrade them on normal `helm upgrade`.
