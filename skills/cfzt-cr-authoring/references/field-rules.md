# CFZT Field Rules

Use these rules when authoring or reviewing manifests.

## CloudflareTunnel

Required:

- `spec.tunnelName`: Cloudflare-side tunnel name. Immutable.
- `spec.credentialsSecretRef.name`: Secret name holding Cloudflare credentials.

Defaults and options:

- `spec.credentialsSecretRef.keys.accountId` defaults to `accountId`.
- `spec.credentialsSecretRef.keys.apiToken` defaults to `apiToken`.
- `spec.dns.manage` defaults to `true`. Set `false` to create no DNS records for Exposures on this tunnel.
- `spec.cloudflared.namespace` defaults to `cfzt-system`. This is also where the credentials Secret is read and the token Secret and DaemonSet are managed.
- `spec.cloudflared.image` may override the pinned default but must not end in `:latest`.
- `spec.cloudflared.hostNetwork: true` is useful when `cloudflared` must reach LAN origins via node networking.
- `resources`, `nodeSelector`, `tolerations`, and `affinity` are supported for the `cloudflared` DaemonSet.

Do not include a namespace on `CloudflareTunnel`.

## CloudflareExposure

Required:

- `metadata.namespace`: Exposures are namespaced.
- `spec.tunnelRef.name`: referenced `CloudflareTunnel`. Immutable.
- `spec.hostname`: required unless derived from `sourceRef.kind: HTTPRoute`.
- `spec.origin.protocol`, `host`, and `port`: required unless `sourceRef.kind: Service` can derive host and port.

Access:

- `spec.access.enabled` defaults to `false`.
- If `access.enabled: true`, set exactly one of:
  - `spec.access.policyRef.uuid`: existing Cloudflare Access policy UUID.
  - `spec.access.policyRef.name`: managed `CloudflareAccessPolicy.metadata.name`.
- Do not set both `uuid` and `name`. Do not omit both when Access is enabled.

Source refs:

- `sourceRef` is immutable.
- `sourceRef.kind: Service` requires `apiVersion: v1`; it can derive `origin.host` and `origin.port` only for a same-namespace Service with exactly one port. Keep `spec.hostname`; set at least `origin.protocol`.
- `sourceRef.kind: HTTPRoute` requires `apiVersion: gateway.networking.k8s.io/v1`; it can derive hostname from a single route hostname. Keep explicit `origin.protocol`, `origin.host`, and `origin.port`.
- Same-namespace sourceRefs get ownerReferences, so deleting the source can garbage-collect the Exposure and trigger Cloudflare cleanup.

Immutable fields:

- `spec.tunnelRef.name`
- `spec.sourceRef`
- `spec.hostname`, except when it was initially omitted and derived from an HTTPRoute sourceRef

Failover (optional, D26):

- `spec.failover` opts the Exposure into active-passive multi-cluster DR. Omit it for normal single-cluster Exposures.
- `spec.failover.group`: required when the block is present. RFC 1123 label, 3–63 chars. The cross-cluster identity of one logical exposure; apply the same group in every participating cluster. Do not derive it from the hostname.
- `spec.failover.leaseSeconds`: optional, default 60, min 30, max 600. Lease TTL; the primary renews at half this interval. Lower it to shrink the worst-case dual-writer window.
- Status: `status.failover` reports `role` (`Primary`/`Standby`/`Unknown`), `siteId`, `leaseOwner`, `leaseExpiresAt`, and `lastForcePromoteToken`. The `Role` print column shows the role.
- Prerequisites (not CR fields): the referenced `CloudflareTunnel` must have `dns.manage: true`; each cluster's operator must run a distinct `--site-id` (Helm `site.id`); the group must be unique within a cluster. Violations surface `FailoverRequiresManagedDNS`, `FailoverRequiresDistinctSiteID`, or `FailoverGroupConflict`.
- Emergency manual promotion: set annotation `cfzt.reid.ee/force-promote` to a fresh, unique token (e.g. a timestamp). It is a one-shot token compared against `status.failover.lastForcePromoteToken`; the operator records the honored token and never edits the annotation, so committing it to Git is safe and re-applying the same value does not re-promote. Change the value to force again.

## CloudflareAccessPolicy

Required:

- `spec.credentialsSecretRef.name`
- `spec.credentialsSecretRef.namespace`
- `spec.decision`: one of `allow`, `deny`, `bypass`, `non_identity`.
- `spec.rules`: at least one item across `include`, `exclude`, or `require`.

Defaults and options:

- `spec.credentialsSecretRef.keys.accountId` defaults to `accountId`.
- `spec.credentialsSecretRef.keys.apiToken` defaults to `apiToken`.
- `spec.policyName` is optional and immutable. If omitted, the base name defaults to `metadata.name`.
- Cloudflare-side policy name is always `<base>-cfzt`.
- `spec.sessionDuration` uses values like `30m`, `24h`, `7d`, or `1mo`.
- `spec.purposeJustification.required` defaults to `false`; `prompt` is optional.

Rule groups:

- `include`: any-of match.
- `exclude`: none-of match.
- `require`: all-of match.

Each rule item must set exactly one discriminator:

- `email`
- `emailDomain`
- `ip`
- `everyone: true`
- `serviceToken`
- `geoCountryCode`

Do not include a namespace on `CloudflareAccessPolicy`; only its credentials reference carries a namespace.

## CloudflareTunnelRoute

Required:

- `spec.tunnelRef.name`: referenced `CloudflareTunnel`. Immutable.
- `spec.network`: exactly one IPv4 or IPv6 CIDR.

Defaults and options:

- Omit `spec.virtualNetworkId` to let Cloudflare use the account default VNet. The operator omits Cloudflare's `virtual_network_id` in this case.
- Set `spec.virtualNetworkId` only when intentionally targeting a specific Cloudflare VNet UUID.
- Once a non-empty VNet is set, clearing it is invalid. Delete and recreate the route to return to the account default VNet.
- `spec.comment` is optional user text appended after the ownership tag and is limited to 34 characters.

Do not include a namespace on `CloudflareTunnelRoute`.
