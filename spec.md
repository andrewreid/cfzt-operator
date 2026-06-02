# cfzt-operator architecture specification

## Purpose

`cfzt-operator` is a Kubernetes operator for publishing workloads through Cloudflare Zero Trust, Cloudflare Tunnel, and Cloudflare Access.

The primary goal is to let a cluster user expose a workload by creating a single small custom resource alongside their workload, in the broad spirit of `external-dns` but using a CR rather than annotations as the user-facing surface. Non-Kubernetes workloads (devices on the cluster's reachable network — e.g. Home Assistant on the LAN) are also exposable via the same CR by giving an explicit external origin; this lets users keep their entire tunnel configuration in Kubernetes / GitOps.

The operator manages the Cloudflare-side lifecycle needed to make exposure work:

- Cloudflare Tunnel lifecycle (create, delete; no auto-adoption — see D9).
- `cloudflared` connector workload lifecycle inside Kubernetes.
- Tunnel public hostname / published application route configuration via the remotely-managed tunnel-config endpoint.
- Public DNS CNAME records pointing the hostname at the tunnel.
- Cloudflare Access application creation and update.
- Cloudflare Access policy binding (existing policies by UUID in MVP Slices 1–2; managed `CloudflareAccessPolicy` CRs in Slice 4).
- Status reporting back into Kubernetes.
- Safe cleanup of Cloudflare resources owned by the operator.

The MVP is deliberately small. It does not attempt to manage every Cloudflare Zero Trust feature.

## Important design warning

The hard part of this operator is not making Cloudflare API calls.

The hard part is safe reconciliation:

- deciding what the operator owns
- preventing accidental deletion
- handling partial failure
- handling manual drift in the Cloudflare dashboard
- reporting useful status
- making repeated reconciles idempotent
- keeping the user surface small without losing explicit state

Prioritise correctness and clarity over broad feature coverage. Every later decision in this document should be read through this lens.

## Decisions

These are resolved. Treat them as binding constraints, not options.

| # | Decision |
|---|----------|
| D1 | Tunnel configuration is **remotely-managed only**. Operator writes ingress rules via the `cfd_tunnel/{id}/configurations` endpoint. Cloudflared pods are never given a `config.yml`. |
| D2 | Operator **manages public DNS CNAMEs** by default. `CloudflareTunnel.spec.dns.manage: false` opts out completely — when off the operator creates **no** external DNS records and emits **no** external-dns annotations. |
| D3 | **Superseded by D24.** Previously: `CloudflareAccessPolicy` CRD out of MVP; `CloudflareExposure.spec.access.policyRef.uuid` references existing Cloudflare Access policy by UUID. UUID-only binding remains supported alongside managed policies post-Slice 4. |
| D4 | Cloudflared uses a **per-tunnel token**, retrieved by the operator and stored in an operator-managed Kubernetes Secret. No `cert.pem`. |
| D5 | The user interface is **CR-only**. `CloudflareExposure` is the workload-facing CR. There is no annotation controller in MVP. An annotation→Exposure convenience layer may be added post-MVP without changing the core. |
| D6 | `CloudflareTunnel` is **cluster-scoped**. The cloudflared workload it manages is namespaced (default namespace `cfzt-system`, override via `spec.cloudflared.namespace`). |
| D7 | Cloudflared is deployed as a **DaemonSet only** in MVP. Image is pinned in operator code to a specific version, overridable via `spec.cloudflared.image`. |
| D8 | Conditions on every CRD are **`Ready` and `Progressing` only**. Detail goes in `Reason`/`Message`. No granular `*Ready` condition set. |
| D9 | **Superseded by D26.** Previously: **Multi-cluster is out of scope.** No cluster-name flag. Accidental-collision safety is provided per-resource as follows: **Access applications and DNS records** are tagged with the source CR's UID (`managed-by=cfzt-operator source-uid=<uid>`) — operator refuses to mutate any tagged resource whose UID does not match a current local CR. **Cloudflare Tunnels** are named `<spec.tunnelName>-cfzt-<hash8(metadata.uid)>` and tracked by tunnel ID in `CloudflareTunnel.status.tunnelId`. If `status.tunnelId` is missing, the operator may recover a tunnel with the exact generated name; legacy unsuffixed name matches still produce `Reason=ForeignTunnel` and require manual status repair or cleanup. The SDK (`cloudflare-go/v4` v4.6.0) does not surface a tunnel `comment` field, so tunnel ownership relies on the generated name plus local ID record rather than a CF-side tag. **Ingress rules inside the tunnel-config doc are NOT individually tagged** — see D11. |
| D10 | Origin is **always explicit** on `CloudflareExposure.spec.origin` in MVP. There is no `defaultGatewayOrigin` field on `CloudflareTunnel`. Origin host may point at anything reachable from the cloudflared pods (in-cluster Service DNS, LAN hostname, public IP). Auto-derivation from HTTPRoute parentRefs is deferred. |
| D11 | The tunnel-config doc is **single-writer per tunnel**. Exposure controllers do not call the configurations endpoint directly — they enqueue the owning tunnel, and the tunnel reconciler computes and writes the **full** ingress doc from all referencing Exposures. The doc is fully derived from K8s state every reconcile; the operator never adopts pre-existing rules and does not tag individual rules. **Last-write-wins.** Combined with D12 this is safe. |
| D12 | **Leader election is required ON.** Together with D11 this guarantees one writer per tunnel-config doc per cluster. |
| D13 | Cloudflare SDK: **`github.com/cloudflare/cloudflare-go/v4`** (auto-generated client). Wrapped behind an internal interface so a fake can be substituted for tests. Reference the [Cloudflare MCP server](https://github.com/cloudflare/mcp-server-cloudflare) for SDK method discovery during implementation. |
| D14 | Distribution: **Helm chart published as an OCI artifact to GHCR** (D17 for chart shape). Container image also on GHCR. Module path: `github.com/andrewreid/cfzt-operator`. |
| D15 | API version is `v1alpha1`. Breaking changes are permitted without a conversion webhook. Safe targeted migrations may be implemented in reconcilers; otherwise the upgrade strategy is delete and recreate CRs after exporting manifests. |
| D16 | **External (non-Kubernetes) origins are first-class.** A `CloudflareExposure` with no `sourceRef` and an explicit `origin` pointing at a LAN host, public IP, or any address reachable from the cloudflared pods is fully supported. Use case: keeping a Home Assistant / NAS / appliance tunnel in GitOps without making the device a Kubernetes Service. |
| D17 | **Helm chart shape**: hand-written under `charts/cfzt-operator/`. CRDs live in `charts/cfzt-operator/crds/` for Helm 3 native handling (install-only; upgrades do not modify CRDs — matches D15 delete-recreate policy). Templates cover Deployment, ServiceAccount, ClusterRole, ClusterRoleBinding, RBAC for leases. `values.yaml` exposes image repo/tag/pullPolicy, replicas, resources, and logLevel. The chart always passes `--leader-elect=true`; D12 is not user-toggleable in shipped installs. CRDs are NOT installed by `helmify`-style generation — written by hand. |
| D18 | **CI in scope.** GitHub Actions workflows: `ci.yaml` (lint + unit + envtest on PR), `live-smoke.yaml` (manual live Cloudflare smoke), `release.yaml` (manual versioned release → build + push image to GHCR + push Helm OCI chart to GHCR). |
| D19 | **Per-controller `MaxConcurrentReconciles=1`** for all MVP controllers. Tunnel controller remains the single writer per process for the tunnel-config doc. |
| D20 | **Cross-controller wiring**: Tunnel watches Exposure and TunnelRoute; Exposure watches Tunnel and AccessPolicy; AccessPolicy watches Exposure; TunnelRoute watches Tunnel. |
| D21 | **Finalizer string**: `cfzt.reid.ee/finalizer` on all owning CRDs. |
| D22 | **Minimum Kubernetes version: 1.27.** Required for stable CEL validation (`x-kubernetes-validations`) used by CRD schema (see `## CRD validation`). |
| D23 | **GitOps caveat for Helm CRDs**: D17 places CRDs in `charts/cfzt-operator/crds/` (Helm 3 native install-only behaviour). ArgoCD users who render the chart and apply manifests via Application sync will see CRDs *not* upgraded on chart upgrade — matches D15 delete-and-recreate policy. Flux users should set `install.crds: Create` and `upgrade.crds: CreateReplace` with care, again matching D15. Document this in chart `NOTES.txt`. |
| D24 | **`CloudflareAccessPolicy` CRD is in scope, ships in Slice 4.** Cluster-scoped CRD modelling reusable account-level Cloudflare Access policies with a structured rule subset (decisions: allow/deny/bypass/non_identity; rule types: email, email_domain, ip, everyone, service_token, geo; rule groups: include/exclude/require). `CloudflareExposure.spec.access.policyRef` gains a `name` field that references a managed Policy CR; exactly one of `{uuid, name}` is required when `access.enabled: true`. Name-colliding pre-existing CF policies are NOT auto-adopted (mirrors D9): `Reason=ForeignPolicy`. Deletion of a Policy CR is blocked while ≥1 `CloudflareExposure` references it (`Reason=BlockedByExposures`). Policy ownership is recorded via `status.policyId`; the CF-side policy carries `managed-by=cfzt-operator` and `source-uid=<CloudflareAccessPolicy.uid>` in its tags/decoration field (verify via Cloudflare MCP at implementation time — fall back to ID-only tracking like tunnels if no taggable field exists). |
| D25 | **`CloudflareTunnelRoute` CRD is in scope, ships in Slice 5.** Cluster-scoped CRD modelling a single Cloudflare Tunnel private-network route (CIDR → tunnel binding). One CR → one CF route. `spec.tunnelRef.name` references a `CloudflareTunnel`; `spec.network` is a single IPv4 or IPv6 CIDR; `spec.virtualNetworkId` is optional. When unset, the operator omits `virtual_network_id` from Cloudflare route create/update/list calls and lets Cloudflare apply the account default VNet; it does not manage or resolve VNets in Slice 5. `spec.comment` is optional human text, capped to fit beside the compact operator ownership tag in Cloudflare's 100-character route comment. Ownership is recorded via `status.routeId`; the CF-side route `comment` MUST carry `managed-by=cfzt source-uid=<CloudflareTunnelRoute.uid>`. Pre-existing CF routes with the same target CIDR/VNet and no matching source-uid are NOT auto-adopted: `Reason=ForeignRoute`. Tunnel deletion is blocked while ≥1 `CloudflareTunnelRoute` references the tunnel: `Reason=BlockedByRoutes`. WARP / Cloudflare One Client Split Tunnels, Gateway network policies, private DNS, and packet-level validation remain deferred — this CRD only registers the route on the tunnel side. |
| D26 | **Multi-cluster DR is in scope as a per-Exposure opt-in (supersedes D9).** Active-active and cross-cluster federation remain out of scope. Every operator process carries a **mandatory `--site-id`** (Helm `values.site.id`, sane default for clean single-site upgrades). Each cluster runs its own `CloudflareTunnel` (own `metadata.uid`, own generated name, own tunnel ID, own cloudflared DaemonSet), preserving the per-tunnel single-writer invariants (D11+D12+D19) **inside each cluster**. Cross-cluster coordination for a `CloudflareExposure` with `spec.failover` set is mediated by a **best-effort** Cloudflare **DNS TXT lease record** at `_cfzt-lease.<hash8(failover.group)>.<zone>`. Cloudflare DNS offers no conditional-write precondition nor TXT-uniqueness guarantee, so the lease is **not linearizable** — it is an optimistic, eventually-consistent coordination hint, **not a mutual-exclusion lock**. Correctness does not depend on lease atomicity. Safety rests on two properties: (1) a single public CNAME targets at most one tunnel ID, so two sites can never serve divergent origins; (2) a failed primary loses its `cloudflared` edge connection within seconds, so Cloudflare stops routing to its tunnel ID regardless of lease state. The lease's only job is to **damp CNAME flapping** between two healthy sites by electing a single writer. Simultaneous acquisition of an absent lease can transiently create duplicate TXT records; the controller detects `>1` group-owned record and **resolves deterministically** (among unexpired records the lowest `site-id` wins; losers delete their own duplicate and demote), converging to one record without human intervention. Unresolvable ambiguity **fails closed** (`Ready=False, Reason=LeaseConflict`, no shared write). Exactly one site holds the lease and is **Primary**; it alone writes the public DNS CNAME and the Access application for the hostname. Promotion is **automatic on lease TTL expiry**; an emergency `cfzt.reid.ee/force-promote` **token** annotation overrides expiry. The controller records the last-honored token in `status.failover` and never mutates the annotation, so a re-applied (GitOps) token does not replay. A recovered former primary that finds the lease held by another site **demotes itself to Standby** and does not auto-steal back. For failover-enabled Exposures the `source-uid` on the shared Access application and public DNS CNAME is the **failover-group ID**, not the per-CR `uid`; ownership-tag verification accepts that group ID from either cluster. Two failover Exposures in the **same cluster** MUST NOT share a `group` (cluster-wide guard; controller surfaces `Reason=FailoverGroupConflict`, no CF write). `dns.manage: true` on the referenced `CloudflareTunnel` is **required**; the lease TXT reuses the existing `Zone:DNS:Edit` scope. `--site-id` must be **distinct per cluster**: a failover Exposure on a process still running the chart-default `site.id` goes `Ready=False, Reason=FailoverRequiresDistinctSiteID`. The collision-safety mechanics of the superseded D9 (generated tunnel names + `status.tunnelId`, source-uid tags on Access apps and DNS records, untagged ingress rules per D11) remain in force unchanged. |

## Design philosophy

The operator is lightweight, explicit, and easy to reason about.

`CloudflareExposure` is the workload interface. Each exposure is one small CR per workload. CRD spec fields are a stable contract.

The translation pipeline is:

```text
CloudflareExposure
  -> Cloudflare published route (added to tunnel-config doc)
  -> Cloudflare Access application
  -> Cloudflare Access policy binding
  -> Cloudflare DNS CNAME (when D2 manage=true)
```

`CloudflareTunnel` owns the tunnel and its connector. `CloudflareExposure` owns one hostname's worth of intent. Reconciled by separate controllers but the tunnel reconciler depends on the set of Exposures referencing it (D11, D20).

## MVP scope

Supported:

- Kubebuilder project scaffold under module `github.com/andrewreid/cfzt-operator`.
- One operator Deployment with leader election (D12).
- API group: `cfzt.reid.ee`.
- Cluster-scoped `CloudflareTunnel` CRD.
- Namespaced `CloudflareExposure` CRD.
- Creation of Cloudflare Tunnels with generated Cloudflare names. Recovery is limited to exact generated-name matches — see D9 (`status.tunnelId` plus `<spec.tunnelName>-cfzt-<hash8(uid)>`; legacy name-collision without a local ID record → `Reason=ForeignTunnel`).
- Per-tunnel token retrieval and storage in a Kubernetes Secret.
- Managed cloudflared DaemonSet per `CloudflareTunnel`.
- Creation and update of Cloudflare published hostname routes via tunnel-config (D1, D11).
- Creation and update of Cloudflare DNS CNAMEs (D2, when enabled).
- Creation and update of Cloudflare Access applications.
- Binding of pre-existing Access policies to Access applications by UUID (D3 / D24 — MVP Slices 1–2).
- Managed `CloudflareAccessPolicy` CRD with structured rule subset (D24 — Slice 4).
- Cluster-scoped `CloudflareTunnelRoute` CRD: private network CIDR-to-tunnel routes (D25 — Slice 5).
- Active-passive multi-cluster DR via per-Exposure `spec.failover` + DNS TXT lease (D26 — Slice 6).
- External (non-K8s) origins (D16).
- `Ready` and `Progressing` conditions on all CRDs (D8).
- Finalizers for owned Cloudflare resources.
- Helm OCI chart (D17) + CI/release pipeline (D18).

Deferred:

- Annotation-driven UX (D5, post-MVP convenience layer).
- Ingress source support.
- WARP routing.
- Device posture management.
- Gateway policy management.
- Full Cloudflare Gateway management.
- Multi-account support.
- Multi-cluster active-active / federation (D26 covers active-passive DR only).
- Operator Lifecycle Manager packaging.
- Cross-cluster federation.
- Validating webhooks (CRD validation rules only in MVP — see `## CRD validation`).
- Conversion webhooks (D15).
- Kubernetes versions <1.27 (D22).

## Cloudflare dashboard workflow being modelled

The operator models the manual Cloudflare Zero Trust dashboard workflow for a self-hosted application:

1. Create a published application route on a tunnel.
   - Name, e.g. `Jellyfin`.
   - Public hostname, e.g. `jellyfin.reid.ee`.
   - Private origin URL, e.g. `http://jellyfin.media.svc.cluster.local:8096`.
   - Attached to a Cloudflare Tunnel.

2. Create a Cloudflare Access application protecting that hostname.
   - Application name, e.g. `Jellyfin`.
   - Bound to `jellyfin.reid.ee`.
   - Attached to a policy, e.g. `family-only`.

The operator treats these as separate Cloudflare resources reconciled from one `CloudflareExposure`.

## Primary user experience

A user creates one `CloudflareTunnel` per tunnel and one `CloudflareExposure` per workload.

### Minimal `CloudflareExposure` (in-cluster Service origin)

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: jellyfin
  namespace: media
spec:
  hostname: jellyfin.reid.ee
  tunnelRef:
    name: homelab
  origin:
    protocol: http
    host: jellyfin.media.svc.cluster.local
    port: 8096
  access:
    enabled: true
    policyRef:
      uuid: 0123abcd-4567-89ef-0123-456789abcdef
```

### External origin (D16) — Home Assistant on the LAN

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: home-assistant
  namespace: home
spec:
  hostname: ha.reid.ee
  tunnelRef:
    name: homelab
  origin:
    protocol: http
    host: homeassistant.lan        # any address reachable from cloudflared pods
    port: 8123
  access:
    enabled: true
    policyRef:
      uuid: 0123abcd-4567-89ef-0123-456789abcdef
```

Requirement: cloudflared pods must have a network path to the origin. With default cluster networking, in-cluster Service DNS works out of the box. For LAN targets, cluster nodes must route to the LAN (most home clusters do) or cloudflared must run with `hostNetwork: true` — settable via `spec.cloudflared.hostNetwork`.

### With optional `sourceRef` (Slice 3)

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: jellyfin
  namespace: media
spec:
  hostname: jellyfin.reid.ee
  tunnelRef:
    name: homelab
  sourceRef:                       # optional
    apiVersion: v1
    kind: Service
    name: jellyfin
  origin:
    protocol: http
    host: jellyfin.media.svc.cluster.local
    port: 8096
  access:
    enabled: true
    policyRef:
      uuid: 0123abcd-4567-89ef-0123-456789abcdef
```

When `sourceRef` is set and resolves to a same-namespace resource, the controller adds an `ownerReference` from the `CloudflareExposure` to the source resource. Deleting the source garbage-collects the Exposure, which fires the Exposure finalizer and cleans Cloudflare state.

`origin` remains explicit even when `sourceRef` is present in MVP. Auto-derivation from Service ports / HTTPRoute backends is a Slice 3 convenience that fills `origin` defaults when fields are empty.

### With DR failover (Slice 6, D26)

The same `CloudflareExposure` manifest is applied — via GitOps — to **two** clusters that both run cfzt-operator (each with its own `--site-id`) against the same Cloudflare account. `spec.failover.group` ties the two copies together as one logical exposure; whichever cluster holds the DNS TXT lease serves the hostname.

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: jellyfin
  namespace: media
spec:
  hostname: jellyfin.reid.ee
  tunnelRef:
    name: homelab                  # a per-cluster CloudflareTunnel (dns.manage: true required)
  origin:
    protocol: http
    host: jellyfin.media.svc.cluster.local
    port: 8096
  access:
    enabled: true
    policyRef:
      uuid: 0123abcd-4567-89ef-0123-456789abcdef
  failover:
    group: jellyfin-dr             # identical on both clusters
    leaseSeconds: 60               # optional
```

On the cluster holding the lease, `status.failover.role` is `Primary`; on the other it is `Standby` with `leaseOwner` naming the primary's site ID. See `## DR failover`.

## CRD model

### CloudflareTunnel (cluster-scoped)

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareTunnel
metadata:
  name: homelab
spec:
  credentialsSecretRef:
    name: cloudflare-credentials
    keys:
      accountId: accountId           # default "accountId"
      apiToken: apiToken             # default "apiToken"
  tunnelName: homelab-rke2           # human base name; CF name becomes homelab-rke2-cfzt-<hash8(uid)>
  dns:
    manage: true                     # D2 default
  cloudflared:
    namespace: cfzt-system           # default cfzt-system
    image: cloudflare/cloudflared:2025.1.0           # operator pins a default
    hostNetwork: false               # set true to reach LAN origins (D16)
    resources: {}
    nodeSelector: {}
    tolerations: []
    affinity: {}
status:
  tunnelId: ""
  tokenSecretRef:
    name: ""
  dnsMode: ""                        # "managed" or "external"
  routes:                            # written by Tunnel controller per D11/D20
    - exposureUid: 4f8b1c2e-...      # CloudflareExposure metadata.uid
      namespace: media
      name: jellyfin
      hostname: jellyfin.reid.ee
      hash: sha256:abcdef...         # sha256 of the canonical rule JSON
      lastWrittenAt: 2026-05-19T10:00:00Z
  conditions: []
```

Kubebuilder markers (non-exhaustive):

- `+kubebuilder:resource:scope=Cluster,shortName=cft`
- `+kubebuilder:subresource:status`
- `+kubebuilder:printcolumn:name=Tunnel,type=string,JSONPath=.spec.tunnelName`
- `+kubebuilder:printcolumn:name=TunnelID,type=string,JSONPath=.status.tunnelId`
- `+kubebuilder:printcolumn:name=Ready,type=string,JSONPath=.status.conditions[?(@.type=="Ready")].status`
- `+kubebuilder:printcolumn:name=Age,type=date,JSONPath=.metadata.creationTimestamp`

`CloudflareTunnel` controller responsibilities:

1. Resolve Cloudflare credentials from the referenced Secret.
2. Reconcile tunnel identity (see D9 + `## Ownership and deletion semantics`): desired Cloudflare name is `<spec.tunnelName>-cfzt-<hash8(metadata.uid)>`. If `status.tunnelId` is set, `Get(id)` and verify the name is desired, or rename legacy `spec.tunnelName` to the desired generated name. If unset, recover exactly one generated-name tunnel by writing its ID to status; a legacy unsuffixed match remains `Ready=False, Reason=ForeignTunnel`; otherwise create the generated-name tunnel.
3. Store the Cloudflare tunnel ID in `status.tunnelId`.
4. Retrieve the tunnel token (D4) via the SDK and create/update the token Secret. Record its name in `status.tokenSecretRef`.
5. Create or update the cloudflared DaemonSet (D7) in `spec.cloudflared.namespace`. Stamp a token-checksum annotation on the pod template so token rotations roll the workload.
6. List all `CloudflareExposure` resources referencing this tunnel; compute desired ingress doc; PUT it.
7. Write per-Exposure route hashes into Tunnel `status.routes[]` keyed by Exposure UID.
8. Set `Ready=True` once tunnel exists, token Secret exists, DaemonSet has at least one ready pod, and the latest ingress write succeeded.

### CloudflareExposure (namespaced)

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: jellyfin
  namespace: media
spec:
  displayName: Jellyfin              # optional, defaults to metadata.name
  hostname: jellyfin.reid.ee
  tunnelRef:
    name: homelab
  sourceRef:                         # optional; Slice 3 derives origin
    apiVersion: v1
    kind: Service
    name: jellyfin
  origin:
    protocol: http
    host: jellyfin.media.svc.cluster.local
    port: 8096
  access:
    enabled: true
    policyRef:
      # exactly one of {uuid, name} when access.enabled: true
      uuid: 0123abcd-4567-89ef-0123-456789abcdef    # bind a Cloudflare-managed policy by UUID
      # name: family-only                            # OR reference a CloudflareAccessPolicy CR (Slice 4, D24)
  failover:                          # optional; Slice 6 (D26). Cross-cluster active-passive DR.
    group: jellyfin-dr               # required when failover present; logical-exposure identity
    leaseSeconds: 60                 # optional; default 60, min 30, max 600
status:
  cloudflare:
    accessApplicationId: ""
    publicHostnameRouteHash: ""      # SHA of the rule the tunnel reconciler placed
    dnsRecordId: ""                  # only when D2 manage=true
  failover:                          # populated only when spec.failover is set (Slice 6, D26)
    role: Standby                    # Primary | Standby | Unknown
    siteId: homelab-dr               # this process's --site-id
    leaseOwner: homelab-primary      # site-id currently holding the lease
    leaseExpiresAt: 2026-06-02T10:00:00Z
    leaseRenewedAt: 2026-06-02T09:59:30Z
    lastRoleTransitionAt: 2026-06-02T09:30:00Z
    observedPrimaryTunnelId: ""      # tunnel ID recorded in the lease record
    lastForcePromoteToken: ""        # last honored cfzt.reid.ee/force-promote token (replay guard)
  conditions: []
```

Kubebuilder markers:

- `+kubebuilder:resource:scope=Namespaced,shortName=cfe`
- `+kubebuilder:subresource:status`
- `+kubebuilder:printcolumn:name=Hostname,type=string,JSONPath=.spec.hostname`
- `+kubebuilder:printcolumn:name=Tunnel,type=string,JSONPath=.spec.tunnelRef.name`
- `+kubebuilder:printcolumn:name=Access,type=boolean,JSONPath=.spec.access.enabled`
- `+kubebuilder:printcolumn:name=Role,type=string,JSONPath=.status.failover.role` (Slice 6, D26; empty for non-failover Exposures)
- `+kubebuilder:printcolumn:name=Ready,type=string,JSONPath=.status.conditions[?(@.type=="Ready")].status`
- `+kubebuilder:printcolumn:name=Age,type=date,JSONPath=.metadata.creationTimestamp`

`CloudflareExposure` controller responsibilities:

1. Resolve the referenced `CloudflareTunnel` and read its credentials.
2. Validate origin and hostname.
3. Ensure the Access application exists for `spec.hostname` when `access.enabled: true`. Resolve the bound policy UUID:
   - `policyRef.uuid` set → use verbatim (D3).
   - `policyRef.name` set → `Get` the named `CloudflareAccessPolicy`; if its `status.policyId` is empty or `Ready=False`, surface `Ready=False, Reason=PolicyNotReady` on this Exposure and requeue. Otherwise use `status.policyId`.
   - Bind the resolved UUID via `Applications.Policies.Update` (D24, Slice 4).
4. Ensure the proxied DNS CNAME exists for `spec.hostname` → `<tunnelId>.cfargotunnel.com` when the tunnel has `dns.manage: true`.
5. Enqueue the referenced `CloudflareTunnel` so the tunnel reconciler updates the ingress doc (D11).
6. Read back the route placement from the tunnel's `status.routes[]` and record `publicHostnameRouteHash` in this Exposure's status.
7. Set `Ready=True` once Access (if enabled), DNS (if managed), and the published route are all in their desired state.
8. On deletion, run a finalizer (`cfzt.reid.ee/finalizer`) that removes DNS record + Access app, then enqueues the tunnel for ingress-doc update.

`access.enabled: false` skips Access application creation; the hostname is reachable without auth.

When `spec.failover` is set (Slice 6, D26), the controller first reads the DNS TXT lease and determines its role **before** steps 3–4. A **Standby** writes `status.failover` and returns without touching the Access application or public DNS CNAME; a **Primary** proceeds through steps 3–4, using the failover-group ID as the `source-uid` on the Access app and DNS record, and runs a lease-renewal loop. Promotion, demotion, and split-brain handling are specified in `## DR failover`.

### CloudflareAccessPolicy (cluster-scoped)

Ships in Slice 4 (D24). Models a reusable account-level Cloudflare Access policy. One CR → one CF policy, referenced by N `CloudflareExposure` CRs via `spec.access.policyRef.name`.

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareAccessPolicy
metadata:
  name: family-only
spec:
  credentialsSecretRef:
    name: cloudflare-credentials
    namespace: cfzt-system          # required; cluster-scoped CR has no implicit namespace
    keys:
      accountId: accountId          # default "accountId"
      apiToken: apiToken            # default "apiToken"
  policyName: family-only           # base name; Cloudflare name becomes "family-only-cfzt"
  decision: allow                   # allow | deny | bypass | non_identity
  rules:
    include:                        # any-of match
      - emailDomain: reid.ee
      - email: alice@example.com
    exclude: []                     # none-of match
    require: []                     # all-of match
  sessionDuration: 24h              # optional
  purposeJustification:
    required: false
    prompt: ""                      # optional
status:
  policyId: ""                      # CF policy UUID once created
  observedRulesHash: ""             # sha256 of canonical rules JSON, for drift detection
  referencedBy:                     # populated from cross-watch on Exposures
    - namespace: media
      name: jellyfin
      uid: 4f8b1c2e-...
  referencedByCount: 0              # convenience int for kubectl printcolumn
  conditions: []
```

Rule item shape — discriminated union, exactly one field set per item:

```go
type AccessRule struct {
    Email          string `json:"email,omitempty"`           // exact email
    EmailDomain    string `json:"emailDomain,omitempty"`     // domain (no @)
    IP             string `json:"ip,omitempty"`              // IP or CIDR
    Everyone       bool   `json:"everyone,omitempty"`        // true => match all
    ServiceToken   string `json:"serviceToken,omitempty"`    // service token id (UUID)
    GeoCountryCode string `json:"geoCountryCode,omitempty"`  // ISO 3166-1 alpha-2
}
```

Kubebuilder markers:

- `+kubebuilder:resource:scope=Cluster,shortName=cfap`
- `+kubebuilder:subresource:status`
- `+kubebuilder:printcolumn:name=Decision,type=string,JSONPath=.spec.decision`
- `+kubebuilder:printcolumn:name=PolicyID,type=string,JSONPath=.status.policyId`
- `+kubebuilder:printcolumn:name=RefBy,type=integer,JSONPath=.status.referencedByCount`
- `+kubebuilder:printcolumn:name=Ready,type=string,JSONPath=.status.conditions[?(@.type=="Ready")].status`
- `+kubebuilder:printcolumn:name=Age,type=date,JSONPath=.metadata.creationTimestamp`

`CloudflareAccessPolicy` controller responsibilities:

1. Resolve Cloudflare credentials from the referenced Secret (same shape as Tunnel; namespace stored on the field).
2. Reconcile policy identity by ID-record: if `status.policyId` is set, `Get(id)` and verify name matches desired Cloudflare name (`<spec.policyName | metadata.name>-cfzt`); if unset, zero same-name hits → create, one same-name exact rule-shape match → recover into status, any mismatch → `Ready=False, Reason=ForeignPolicy`, no mutation.
3. Compare desired rules JSON against `status.observedRulesHash`; on mismatch, `Update` the CF policy and rewrite the hash.
4. List `CloudflareExposure` CRs referencing this Policy CR by `spec.access.policyRef.name`; populate `status.referencedBy[]` and `status.referencedByCount`.
5. Enqueue each referencing Exposure when `status.policyId` first becomes set or `observedRulesHash` changes (cross-watch — see additions to `## Tunnel configuration concurrency`).
6. Finalizer `cfzt.reid.ee/finalizer`: blocks deletion while `len(status.referencedBy) > 0` → `Ready=False, Reason=BlockedByExposures`. Once unblocked: delete CF policy only when `status.policyId` resolves and the Cloudflare policy name still matches the desired name; remove finalizer.
7. Set `Ready=True` once `status.policyId` is set and `observedRulesHash` matches desired rules.

### CloudflareTunnelRoute (cluster-scoped)

Ships in Slice 5 (D25). Models a single Cloudflare Tunnel private-network route — a CIDR (IPv4 or IPv6) bound to a tunnel so WARP clients (or peer tunnels) can reach IPs inside the CIDR. One CR → one CF route. WARP / Cloudflare One Client Split Tunnels, Gateway network policies, private DNS, and packet-level validation are out of scope; this CRD only registers the route on the tunnel side.

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareTunnelRoute
metadata:
  name: homelab-lan-v4
spec:
  tunnelRef:
    name: homelab
  network: 172.16.0.0/24            # IPv4 or IPv6 CIDR
  virtualNetworkId: ""              # optional; omitted from CF calls when unset
  comment: "homelab LAN via WARP"   # optional human label, max 34 chars
status:
  routeId: ""                       # CF route UUID once created
  virtualNetworkId: ""              # CF value returned by the route API, if any
  conditions: []
```

Kubebuilder markers:

- `+kubebuilder:resource:scope=Cluster,shortName=cftr`
- `+kubebuilder:subresource:status`
- `+kubebuilder:printcolumn:name=Network,type=string,JSONPath=.spec.network`
- `+kubebuilder:printcolumn:name=Tunnel,type=string,JSONPath=.spec.tunnelRef.name`
- `+kubebuilder:printcolumn:name=RouteID,type=string,JSONPath=.status.routeId`
- `+kubebuilder:printcolumn:name=Ready,type=string,JSONPath=.status.conditions[?(@.type=="Ready")].status`
- `+kubebuilder:printcolumn:name=Age,type=date,JSONPath=.metadata.creationTimestamp`

`CloudflareTunnelRoute` controller responsibilities:

1. Resolve the referenced `CloudflareTunnel`; read credentials via the same path as the Exposure controller (Tunnel's `credentialsSecretRef` in its `cloudflared.namespace`). If the tunnel does not exist, is deleting, or has no `status.tunnelId`, surface `Reason=TunnelNotReady` and requeue. Do not require `CloudflareTunnel Ready=True`; private route registration only needs the CF tunnel ID and credentials.
2. Canonicalise `spec.network` with `net/netip.ParsePrefix` and `Prefix.Masked().String()` for both IPv4 and IPv6. Invalid CIDR → `Ready=False, Reason=NetworkInvalid` with no Cloudflare call.
3. Reconcile route identity by ID-record (mirrors D9 tunnel pattern): if `status.routeId` is set, `Get(id)` and verify canonical network, tunnel ID, and VNet match spec. If unset, `List` active routes with `tun_types=cfd_tunnel`, optional `tunnel_id`, and optional `virtual_network_id` only when `spec.virtualNetworkId` is set; exact-filter in code by canonical CIDR and VNet. Zero hits → create with compact ownership comment `managed-by=cfzt source-uid=<uid>` plus optional ` | <spec.comment>`; one or more hits → check the `comment` for matching source-uid (mutation allowed) or refuse with `Reason=ForeignRoute` (no mutation). When `spec.virtualNetworkId` is unset, the operator omits `virtual_network_id` from List/New/Edit and fails closed on exact-CIDR foreign matches because it deliberately does not resolve the account default VNet in Slice 5.
4. Compare desired network / VNet / comment against the live route; on drift, preflight the target CIDR/VNet for foreign active routes before calling `Edit`. Network and non-empty VNet changes are allowed via update (no delete-recreate), but clearing VNet after it was set is rejected by CRD validation. A different existing route at the target CIDR/VNet → `Ready=False, Reason=ForeignRoute`.
5. Set `status.routeId` and `status.virtualNetworkId` on success.
6. Finalizer `cfzt.reid.ee/finalizer`: on deletion, `Get(routeId)`, verify the compact comment source-uid matches, then `Delete` the route; remove finalizer. Foreign-tagged or missing-tag route → leave alone, remove finalizer (the route is not ours to delete).
7. Set `Ready=True` once the route is created and matches desired state.

## CRD validation

CRD fields are validated by `+kubebuilder:validation:*` markers and `x-kubernetes-validations` CEL rules. No validating webhook in MVP.

`CloudflareExposure`:

- `spec.hostname`: required, pattern matching RFC 1123 subdomain (`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`).
- `spec.tunnelRef.name`: required, minLength 1.
- `spec.origin`: required in MVP (CEL rule asserts presence; Slice 3 relaxes when `sourceRef` is set).
- `spec.origin.protocol`: enum `http|https`.
- `spec.origin.host`: required when `spec.origin` present, minLength 1.
- `spec.origin.port`: int, 1–65535.
- `spec.access.enabled`: bool, default `false`.
- `spec.access.policyRef.uuid`: optional, pattern UUID v4.
- `spec.access.policyRef.name`: optional, RFC 1123 subdomain pattern, maxLength 253 (refers to a `CloudflareAccessPolicy` `metadata.name`).
- CEL on `spec.access`: when `enabled: true`, exactly one of `policyRef.uuid` or `policyRef.name` must be set:

  ```
  +kubebuilder:validation:XValidation:rule="!has(self.access) || !self.access.enabled || (has(self.access.policyRef) && ((has(self.access.policyRef.uuid) && size(self.access.policyRef.uuid) > 0) != (has(self.access.policyRef.name) && size(self.access.policyRef.name) > 0)))",message="access.policyRef requires exactly one of uuid or name when access.enabled is true"
  ```

- `spec.failover.group` (Slice 6, D26): required when `spec.failover` is present; RFC 1123 label pattern (`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`); minLength 3, maxLength 63.
- `spec.failover.leaseSeconds` (Slice 6, D26): optional int, default 60, minimum 30, maximum 600.
- The requirement that `spec.failover` needs the referenced `CloudflareTunnel` to have `dns.manage: true` is **not** expressible in CEL (cross-resource read) and is enforced in the controller: a failover Exposure on an unmanaged-DNS tunnel goes `Ready=False, Reason=FailoverRequiresManagedDNS` and does not write a lease.

`CloudflareTunnel`:

- `spec.tunnelName`: required, minLength 1, maxLength 106. Effective Cloudflare tunnel name is `<spec.tunnelName>-cfzt-<hash8(metadata.uid)>`, capped to the existing 120-character budget.
- `spec.credentialsSecretRef.name`: required.
- `spec.credentialsSecretRef.keys.accountId`: optional, default `accountId`, maxLength 253.
- `spec.credentialsSecretRef.keys.apiToken`: optional, default `apiToken`, maxLength 253.
- Credentials Secret namespace is always `spec.cloudflared.namespace` (default `cfzt-system`); the API intentionally stores that namespace once.
- `spec.dns.manage`: bool, default `true`.
- `spec.cloudflared.image`: image-reference pattern allows an optional registry port, not allowed to end `:latest`.

`CloudflareAccessPolicy` (Slice 4, D24):

- `spec.credentialsSecretRef.name`: required.
- `spec.credentialsSecretRef.namespace`: required, RFC 1123 subdomain (cluster-scoped CR — namespace is stored on the field, not inherited).
- `spec.credentialsSecretRef.keys.accountId`: optional, default `accountId`, maxLength 253.
- `spec.credentialsSecretRef.keys.apiToken`: optional, default `apiToken`, maxLength 253.
- `spec.policyName`: optional base name; Cloudflare policy name is always `<spec.policyName | metadata.name>-cfzt`; allowed charset matches CF Access policy name rules; maxLength 120.
- `spec.decision`: required, enum `allow|deny|bypass|non_identity`.
- `spec.rules.include`, `spec.rules.exclude`, `spec.rules.require`: optional lists of rule items (default `[]`).
- Each rule item is a discriminated union with **exactly one** of `email`, `emailDomain`, `ip`, `everyone`, `serviceToken`, `geoCountryCode` set. CEL: `[has(self.email), has(self.emailDomain), has(self.ip), has(self.everyone), has(self.serviceToken), has(self.geoCountryCode)].filter(b, b).size() == 1`.
- Spec-level CEL: `size(self.rules.include) + size(self.rules.exclude) + size(self.rules.require) >= 1` — empty policies forbidden.
- `spec.sessionDuration`: optional, pattern `^[0-9]+(s|m|h|d|w|mo|y)$`.
- `spec.purposeJustification.required`: bool, default `false`.
- `spec.purposeJustification.prompt`: optional, maxLength 1000.

`CloudflareTunnelRoute` (Slice 5, D25):

- `spec.tunnelRef.name`: required, minLength 1, maxLength 253.
- `spec.network`: required. CEL validates one of:
  - IPv4 CIDR pattern `^([0-9]{1,3}\.){3}[0-9]{1,3}/([0-9]|[1-2][0-9]|3[0-2])$`
  - IPv6 CIDR pattern (compressed form accepted) `^([0-9a-fA-F:]+)/([0-9]|[1-9][0-9]|1[0-1][0-9]|12[0-8])$`
- Controller validation MUST parse the CIDR with `net/netip.ParsePrefix` for both IPv4 and IPv6, reject non-canonical-invalid addresses missed by regex (for example IPv4 octets >255), and compare routes using the masked canonical CIDR string.
- `spec.virtualNetworkId`: optional, general UUID pattern when set. Explicit empty string is accepted as unset. When unset, the operator omits `virtual_network_id` from Cloudflare route SDK params. Clearing a previously set value is rejected; delete and recreate the route to return to the account default VNet.
- `spec.comment`: optional, maxLength 34. Cloudflare route comments are max 100 chars; the compact ownership prefix `managed-by=cfzt source-uid=<36-char uid>` plus ` | ` leaves 34 chars for user text.
- Immutability CEL: `spec.tunnelRef.name` is immutable post-creation (routes do not migrate between tunnels via spec edit; delete + recreate). `spec.network` and non-empty `spec.virtualNetworkId` changes are mutable; the controller preflights target CIDR/VNet conflicts before `Edit`. Clearing `spec.virtualNetworkId` after it has been set is rejected because Slice 5 deliberately omits `virtual_network_id` when unset and does not send an API null to move a route back to the account default VNet.

## Tunnel configuration concurrency

D1 forces all ingress rules for a tunnel into a single configuration doc. D11 makes the `CloudflareTunnel` controller the sole writer. D12 makes the operator process the sole writer cluster-wide. D19 makes the Tunnel controller `MaxConcurrentReconciles=1`.

Flow:

```text
CloudflareExposure controller
  validates Exposure, ensures Access + DNS, writes its own status,
  enqueues the referenced CloudflareTunnel for reconcile.

CloudflareTunnel controller
  lists all CloudflareExposures with spec.tunnelRef.name == this tunnel's name,
  computes the desired ingress[] array (deterministic ordering by hostname),
  PUTs the new doc (last-write-wins; safe due to D11+D12+D19),
  writes per-Exposure route hashes back to its own status.routes[].
```

The Exposure controller MUST NOT call the tunnel-configurations endpoint. The Tunnel controller MUST NOT directly modify Exposure CRs (writes its own status only; Exposure reconciles re-read via D20 watch).

Cross-controller watches (D20):

- Tunnel controller `.Watches(&CloudflareExposure{}, EnqueueRequestsFromMapFunc(exposureToTunnel))`.
- Exposure controller `.Watches(&CloudflareTunnel{}, EnqueueRequestsFromMapFunc(tunnelToExposures))`.

**Slice 4 additions (D24)**:

- Policy controller `.Watches(&CloudflareExposure{}, EnqueueRequestsFromMapFunc(exposureToPolicy))` — refresh `status.referencedBy[]` when Exposures change `policyRef.name`.
- Exposure controller `.Watches(&CloudflareAccessPolicy{}, EnqueueRequestsFromMapFunc(policyToExposures))` — propagate `status.policyId` becoming set into binding.
- Policy controller `MaxConcurrentReconciles=1` (single writer per CF policy; raise post-Slice 4 if needed). Extends D19.

## DR failover

D26 adds active-passive disaster-recovery across two (or more) clusters that run cfzt-operator against the **same Cloudflare account** with **identical GitOps state**. The goal: when the cluster currently serving a hostname fails, a standby cluster takes over the public surface; when the failed cluster recovers, it learns it is no longer the authoritative writer and stands down without thrashing Cloudflare state.

This is **opt-in per Exposure** and changes nothing for Exposures without `spec.failover`.

### Site identity

Every operator process is started with a **mandatory `--site-id=<stable string>`** (for example `homelab-primary`, `homelab-dr`). It is surfaced via Helm `values.site.id`, which carries a sane default so existing single-site installs upgrade cleanly without a values change. The site ID is the identity a cluster writes into the lease record and compares against on every reconcile. It is validated as non-empty at manager boot; an empty value is a fatal start-up error (there is no per-Exposure `SiteIDMissing` reason — identity is a process-level invariant).

### Per-cluster tunnels (warm standby)

Each cluster owns its **own** `CloudflareTunnel`: distinct `metadata.uid` → distinct generated Cloudflare name → distinct tunnel ID → distinct cloudflared DaemonSet. The per-tunnel single-writer invariants (D11+D12+D19) are therefore untouched inside each cluster — no cluster ever writes another cluster's tunnel-config doc.

Standby clusters keep their tunnel and cloudflared DaemonSet **running and connected to the Cloudflare edge** (warm). Failover is then a DNS CNAME swap, not a connector cold-start; RTO is bounded by Cloudflare edge propagation rather than pod scheduling.

### `CloudflareExposure.spec.failover`

```yaml
spec:
  failover:
    group: jellyfin-dr         # required when the block is present; logical-exposure identity
    leaseSeconds: 60           # optional; default 60, min 30, max 600
```

`group` is an explicit, user-supplied identifier. Exposures sharing the same `group` across clusters are treated as **one logical exposure**. It is deliberately **not** derived from `spec.hostname` so that renaming a hostname does not silently break the failover relationship. Two failover-enabled Exposures in the **same cluster** MUST NOT share a `group` (controller surfaces `Reason=FailoverGroupConflict`, no CF write). The guard is cluster-wide, not per-namespace: the lease name carries no namespace and all Exposures in a cluster share one `--site-id`, so two same-group members in one cluster would resolve to the same lease record and each read it as self-owned with no mutual exclusion. The legitimate cross-cluster pair lives in separate apiservers, so each operator only ever lists its own cluster and never trips this guard.

Failover requires the referenced `CloudflareTunnel` to have `dns.manage: true`. With `dns.manage: false` the operator creates no DNS records and has no lease substrate, so the Exposure goes `Ready=False, Reason=FailoverRequiresManagedDNS` and writes no lease (see `## CRD validation`). `access.enabled: false` is allowed: there is no Access application to coordinate, but the lease still arbitrates which cluster owns the public DNS CNAME.

### Lease record

The lease lives in a Cloudflare **DNS TXT record** so it needs no infrastructure beyond the DNS access the operator already holds (D2 / `Zone:DNS:Edit`). Location:

```text
_cfzt-lease.<hash8(failover.group)>.<zone>
```

`<zone>` is resolved by the same longest-suffix zone match used for the public hostname (`## DNS management`). `hash8(failover.group)` keeps the record name bounded and avoids leaking the group string into DNS. Payload (single TXT string):

```text
v=1 site=<site-id> tunnel=<tunnelId> exp=<unix-epoch> renewed=<unix-epoch>
```

- `site` — the lease holder's `--site-id`.
- `tunnel` — the holder's Cloudflare tunnel ID. Lets a standby observer know which tunnel ID the public CNAME currently targets (split-brain diagnostics).
- `exp` — lease expiry, `renewed + leaseSeconds`.
- `renewed` — last successful renewal time.

### Lease write protocol (best-effort, not atomic)

**Cloudflare DNS provides no conditional-write precondition and no TXT-uniqueness guarantee.** There is no `record_id`-conditioned update and no "create if absent". The lease is therefore an *optimistic* coordination record, not a compare-and-swap lock; the protocol is designed to **converge** and to **fail closed**, not to be linearizable. Each reconcile:

1. **List** the lease TXT records at the lease name and filter to group-owned ones (`MatchesComment` with the failover-group ID).
2. **Count**:
   - `0` records → no lease. Acquire path: `Create` the lease, then **read back** (step 4).
   - `1` record → the normal case. Feed it to the role state machine; on a write (acquire/renew) `Update` by its `record_id`, then read back (step 4).
   - `>1` records → a create race produced duplicates. **Resolve deterministically** (see `### Duplicate-lease resolution`).
3. **Renew / acquire** writes the desired payload. Because the write is not conditional, two sites racing from an absent lease can both `Create`; that is expected and handled by steps 2/4, not prevented.
4. **Read-back verification**: after any acquire/renew write, re-list. If `>1` group-owned record now exists, resolve (step 2 `>1`). If the surviving single record is not this site's, this site lost the race → demote to Standby. This bounds the dual-writer window to roughly one reconcile.

`record_id` is used only as the address for `Update`/`Delete` of a record this site has just observed; it is **not** a precondition and confers no atomicity. The `MatchesComment` ownership check is the guard that keeps the controller from ever touching a foreign (non-group) TXT record at the lease name.

### Duplicate-lease resolution

When `>1` group-owned lease records exist at the lease name, every site computes the **same** winner from the record set, so resolution is deterministic and needs no coordination:

1. Among **unexpired** records, the winner is the one with the lexicographically-lowest `site`; if all are expired, the lowest `site` across all records wins; ties on `site` break on lowest `record_id`.
2. If this site is the winner, it **deletes the other** group-owned duplicates by `record_id` and keeps its own.
3. If this site is not the winner, it **deletes only its own** duplicate (if present) and demotes to Standby; it never touches the winner's record.

Both sites perform idempotent deletes (`ErrNotFound` tolerated), so the set converges to a single record within a reconcile or two. If duplicates persist beyond resolution (e.g. a record cannot be parsed), the controller **fails closed**: `Ready=False, Reason=LeaseConflict`, no shared Access/DNS write, requeue 30s. This race only arises from simultaneous acquisition of an *absent* lease (a cold start); renewals operate on the single owned record and never create duplicates.

### Role state machine

```text
Unknown ──read lease──▶ Standby ──acquire (expiry or force token)──▶ Primary
                          ▲                                           │
                          └──────── lease lost / observed foreign ────┘
```

- **Unknown** — initial, before the first lease read.
- **Standby** — lease held by another unexpired site. The controller writes status (`role`, `leaseOwner`, `leaseExpiresAt`, `observedPrimaryTunnelId`) and **returns without touching the shared Access application or public DNS CNAME**. It continues to reconcile its own tunnel + connector (warm).
- **Primary** — this site holds the lease. It writes the public DNS CNAME (pointing at its own `<tunnelId>.cfargotunnel.com`) and the Access application, and runs a renewal loop renewing at `leaseSeconds/2`.

**Promotion (auto):** a Standby that observes `now > leaseExpiresAt + jitter` attempts an acquire (`Create` if absent / `Update` of the expired record), then read-back verifies. Success → emit `PromotedToPrimary`, write Access + DNS, set `role=Primary`, renew at `leaseSeconds/2`.

**Promotion (manual):** the `cfzt.reid.ee/force-promote` annotation carries a caller-chosen **token** (timestamp, nonce, UUID — any non-empty value). A Standby attempts an acquire **regardless of expiry** only when the annotation token differs from `status.failover.lastForcePromoteToken`. On a successful acquire the controller records the token in `status.failover.lastForcePromoteToken` and emits `ForcePromoted`; it **does not modify the annotation**. A GitOps reconcile that re-applies the same token is therefore ignored (no replay). To force again, change the token. This is an emergency tool; the split-brain risk is the operator's responsibility.

**Demotion (returning primary):** a recovered former primary reconciles and reads the lease **before writing anything**. If `leaseOwner != my site-id`, it emits `DemotedToStandby`, sets `role=Standby`, and performs **no** Cloudflare writes for the shared hostname. It never auto-steals the lease back; the current holder keeps it until its own TTL lapses. Role decisions are driven by the **live lease read every reconcile**, never by the persisted `status.failover.role` alone — including in the finalizer (see `### Deletion`).

### Deletion

Failover deletion proves **current** ownership, not persisted role. On finalizer:

- Re-read the live lease. Delete the shared public CNAME + Access application **only if** `lease.Site == r.SiteID` at read time. A site whose persisted `status.failover.role` is stale `Primary` (a peer has since acquired) must **not** tear down the surface the peer is now serving.
- Always remove **this site's own** lease record if it owns it (idempotent); never delete a peer's lease.
- Otherwise (Standby, or peer-held) drop the finalizer without touching shared resources.

### Ownership tagging for failover Exposures

For a failover-enabled Exposure, the shared Access application and public DNS CNAME carry the **failover-group ID** as their `source-uid` instead of the per-CR `metadata.uid`. Both clusters compute the same group ID, so the mutation guard (`## Ownership and deletion semantics`) accepts the resource from either site. The `MatchesComment` / `MatchesTags` checks accept **either** a matching per-CR uid (non-failover Exposures) **or** a matching failover-group ID (failover Exposures).

### Split-brain bounding

The lease is best-effort, so a brief dual-writer window is possible; it is bounded by lease TTL + acquire jitter and made harmless by the data plane. Defaults: `leaseSeconds: 60`, `±5s` jitter. What keeps the exposure small:

- A genuinely partitioned primary loses its cloudflared connection to the Cloudflare edge within seconds of a real outage, so Cloudflare stops routing traffic to the old tunnel ID regardless of who holds the lease. This — not the lease — is the actual safety mechanism.
- There is one CNAME with one target at a time, so two sites can never serve divergent origins; the worst control-plane symptom is transient CNAME flapping, which the single-writer election + read-back + duplicate resolution converge away within a reconcile or two.

If a Primary reads the lease during renewal and finds `leaseOwner != my site-id` (it was promoted away while it believed itself Primary), it emits `SplitBrainDetected`, demotes to Standby, and stops writing.

### Operator behaviour matrix

| Role | Lease state | Public DNS CNAME | Access app | Own tunnel + connector | Lease record |
|---|---|---|---|---|---|
| Primary | held by self, unexpired | write/maintain | write/maintain | reconcile | renew at `leaseSeconds/2` |
| Standby | held by other, unexpired | leave alone | leave alone | reconcile (warm) | read only |
| Standby | expired (or force-promote token) | — (after acquire → Primary) | — (after acquire → Primary) | reconcile | acquire + read-back |
| any | duplicate records | resolve, then act on winner | resolve, then act on winner | reconcile | deterministic resolution |
| any | unresolvable ambiguity | leave alone | leave alone | reconcile | `Ready=False, Reason=LeaseConflict`, requeue |

### Non-goals

- **Active-active.** Exactly one Primary per group at a time.
- **Automatic primary auto-restoration.** A recovered primary stays Standby until it legitimately re-acquires an expired lease.
- **Cross-zone failover.** The lease and the hostname live in the same zone.
- **More than DNS-mediated coordination.** No Workers KV, no Cloudflare Load Balancer dependency, no external coordination service in this design (a future `spec.failover.mode: loadBalancer` could add LB-based failover for accounts on that plan without invalidating this lease design).

## Ownership and deletion semantics

The operator is conservative. It only mutates Cloudflare resources whose ownership it can prove — **except for ingress rules inside the tunnel-config doc**, which are computed-from-K8s every reconcile and never adopted (D11).

**Ownership rules** (see D9 for rationale on tunnels):

- **Tunnels: generated name + status ID.** Desired Cloudflare name is `<spec.tunnelName>-cfzt-<hash8(metadata.uid)>`. `CloudflareTunnel.status.tunnelId` is the authoritative ownership record once present. On reconcile: if `status.tunnelId` is set, `Get(id)` confirms the tunnel exists and the name is either desired or legacy `spec.tunnelName`; legacy names are renamed in place to the generated name, preserving tunnel ID and token. If `status.tunnelId` is unset, exactly one generated-name tunnel is recovered into status; legacy unsuffixed matches remain `Ready=False, Reason=ForeignTunnel`. Pre-existing generated-name collisions are unlikely but still treated conservatively if ambiguous. cloudflare-go/v4 v4.6.0 does not surface a tunnel `comment` field, so no CF-side tag is written for tunnels.
- **Access applications**: `tags` field carries `managed-by=cfzt-operator` and chunked `source-uid-<n>=...` values for `CloudflareExposure.uid`. Name = `<displayName-or-metadata.name>-cfzt`.
- **DNS records**: `comment` field carries `managed-by=cfzt-operator source-uid=<CloudflareExposure.uid>`.
- **Ingress rules: not tagged.** The entire doc is overwritten each reconcile from K8s desired state.
- **Access policies** (Slice 4, D24): `CloudflareAccessPolicy.status.policyId` is the authoritative ownership record. Before mutation or deletion, the controller verifies the tracked Cloudflare policy name still matches `<spec.policyName | metadata.name>-cfzt`; the suffix is always appended for collision protection. If status is empty and a same-name policy already exists, the operator recovers it only when the full Cloudflare policy shape exactly matches the CR; mismatches remain `Reason=ForeignPolicy`.
- **Tunnel routes** (Slice 5, D25): `CloudflareTunnelRoute.status.routeId` is the authoritative ownership record. CF-side `comment` carries compact ownership text `managed-by=cfzt source-uid=<CloudflareTunnelRoute.uid>`; the installed `cloudflare-go/v4` route API exposes `comment`, so Slice 5 requires this guard. Pre-existing active routes with the same target CIDR/VNet and no matching source-uid are NOT auto-adopted: `Ready=False, Reason=ForeignRoute`.

**Mutation rule.** Before update or delete of an Access app, DNS record, or TunnelRoute, the operator MUST verify the resource's `source-uid` matches a current local CR of the expected kind. Mismatch or missing tag → `Ready=False, Reason=ForeignResource` or route-specific `ForeignRoute`, no destructive action. Tunnels and Access policies follow the ID-based rules above instead.

**Hostname conflict rule.** If an Access app or DNS record already exists for a hostname and its `source-uid` does not match the reconciling Exposure → `Ready=False, Reason=HostnameConflict`. Do not touch the conflicting resource. Requeue after 30 seconds. (Ingress-rule conflicts inside the doc are resolved at build time: builder errors if two Exposures claim the same hostname, surfacing on both as `HostnameConflict`.)

**Tunnel deletion rule.** A `CloudflareTunnel` with ≥1 referencing `CloudflareExposure` cannot complete deletion. The tunnel finalizer holds, sets `Ready=False, Reason=BlockedByExposures`. A `CloudflareTunnel` with ≥1 referencing `CloudflareTunnelRoute` (Slice 5, D25) is similarly blocked with `Ready=False, Reason=BlockedByRoutes`. Both blocks must clear before deletion completes.

**Tunnel generated-name migration rule.** Existing statusful `CloudflareTunnel` CRs created before generated names are migrated automatically: the controller reads `status.tunnelId`, renames the same Cloudflare tunnel from legacy `spec.tunnelName` to `<spec.tunnelName>-cfzt-<hash8(metadata.uid)>`, and keeps the same tunnel ID. Existing DNS CNAMEs, Access applications, tunnel routes, tokens, and published hostname routes continue to point at the same tunnel ID. If `status.tunnelId` has already been lost and only a legacy unsuffixed tunnel remains, the operator does not auto-adopt; patch `status.tunnelId` manually or delete the remote tunnel and let the controller recreate it.

If an old statusful CR has `spec.tunnelName` longer than 106 characters, the generated name would exceed the 120-character budget. The controller must not rename or create in that case; it reports `Ready=False, Reason=TunnelNameInvalid`. Because `spec.tunnelName` is immutable, replace the `CloudflareTunnel` with a shorter base name and repoint dependants.

**Policy deletion rule** (Slice 4, D24). A `CloudflareAccessPolicy` with ≥1 referencing `CloudflareExposure` (via `spec.access.policyRef.name`) cannot complete deletion. The policy finalizer holds, sets `Ready=False, Reason=BlockedByExposures`. Mirrors the tunnel-deletion rule.

**Exposure source GC.** When `spec.sourceRef` is set and resolves in the same namespace, the Exposure controller adds an `ownerReference` from itself to the source resource. K8s GC cascades source deletion into Exposure deletion → finalizer fires.

**Drift policy.** Operator-tagged resource mutated outside the operator → reconcile rewrites to desired state. Untagged or foreign-tagged resource for the same hostname → operator leaves it alone, surfaces conflict.

## Status and conditions

Every CRD exposes exactly two conditions:

- `Ready` — desired state fully realised. `Status: True/False/Unknown`. `Reason` and `Message` carry detail.
- `Progressing` — at least one reconcile step is in flight or backing off.

`status.cloudflare.*` carries CF resource IDs and route hashes. `CloudflareTunnel.status.ingressDocHash` records the last desired tunnel-config document hash so no-op reconciles can skip redundant writes. IDs are authoritative for ownership in the operator's local view; CF-side `source-uid` tags are the defence-in-depth check before mutation.

`Reason` values (non-exhaustive):

- `CredentialsMissing`
- `TunnelCreating`, `TunnelNameInvalid`, `TokenFetchFailed`, `WorkloadNotReady`
- `OriginInvalid`, `NetworkInvalid`, `HostnameConflict`, `ForeignResource`, `ForeignTunnel`, `ForeignPolicy`, `ForeignRoute`
- `AccessAppPending`, `PolicyNotFound`, `PolicyNotReady`, `DNSWriteFailed`, `RouteWriteFailed`
- `TunnelNotReady`
- `BlockedByExposures`, `BlockedByRoutes`
- `Standby`, `LeaseConflict`, `FailoverRequiresManagedDNS`, `FailoverRequiresDistinctSiteID`, `FailoverGroupConflict` (Slice 6, D26)
- `UnsupportedDrift`, `Reconciled`

## Credentials

Cloudflare credentials live in a single Kubernetes Secret per `CloudflareTunnel`, referenced by `spec.credentialsSecretRef`. Two keys (defaults `accountId` and `apiToken`, override via `keys`).

Secret MUST live in the same namespace as the cloudflared workload (default `cfzt-system`). Enforced by API shape: `credentialsSecretRef` names only the Secret, and the namespace is always `spec.cloudflared.namespace`.

API token MVP scopes:

- `Account:Cloudflare Tunnel:Edit`
- `Access: Apps and Policies:Edit`
- `Zone:Zone:Read` on every zone covered by managed hostnames — **only** when any referenced `CloudflareTunnel` has `dns.manage: true`.
- `Zone:DNS:Edit` on every zone covered by managed hostnames — **only** when any referenced `CloudflareTunnel` has `dns.manage: true`.

DR failover (Slice 6, D26) requires `dns.manage: true`; its TXT lease record lives in the same managed zone and reuses the `Zone:DNS:Edit` / `Zone:Zone:Read` scopes above — no additional token scope.

## RBAC (operator ServiceAccount)

| API group | Resource | Verbs | Scope |
|---|---|---|---|
| `cfzt.reid.ee` | `cloudflaretunnels`, `cloudflareexposures` | `get,list,watch,create,update,patch,delete` | cluster |
| `cfzt.reid.ee` | `cloudflaretunnels/status`, `cloudflareexposures/status` | `get,update,patch` | cluster |
| `cfzt.reid.ee` | `cloudflaretunnels/finalizers`, `cloudflareexposures/finalizers` | `update` | cluster |
| `cfzt.reid.ee` | `cloudflareaccesspolicies` | `get,list,watch,create,update,patch,delete` | cluster (Slice 4) |
| `cfzt.reid.ee` | `cloudflareaccesspolicies/status` | `get,update,patch` | cluster (Slice 4) |
| `cfzt.reid.ee` | `cloudflareaccesspolicies/finalizers` | `update` | cluster (Slice 4) |
| `cfzt.reid.ee` | `cloudflaretunnelroutes` | `get,list,watch,create,update,patch,delete` | cluster (Slice 5) |
| `cfzt.reid.ee` | `cloudflaretunnelroutes/status` | `get,update,patch` | cluster (Slice 5) |
| `cfzt.reid.ee` | `cloudflaretunnelroutes/finalizers` | `update` | cluster (Slice 5) |
| `""` | `secrets` | `get,list,watch` | cluster (read credentials Secrets + own token Secrets) |
| `""` | `secrets` | `create,update,patch,delete` | cluster — **operator contract**: only writes Secrets whose name matches `<CloudflareTunnel.metadata.name>-token` in `cloudflared.namespace`. Audit trail via Events. (K8s RBAC cannot pattern-match `resourceNames` so contract is enforced in code, not RBAC.) |
| `apps` | `daemonsets` | `get,list,watch,create,update,patch,delete` | cluster — operator contract: only writes DaemonSets named `cloudflared-<CloudflareTunnel.metadata.name>` in `cloudflared.namespace`. |
| `""` | `services` | `get,list,watch` | cluster (Slice 3) |
| `gateway.networking.k8s.io` | `httproutes` | `get,list,watch` | cluster (Slice 3, conditional on CRD presence) |
| `coordination.k8s.io` | `leases` | `get,list,watch,create,update,patch,delete` | namespaced (leader election in operator namespace) |
| `""` | `events` | `create,patch` | cluster |
| `apiextensions.k8s.io` | `customresourcedefinitions` | `get,list,watch` | cluster (HTTPRoute CRD detection at startup) |

DR failover (Slice 6, D26) adds **no** Kubernetes RBAC rows: the lease is a Cloudflare DNS record (governed by the Cloudflare API token scope, not K8s RBAC), and `--site-id` is a process flag. The existing `cloudflareexposures` / `cloudflareexposures/status` rows cover the new `spec.failover` / `status.failover` fields.

## DNS management

When `CloudflareTunnel.spec.dns.manage: true` (default), operator creates a proxied CNAME for `spec.hostname` → `<tunnelId>.cfargotunnel.com` for each Exposure. Records tagged per ownership rules.

**Zone resolution.** Operator lists zones the API token can see at startup and on cache miss, then matches the longest zone-name suffix of `spec.hostname`. Tokens used with managed DNS need zone read access for those zones. No PSL parsing.

**External-dns coexistence.** Operator never emits external-dns annotations. With `dns.manage: true`, users running external-dns on the same zone with overlapping hostnames must scope external-dns to other zones or filter records out. With `dns.manage: false`, operator creates no records and external-dns (or any other tool) is free to manage them. `CloudflareTunnel.status.dnsMode` reports `"managed"` or `"external"`.

## Origin resolution

MVP: `spec.origin` is always explicit and used verbatim:

```text
<protocol>://<host>:<port>
```

Origin host may be in-cluster (`<service>.<namespace>.svc.cluster.local`), LAN (D16), or public IP — anything reachable from cloudflared pods.

Slice 3, when `spec.sourceRef` is set:

- For `Service` sources, missing `origin.host` defaults to `<service>.<namespace>.svc.cluster.local`. Missing `origin.port` defaults to the source Service's single port (error if zero or multiple ports).
- For `HTTPRoute` sources, missing `spec.hostname` defaults to the HTTPRoute's single `spec.hostnames` entry (error if zero or multiple). Origin is not auto-derived from parentRefs in MVP — `spec.origin` remains required for HTTPRoute sources.

## Reconcile idempotency

Every reconcile must be safe to run repeatedly. Pattern:

1. Observe Kubernetes desired state.
2. Observe Cloudflare actual state.
3. Compute desired Cloudflare state.
4. Verify ownership tags before any mutation of persistent CF resources (not applicable to tunnel-config doc per D11).
5. Create missing resources.
6. Update differing resources.
7. Delete only inside finalizers for owned resources.
8. Write status.
9. Requeue only when necessary (after retryable error, or periodic resync for drift detection).

The Cloudflare client wrapper enforces a per-token rate limit (single shared token bucket) and exponential backoff on `429` and `5xx`.

## Cloudflare SDK method mapping

Reference `cloudflare-go/v4` types and methods, not raw URLs. Use the [Cloudflare MCP server](https://github.com/cloudflare/mcp-server-cloudflare) to look up exact method signatures during implementation. Expected mapping (verify against current SDK at code time):

| Operation | SDK path |
|---|---|
| List tunnels | `client.ZeroTrust.Tunnels.List(ctx, ...)` |
| Create tunnel | `client.ZeroTrust.Tunnels.New(ctx, ...)` |
| Get tunnel | `client.ZeroTrust.Tunnels.Get(ctx, id, ...)` |
| Delete tunnel | `client.ZeroTrust.Tunnels.Delete(ctx, id, ...)` |
| Get tunnel token | `client.ZeroTrust.Tunnels.Token.Get(ctx, id, ...)` |
| Get tunnel config | `client.ZeroTrust.Tunnels.Configurations.Get(ctx, id, ...)` |
| Put tunnel config | `client.ZeroTrust.Tunnels.Configurations.Update(ctx, id, ...)` |
| List zones | `client.Zones.List(ctx, ...)` |
| Create DNS record | `client.DNS.Records.New(ctx, ...)` |
| Update DNS record | `client.DNS.Records.Update(ctx, id, ...)` |
| Delete DNS record | `client.DNS.Records.Delete(ctx, id, ...)` |
| List Access apps | `client.ZeroTrust.Access.Applications.List(ctx, ...)` |
| Create Access app | `client.ZeroTrust.Access.Applications.New(ctx, ...)` |
| Bind policy to app | `client.ZeroTrust.Access.Applications.Policies.Update(ctx, app, policy, ...)` |
| List Access policies (account-level) | `client.ZeroTrust.Access.Policies.List(ctx, ...)` (Slice 4) |
| Get Access policy | `client.ZeroTrust.Access.Policies.Get(ctx, id, ...)` (Slice 4) |
| Create Access policy | `client.ZeroTrust.Access.Policies.New(ctx, ...)` (Slice 4) |
| Update Access policy | `client.ZeroTrust.Access.Policies.Update(ctx, id, ...)` (Slice 4) |
| Delete Access policy | `client.ZeroTrust.Access.Policies.Delete(ctx, id, ...)` (Slice 4) |
| List tunnel routes | `client.ZeroTrust.Networks.Routes.List(ctx, ...)` (Slice 5) |
| Create tunnel route | `client.ZeroTrust.Networks.Routes.New(ctx, ...)` (Slice 5) |
| Get tunnel route | `client.ZeroTrust.Networks.Routes.Get(ctx, id, ...)` (Slice 5) |
| Edit tunnel route | `client.ZeroTrust.Networks.Routes.Edit(ctx, id, ...)` (Slice 5) |
| Delete tunnel route | `client.ZeroTrust.Networks.Routes.Delete(ctx, id, ...)` (Slice 5) |

For routes, use the non-deprecated route-ID endpoints above, not the deprecated CIDR-path `Routes.Networks.*` endpoints. `NetworkRouteListParams` does not have an exact `network` filter; use `network_subset` / `network_superset` plus in-code canonical exact matching, `tun_types=cfd_tunnel`, `is_deleted=false`, optional `tunnel_id`, and optional `virtual_network_id` only when `spec.virtualNetworkId` is set.

Exact field names and pagination shape: confirm via MCP at implementation time. Wrap all calls behind the `internal/cloudflare` interface so the fake implementation in tests does not depend on the SDK shape.

## Distribution

- Container image: `ghcr.io/andrewreid/cfzt-operator:<version>`. Versioned by git tag (`v0.1.0`, ...).
- Helm chart: published as OCI artifact `oci://ghcr.io/andrewreid/charts/cfzt-operator` (D14, D17).
- Image and chart versions move together.

### Helm chart layout (D17)

```
charts/cfzt-operator/
  Chart.yaml
  values.yaml
  crds/
    cloudflaretunnel.yaml          # copied from config/crd by `make manifests` + script
    cloudflareexposure.yaml
    cloudflareaccesspolicy.yaml
  templates/
    deployment.yaml
    serviceaccount.yaml
    clusterrole.yaml
    clusterrolebinding.yaml
    role-leader-election.yaml      # namespaced lease access
    rolebinding-leader-election.yaml
    NOTES.txt
```

`values.yaml` exposes:

```yaml
image:
  repository: ghcr.io/andrewreid/cfzt-operator
  tag: ""                          # defaults to .Chart.AppVersion
  pullPolicy: IfNotPresent
replicaCount: 2
resources: {}
nodeSelector: {}
tolerations: []
affinity: {}
logLevel: info
namespace: cfzt-system             # operator namespace
```

CRDs in `crds/` are install-only per Helm 3 native behaviour. Matches D15 delete-and-recreate upgrade policy.

## CI / CD (D18)

GitHub Actions workflows live in `.github/workflows/`:

- `ci.yaml` — on `push` and `pull_request`:
  - `go vet ./...`
  - `make lint` (builds the custom golangci-lint binary with module plugins, clears its cache, then runs it)
  - `make manifests generate` and `make helm-sync-crds`, then `git diff --exit-code` (fail if generated drift)
  - `make test` (unit + envtest via `setup-envtest`)
  - `helm lint charts/cfzt-operator`
- `live-smoke.yaml` — manually triggered live Cloudflare smoke against the current checkout and local chart.
- `release.yaml` — manually triggered with a semver input:
  - Run lint, generated-drift, unit/envtest, Helm lint, chart smoke, and live Cloudflare smoke before publishing.
  - Build multi-arch image, push to `ghcr.io/andrewreid/cfzt-operator:<tag>` and `:latest` (latest only on non-prerelease releases).
  - Package chart, set `version` and `appVersion` from the requested version, push to `oci://ghcr.io/andrewreid/charts/cfzt-operator`.
  - Create the git tag and GitHub Release only after all gates pass.

The release workflow uses `GITHUB_TOKEN` with `packages: write`; CI and live-smoke workflows are read-only.

## Observability

- **Metrics** (Prometheus via controller-runtime registry):
  - `cfzt_reconcile_total{controller, result}`
  - `cfzt_reconcile_duration_seconds{controller}`
  - `cfzt_cloudflare_api_total{endpoint, status}`
  - `cfzt_cloudflare_api_duration_seconds{endpoint}`
  - `cfzt_resource_ready{kind, namespace}` (gauge; intentionally no `name` label — avoid cardinality blow-up)
  - `cfzt_failover_role{exposure_group, site_id, role}` (gauge 0/1; Slice 6, D26)
  - `cfzt_failover_lease_renew_total{exposure_group, result}` (Slice 6, D26)
  - `cfzt_failover_promotion_total{exposure_group, reason}` (`reason` ∈ `auto`, `force`; Slice 6, D26)
- **Logs** (logr/zap, structured). Every line carries `controller`; where relevant also `namespace`, `name`, `tunnelId`, `hostname`, and for failover Exposures `siteId`, `failoverGroup`, `role`. Log level from `--zap-log-level` (default `info`).
- **Events**: emit Kubernetes Events for state transitions and Cloudflare mutations: `CreatedTunnel`, `UpdatedTunnelConfig`, `DeletedTunnel`, `CreatedAccessApp`, `UpdatedAccessApp`, `DeletedAccessApp`, `CreatedDNSRecord`, `UpdatedDNSRecord`, `DeletedDNSRecord`, `CreatedAccessPolicy`, `UpdatedAccessPolicy`, `DeletedAccessPolicy`, `HostnameConflict`, `TokenRotated`, `BlockedByExposures`, `CreatedRoute`, `UpdatedRoute`, `DeletedRoute`, `ForeignRoute`, `BlockedByRoutes`, `PromotedToPrimary`, `DemotedToStandby`, `LeaseAcquired`, `LeaseRenewed`, `LeaseLost`, `LeaseConflict`, `SplitBrainDetected`, `ForcePromoted` (the last eight: Slice 6, D26).

## Suggested package layout

```text
api/v1alpha1/
  cloudflaretunnel_types.go
  cloudflareexposure_types.go
  cloudflareaccesspolicy_types.go  # Slice 4 (D24)
  cloudflaretunnelroute_types.go   # Slice 5 (D25)
  groupversion_info.go
  zz_generated_deepcopy.go         # generated

internal/controller/
  cloudflaretunnel_controller.go
  cloudflareexposure_controller.go
  cloudflareaccesspolicy_controller.go  # Slice 4 (D24)
  cloudflaretunnelroute_controller.go   # Slice 5 (D25)
  accesspolicy_hash.go
  base.go
  conditions.go
  httproute_discovery.go
  indexes.go
  mapper.go

internal/tunnelconfig/
  builder.go            # builds desired ingress[] from list of Exposures

internal/cloudflare/
  client.go             # interface wrapping cloudflare-go/v4
  real.go               # SDK implementation
  fake.go               # in-memory fake for tests
  tunnels.go
  configurations.go
  access_applications.go
  access_tags.go        # Access app tag helper for Cloudflare's tag length limit
  access_policies.go    # Slice 4 (D24) — reusable account-level policies
                        # (distinct from access_applications.go which binds existing policy UUIDs to apps)
  routes.go             # Slice 5 (D25) — tunnel private-network routes
  dns.go
  zones.go              # zone cache + longest-suffix resolution

internal/origin/
  service.go            # Slice 3
  httproute.go          # Slice 3

internal/naming/
  names.go              # token Secret + DaemonSet + Access app naming

internal/ownership/
  owner.go              # ownership facade for comments and Access tags
  comment.go            # DNS and TunnelRoute source-uid comments
  accesstag.go          # chunked Access app source-uid tags

internal/workload/
  daemonset.go          # cloudflared DaemonSet construction
  token_secret.go

internal/dr/            # Slice 6 (D26) — DR failover
  lease.go              # TXT lease parse / serialise
  jitter.go             # symmetric acquire jitter
  resolve.go            # deterministic duplicate-lease resolution (best-effort, fail-closed)
  role.go               # role state machine (Unknown/Standby/Primary)

cmd/
  main.go               # kubebuilder-generated manager entrypoint; --site-id flag (Slice 6, D26)

config/                  # kustomize, generated by kubebuilder
  default/
  rbac/
  crd/

charts/
  cfzt-operator/        # D17

.github/workflows/
  ci.yaml
  live-smoke.yaml
  release.yaml

test/live/
  cloudflare_smoke_test.go
  harness.go

docs/
  plan.md               # implementation plan (per-slice, lives in repo)
```

## Naming conventions

- `CloudflareExposure` `metadata.name`: user-chosen. Defaults to source name when `sourceRef` set; no automatic mangling.
- Access app name: `<spec.displayName | metadata.name>-cfzt` (suffix avoids collision with hand-created apps).
- Token Secret name: `<CloudflareTunnel.metadata.name>-token`. Key: `token`.
- DaemonSet name: `cloudflared-<CloudflareTunnel.metadata.name>`.
- DNS record `comment`: `managed-by=cfzt-operator source-uid=<exposure-uid>`.
- Access app `tags`: `managed-by=cfzt-operator`, chunked `source-uid-<n>=...` values.
- Tunnel Cloudflare name: `<spec.tunnelName>-cfzt-<hash8(metadata.uid)>`; ownership is then tracked via `CloudflareTunnel.status.tunnelId` (no CF-side comment/tag — see D9).
- Access policy name in Cloudflare (Slice 4, D24): `<spec.policyName | metadata.name>-cfzt`; the suffix is always appended.
- Access policy ownership (Slice 4): tracked via `CloudflareAccessPolicy.status.policyId`; the controller verifies the tracked Cloudflare policy name before mutation or deletion.
- `CloudflareTunnelRoute` `metadata.name`: user-chosen. No CF-side name (routes are identified by CIDR + VNet + ID).
- Route `comment` in Cloudflare (Slice 5, D25): `managed-by=cfzt source-uid=<route-cr-uid>` plus optional user `spec.comment` text after a ` | ` separator. User comment is capped at 34 chars so the full CF comment fits Cloudflare's 100-char route limit.
- Tunnel route ownership: tracked via `CloudflareTunnelRoute.status.routeId`; CF-side `comment` source-uid is the required defence-in-depth mutation guard.
- Finalizer string: `cfzt.reid.ee/finalizer` on all CRDs (D21, plus `CloudflareAccessPolicy` in Slice 4).
- Tunnel ingress rule ordering: sorted by hostname (lexicographic). Catch-all `service: http_status:404` always last.

## Cloudflared pod spec

Reference shape (verify against the Cloudflare Kubernetes exemplar at implementation time — they may have updated probe paths / args):

```yaml
spec:
  hostNetwork: <from CloudflareTunnel.spec.cloudflared.hostNetwork>
  dnsPolicy: ClusterFirstWithHostNet   # when hostNetwork: true; otherwise ClusterFirst
  containers:
    - name: cloudflared
      image: <pinned default or spec override>
      args:
        - tunnel
        - --no-autoupdate
        - --metrics
        - 0.0.0.0:2000
        - run
      env:
        - name: TUNNEL_TOKEN
          valueFrom:
            secretKeyRef:
              name: <token-secret>
              key: token
      ports:
        - name: metrics
          containerPort: 2000
      livenessProbe:
        httpGet: { path: /ready, port: 2000 }
        initialDelaySeconds: 10
        periodSeconds: 10
      readinessProbe:
        httpGet: { path: /ready, port: 2000 }
        periodSeconds: 5
      resources: <from spec>
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        readOnlyRootFilesystem: true
        allowPrivilegeEscalation: false
        capabilities: { drop: ["ALL"] }
  template:
    metadata:
      annotations:
        cfzt.reid.ee/token-checksum: <sha256 of token>
```

DaemonSet `updateStrategy: RollingUpdate` with `maxUnavailable: 1`.

## Implementation slices

The implementer ships in slices. Each slice has a measurable definition of done.

### Slice 1 — Tunnel and connector

**Outcome**: `CloudflareTunnel` CR creates the tunnel, stores token, runs cloudflared DaemonSet. Pre-existing name-colliding tunnels are NOT auto-adopted (D9).

**Steps**:

1. Kubebuilder scaffold (module `github.com/andrewreid/cfzt-operator`, group `cfzt.reid.ee/v1alpha1`).
2. `CloudflareTunnel` types + CRD generation + Helm chart skeleton.
3. `internal/cloudflare` interface + real + fake implementations (tunnels, token).
4. Tunnel controller: credential resolution, generated-name + ID-based ownership reconciliation (see `## Ownership and deletion semantics`), token fetch, Secret upsert, DaemonSet upsert.
5. Token-rotation handling via pod-template checksum annotation.
6. Finalizer (no-op while no Exposures exist).
7. envtest coverage.

**Definition of done**:

- `kubectl apply` of a `CloudflareTunnel` against an empty Cloudflare account creates the tunnel, populates `status.tunnelId`, creates Secret `<name>-token`, creates DaemonSet `cloudflared-<name>`.
- `Ready=True` once DaemonSet has ≥1 ready pod.
- Reapply is a no-op.
- A pre-existing generated-name tunnel is recovered into `status.tunnelId`; a pre-existing legacy `spec.tunnelName` tunnel with no local status remains `Ready=False, Reason=ForeignTunnel`.
- envtest tests pass: `TestTunnelCreate`, `TestTunnelGeneratedNameOrphanRecovery`, `TestTunnelStatusfulLegacyRenamed`, `TestTunnelForeignTunnelRefuses`, `TestTunnelTokenRotation`, `TestTunnelFinalizerNoop`, `TestTunnelConditionsTransition`.
- `ci.yaml` green.
- `helm install` against a fresh cluster works.

### Slice 2 — Exposure: routes, DNS, Access

**Outcome**: `CloudflareExposure` CR drives ingress doc, DNS CNAME, Access app, policy binding. End-to-end traffic with auth.

**Steps**:

1. `CloudflareExposure` types + CRD.
2. `internal/tunnelconfig/` builder + reconciler. Tunnel controller now lists referencing Exposures and writes ingress doc.
3. Exposure controller: validation, Access app + policy binding, DNS CNAME, enqueue tunnel, read tunnel status for route hash.
4. D20 cross-controller watches.
5. Ownership tagging on every persistent CF write; ownership verification before every mutation.
6. Hostname-conflict detection (builder + Access + DNS).
7. Finalizer: remove DNS, remove Access app, enqueue tunnel for ingress-doc update.
8. Tunnel finalizer now blocks on referencing Exposures.
9. envtest coverage.

**Definition of done**:

- `kubectl apply` of a `CloudflareExposure` results in: ingress rule in tunnel-config doc, proxied DNS CNAME, Access app with policy bound; `Ready=True`.
- External-origin example (D16) works against a host reachable from cloudflared pods.
- `kubectl delete` of the Exposure removes all three CF resources and the ingress rule.
- `kubectl delete` of the Tunnel while Exposures exist is blocked with `Reason=BlockedByExposures`.
- envtest tests pass: `TestExposureCreate`, `TestExposureDNSManagedOff`, `TestExposureAccessDisabled`, `TestExposureHostnameConflict`, `TestExposureForeignResource`, `TestExposureFinalizer`, `TestTunnelBlockedByExposures`, `TestTunnelConfigBuilderDeterministic`.
- Manual verification: `curl https://<hostname>` returns Access challenge; after auth, reaches origin.

### Slice 3 — sourceRef derivation

**Outcome**: `sourceRef` referencing Service or HTTPRoute derives missing origin/hostname fields and wires ownerReference.

**Steps**:

1. Service-source origin defaulting (`internal/origin/service.go`).
2. HTTPRoute-source hostname defaulting (`internal/origin/httproute.go`); origin remains explicit.
3. ownerReference wiring (same-namespace only).
4. HTTPRoute controller enabled only when `gateway.networking.k8s.io` CRD discovered at startup.
5. CRD validation relaxed: `origin` no longer required when `sourceRef.kind == Service` with a single-port Service.

**Definition of done**:

- Exposure with `sourceRef: Service` and no origin fields reconciles using derived `<svc>.<ns>.svc.cluster.local:<port>`.
- Deleting the Service garbage-collects the Exposure (verified by `kubectl delete service` → Exposure disappears within GC interval, CF state cleaned).
- HTTPRoute controller absent → operator boots clean with a log line "HTTPRoute CRD not found, controller disabled".
- envtest tests pass: `TestSourceRefServiceSinglePort`, `TestSourceRefServiceMultiPortRejected`, `TestSourceRefDeletionCascades`, `TestHTTPRouteHostnameDerivation`, `TestHTTPRouteCRDAbsentBootsClean`.

### Slice 4 — Managed Access policies

**Outcome**: `CloudflareAccessPolicy` CR creates and maintains a reusable account-level Cloudflare Access policy. `CloudflareExposure.spec.access.policyRef.name` binds an Exposure to a managed policy as an alternative to `uuid`.

**Steps**:

1. `CloudflareAccessPolicy` types + CRD generation + CEL validation (structured rule subset, discriminated-union rule items).
2. `internal/cloudflare/access_policies.go` interface + real + fake implementations (List, Get, Create, Update, Delete). Verify exact SDK paths via Cloudflare MCP.
3. Policy controller: credential resolution, ID-based ownership reconciliation (mirrors D9 tunnel pattern; `Reason=ForeignPolicy` on collision), rule-hash drift detection, finalizer with `BlockedByExposures`.
4. Cross-watch wiring: Policy ↔ Exposure (see additions to `## Tunnel configuration concurrency`).
5. Exposure controller: resolve `policyRef.name` → `status.policyId`, bind via `Applications.Policies.Update`. `Reason=PolicyNotReady` when target Policy CR is not yet `Ready=True`.
6. CRD validation update on Exposure: exactly-one-of {uuid, name} when access enabled.
7. envtest coverage.

**Definition of done**:

- `kubectl apply` of a `CloudflareAccessPolicy` creates a CF Access policy, populates `status.policyId`, sets `Ready=True`.
- A pre-existing CF policy with name collision and no local ID record → `Ready=False, Reason=ForeignPolicy`, no mutation of the foreign policy.
- An Exposure with `policyRef.name` binds the policy ID once the Policy CR becomes ready.
- `kubectl delete` of a Policy CR with referencing Exposures is blocked (`Reason=BlockedByExposures`); succeeds once references are removed.
- Editing `spec.rules` on a Policy CR rewrites the CF policy and propagates a reconcile to all referencing Exposures.
- envtest tests pass: `TestAccessPolicyCreate`, `TestAccessPolicyForeignRefuses`, `TestAccessPolicyRulesDrift`, `TestAccessPolicyFinalizerBlockedByExposures`, `TestAccessPolicyFinalizerUnblocks`, `TestExposurePolicyRefName`, `TestExposurePolicyRefNamePolicyNotReady`, `TestExposurePolicyRefOneOfValidation`.
- Manual: dashboard shows the policy was created; `kubectl edit cfap` rolls the rules in CF within one reconcile.

### Slice 5 — Tunnel private network routes

**Outcome**: `CloudflareTunnelRoute` CR creates and maintains a Cloudflare Tunnel private-network route (CIDR → tunnel binding). Tunnel deletion is blocked while routes reference the tunnel.

**Steps**:

1. `CloudflareTunnelRoute` types + CRD generation + CEL validation (coarse IPv4 / IPv6 CIDR shape, controller-side `net/netip.ParsePrefix` validation for both families, general UUID VNet with explicit empty as unset, compact comment length, immutable tunnelRef, reject clearing VNet after set).
2. `internal/cloudflare/routes.go` interface + real + fake implementations (List, New, Get, Edit, Delete). Verify exact SDK paths via Cloudflare MCP; use route-ID endpoints and route `comment`, not deprecated CIDR-path endpoints.
3. Route controller: credential resolution via Tunnel CR, tunnel-ID gate (`Reason=TunnelNotReady` until referenced Tunnel has `status.tunnelId`), ID-record reconcile (mirrors D9 + D24), compact comment-tag ownership, preflighted drift correction (`Edit`), finalizer cleanup with comment source-uid verification.
4. Cross-watch: Route controller `.Watches(&CloudflareTunnel{}, …)` to retry when the referenced tunnel gets `status.tunnelId` or is deleted. Tunnel controller `.Watches(&CloudflareTunnelRoute{}, …)` to recompute its `BlockedByRoutes` finalizer state.
5. Tunnel finalizer extension: block on referencing Routes with `Reason=BlockedByRoutes`, parallel to existing `BlockedByExposures`.
6. RBAC markers + Helm CRD sync + ClusterRole rows.
7. Live Cloudflare smoke coverage: extend `test/live/cloudflare_smoke_test.go` (`TestCloudflareLifecycle`) with a route create / idempotent reconcile / foreign-route conflict / cleanup phase. Packet routing through the route is NOT validated (no WARP client in kind); CF-side lifecycle is sufficient.
8. envtest coverage.

**Definition of done**:

- `kubectl apply` of a `CloudflareTunnelRoute` against a tunnel with `status.tunnelId` creates the CF route, populates `status.routeId`, sets `Ready=True`.
- `kubectl delete cloudflaretunnelroute <name>` removes the CF route and finalizer.
- A pre-existing CF route with the same target CIDR/VNet and no local ID record or with a mismatching `source-uid` comment causes `Ready=False, Reason=ForeignRoute` with no mutation of the foreign route.
- `kubectl delete cloudflaretunnel <name>` while a `CloudflareTunnelRoute` references it is blocked with `Reason=BlockedByRoutes`.
- Drift: editing `spec.network`, non-empty `spec.virtualNetworkId`, or `spec.comment` preflights target CIDR/VNet conflicts, then rewrites the CF route within one reconcile when no foreign route blocks the change. Clearing a previously set `spec.virtualNetworkId` is rejected; delete + recreate to return to the account default VNet.
- envtest tests pass: `TestCloudflareTunnelRouteCRDValidation`, `TestRouteCreate`, `TestRouteCreateIPv6`, `TestRouteForeignRefuses`, `TestRouteDriftCorrection`, `TestRouteFinalizerDeletes`, `TestRouteFinalizerLeavesForeign`, `TestRouteTunnelNotReady`, `TestTunnelBlockedByRoutes`, `TestRouteConditionsTransition`.
- Live smoke (`hack/live-cloudflare-local.sh lifecycle`) creates, re-reconciles, refuses a foreign-CIDR collision, and cleans up the `CloudflareTunnelRoute` against real Cloudflare.
- `ci.yaml` green; `helm lint` clean; manual: dashboard Networks → Routes shows the route with compact `managed-by=cfzt` source-uid in the comment.

### Slice 6 — DR failover

**Outcome**: `CloudflareExposure.spec.failover` lets the same Exposure, applied to two clusters (each with its own `--site-id` and its own `CloudflareTunnel`), cooperate over one hostname via a Cloudflare DNS TXT lease. Exactly one cluster is Primary and serves traffic; the standby auto-promotes on lease expiry; a recovered former primary stands down. See `## DR failover` and D26.

**Steps**:

1. `--site-id` flag on the manager (mandatory, non-empty validated at boot); Helm `values.site.id` with a sane default; plumb the site ID into the Exposure controller. (`cmd/main.go`, chart Deployment template + `values.yaml`.)
2. `CloudflareExposure` `spec.failover { group, leaseSeconds }` + `status.failover { role, siteId, leaseOwner, leaseExpiresAt, leaseRenewedAt, lastRoleTransitionAt, observedPrimaryTunnelId }` types; CEL/validation markers; `Role` printcolumn. Regenerate manifests + deepcopy; sync Helm CRDs.
3. `internal/cloudflare` DNS interface: `Create` / `Update` support TXT records (not only CNAME). No conditional-write or CAS primitive — the API has none; the fake models real semantics (`Create` is non-atomic and can yield duplicate TXT records). Coordination lives in the controller + `internal/dr`, not the client.
4. `internal/dr/` package: lease parse/serialise (`v=1 site=… tunnel=… exp=… renewed=…`), symmetric acquire jitter, deterministic duplicate-lease resolution (`Resolve`), role state machine (renew at `leaseSeconds/2`). No CAS retry loop.
5. `internal/ownership`: `FromFailoverGroup(groupID)` constructor; `MatchesComment` / `MatchesTags` accept either a per-CR uid OR a failover-group ID.
6. `internal/naming`: `FailoverLeaseTXTName(groupID, zone)` helper.
7. Exposure controller: reject `spec.failover` when `--site-id` is the chart default (`FailoverRequiresDistinctSiteID`) and reject same-cluster duplicate `group` (`FailoverGroupConflict`, cluster-wide); read lease + determine role at the top of reconcile (before Access/DNS); on `>1` group-owned record resolve deterministically; Standby early-returns after writing `status.failover`; Primary writes Access + DNS with the group-ID `source-uid`, renews the lease, and read-back verifies; promotion (auto on expiry / `cfzt.reid.ee/force-promote` **token** vs `status.lastForcePromoteToken`, no annotation mutation); demotion + `SplitBrainDetected`; `FailoverRequiresManagedDNS` on unmanaged-DNS tunnels; deletion proves **live** lease ownership before tearing down shared resources; emit the Slice 6 events; update the Slice 6 metrics (labelled with `site_id`).
8. envtest coverage using a two-manager-in-process pattern (two managers with distinct `--site-id`, one shared fake CF client), including duplicate-lease resolution, force-promote token replay, group conflict, default-site rejection, and delete-ownership.
9. Live smoke extension: a failover lifecycle scenario in `test/live/` (peer simulated via the CF API when only one operator runs in the local cluster).

**Definition of done**:

- Two clusters apply an identical `CloudflareExposure` with matching `spec.failover.group` and distinct `--site-id` → exactly one reports `status.failover.role == Primary`; the other reports `Standby` with `leaseOwner` set to the primary's site ID.
- Stopping the primary's lease renewer for `leaseSeconds + jitter` → the standby auto-promotes; the CF Access app `source-uid` remains the failover-group ID; the public DNS CNAME flips to the new tunnel ID; `PromotedToPrimary` / `LeaseAcquired` events emit.
- A returning former primary self-demotes (`DemotedToStandby`) on its first reconcile and performs no Cloudflare writes for the shared hostname.
- A new `cfzt.reid.ee/force-promote` **token** on a standby Exposure causes an immediate acquire regardless of expiry; the controller records the token in `status.failover.lastForcePromoteToken` and emits `ForcePromoted`. Re-applying the same token (GitOps) is a no-op; the annotation is never mutated by the controller.
- A failover Exposure on a `dns.manage: false` tunnel goes `Ready=False, Reason=FailoverRequiresManagedDNS` and writes no lease.
- A failover Exposure on a process running the chart-default `site.id` goes `Ready=False, Reason=FailoverRequiresDistinctSiteID` and writes no lease.
- Two same-cluster failover Exposures sharing a `group` both go `Ready=False, Reason=FailoverGroupConflict` with no Cloudflare write.
- Duplicate group-owned lease TXT records converge to one via deterministic resolution; a returning primary's deletion only tears down shared resources when it holds the live lease.
- Empty `--site-id` is a fatal manager start-up error.
- envtest tests pass: `TestFailoverLeaseAcquire`, `TestFailoverLeaseRenew`, `TestFailoverAutoPromoteOnExpiry`, `TestFailoverReturnedPrimaryStandsDown`, `TestFailoverDuplicateLeaseResolves`, `TestFailoverForcePromoteTokenReplayIgnored`, `TestFailoverGroupConflict`, `TestFailoverRequiresDistinctSiteID`, `TestFailoverDeleteRequiresLiveOwnership`, `TestFailoverOwnershipTagAcceptsGroupID`, `TestFailoverDNSManagedRequired`, `TestFailoverSiteIDMandatoryAtBoot`.
- Live smoke (`TestFailoverLifecycle`): one operator + a peer simulated via the CF API asserts one Primary, returning-primary self-demote on peer takeover, and auto-promote on peer-lease expiry.
- `ci.yaml` green; `helm lint` clean; manual: `dig TXT _cfzt-lease.<hash>.<zone> @1.1.1.1` shows the current lease owner and TTL.

### Post-MVP

Annotation→Exposure convenience controller, parentRefs-based Gateway origin auto-resolution, Ingress source, additional Access rule types (groups, IdP claims, certificate, mTLS, posture), WARP client-side routing for private networks.

## Non-goals for early implementation

The early implementation does not:

- Build a web UI.
- Replace `external-dns` broadly.
- Manage arbitrary Cloudflare DNS records (only proxied CNAMEs for managed hostnames).
- Manage arbitrary Zero Trust settings.
- Support every Access policy rule type. Slice 4 covers a structured subset (email, email_domain, ip, everyone, service_token, geo) across include/exclude/require groups. Other rule types (groups, IdP claims, certificate, mTLS, posture, country lists with negation, etc.) remain deferred.
- Implement multi-tenant semantics, or multi-cluster beyond active-passive DR (D26 — active-active and federation remain out).
- Depend on Helm operators, Ansible operators, or Crossplane.
- Ship a validating or conversion webhook in MVP.
