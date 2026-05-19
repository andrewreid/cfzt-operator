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
- Cloudflare Access policy binding (existing policies only in MVP).
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
| D3 | `CloudflareAccessPolicy` CRD is **out of MVP**. `CloudflareExposure.spec.access.policyRef.uuid` references an existing Cloudflare Access policy by UUID. Name lookup is not supported in MVP. |
| D4 | Cloudflared uses a **per-tunnel token**, retrieved by the operator and stored in an operator-managed Kubernetes Secret. No `cert.pem`. |
| D5 | The user interface is **CR-only**. `CloudflareExposure` is the workload-facing CR. There is no annotation controller in MVP. An annotation→Exposure convenience layer may be added post-MVP without changing the core. |
| D6 | `CloudflareTunnel` is **cluster-scoped**. The cloudflared workload it manages is namespaced (default namespace `cfzt-system`, override via `spec.cloudflared.namespace`). |
| D7 | Cloudflared is deployed as a **DaemonSet only** in MVP. Image is pinned in operator code to a specific version, overridable via `spec.cloudflared.image`. |
| D8 | Conditions on every CRD are **`Ready` and `Progressing` only**. Detail goes in `Reason`/`Message`. No granular `*Ready` condition set. |
| D9 | **Multi-cluster is out of scope.** No cluster-name flag. Accidental-collision safety is provided per-resource as follows: **Access applications and DNS records** are tagged with the source CR's UID (`managed-by=cfzt-operator source-uid=<uid>`) — operator refuses to mutate any tagged resource whose UID does not match a current local CR. **Cloudflare Tunnels** are tracked by tunnel ID in `CloudflareTunnel.status.tunnelId` — operator refuses to adopt a name-matched tunnel when no local ID record exists (`Reason=ForeignTunnel`). The SDK (`cloudflare-go/v4` v4.6.0) does not surface a tunnel `comment` field, so tunnel ownership relies on the local ID record rather than a CF-side tag; this is acceptable under D12 leader-election (single operator process per cluster). **Ingress rules inside the tunnel-config doc are NOT individually tagged** — see D11. |
| D10 | Origin is **always explicit** on `CloudflareExposure.spec.origin` in MVP. There is no `defaultGatewayOrigin` field on `CloudflareTunnel`. Origin host may point at anything reachable from the cloudflared pods (in-cluster Service DNS, LAN hostname, public IP). Auto-derivation from HTTPRoute parentRefs is deferred. |
| D11 | The tunnel-config doc is **single-writer per tunnel**. Exposure controllers do not call the configurations endpoint directly — they enqueue the owning tunnel, and the tunnel reconciler computes and writes the **full** ingress doc from all referencing Exposures. The doc is fully derived from K8s state every reconcile; the operator never adopts pre-existing rules and does not tag individual rules. **Last-write-wins.** Combined with D12 this is safe. |
| D12 | **Leader election is required ON.** Together with D11 this guarantees one writer per tunnel-config doc per cluster. |
| D13 | Cloudflare SDK: **`github.com/cloudflare/cloudflare-go/v4`** (auto-generated client). Wrapped behind an internal interface so a fake can be substituted for tests. Reference the [Cloudflare MCP server](https://github.com/cloudflare/mcp-server-cloudflare) for SDK method discovery during implementation. |
| D14 | Distribution: **Helm chart published as an OCI artifact to GHCR** (D17 for chart shape). Container image also on GHCR. Module path: `github.com/andrewreid/cfzt-operator`. |
| D15 | API version is `v1alpha1`. Breaking changes are permitted without a conversion webhook. **Upgrade strategy: delete and recreate CRs.** Users must export Exposure manifests, uninstall, install new version, reapply. No state migration. |
| D16 | **External (non-Kubernetes) origins are first-class.** A `CloudflareExposure` with no `sourceRef` and an explicit `origin` pointing at a LAN host, public IP, or any address reachable from the cloudflared pods is fully supported. Use case: keeping a Home Assistant / NAS / appliance tunnel in GitOps without making the device a Kubernetes Service. |
| D17 | **Helm chart shape**: hand-written under `charts/cfzt-operator/`. CRDs live in `charts/cfzt-operator/crds/` for Helm 3 native handling (install-only; upgrades do not modify CRDs — matches D15 delete-recreate policy). Templates cover Deployment, ServiceAccount, ClusterRole, ClusterRoleBinding, RBAC for leases. `values.yaml` exposes image repo/tag/pullPolicy, replicas, resources, leader-election toggle (default on), logLevel. CRDs are NOT installed by `helmify`-style generation — written by hand. |
| D18 | **CI in scope.** GitHub Actions workflows: `ci.yaml` (lint + unit + envtest on PR), `release.yaml` (tag → build + push image to GHCR + push Helm OCI chart to GHCR). |
| D19 | **Per-controller `MaxConcurrentReconciles`**: Tunnel controller = `1` (single-writer per process for the tunnel-config doc). Exposure controller = `1` in MVP, raise post-MVP if needed. |
| D20 | **Cross-controller wiring**: Tunnel controller watches Exposure (cluster-wide, map → tunnel). Exposure controller watches Tunnel (map → all Exposures referencing that tunnel) so status writes propagate. |
| D21 | **Finalizer string**: `cfzt.reid.ee/finalizer` on both CRDs. |
| D22 | **Minimum Kubernetes version: 1.27.** Required for stable CEL validation (`x-kubernetes-validations`) used by CRD schema (see `## CRD validation`). |
| D23 | **GitOps caveat for Helm CRDs**: D17 places CRDs in `charts/cfzt-operator/crds/` (Helm 3 native install-only behaviour). ArgoCD users who render the chart and apply manifests via Application sync will see CRDs *not* upgraded on chart upgrade — matches D15 delete-and-recreate policy. Flux users should set `install.crds: Create` and `upgrade.crds: CreateReplace` with care, again matching D15. Document this in chart `NOTES.txt`. |

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
- Creation of Cloudflare Tunnels. Adoption of pre-existing tunnels is NOT automatic — see D9 (`status.tunnelId` is the ownership record; name-collision without a local ID record → `Reason=ForeignTunnel`).
- Per-tunnel token retrieval and storage in a Kubernetes Secret.
- Managed cloudflared DaemonSet per `CloudflareTunnel`.
- Creation and update of Cloudflare published hostname routes via tunnel-config (D1, D11).
- Creation and update of Cloudflare DNS CNAMEs (D2, when enabled).
- Creation and update of Cloudflare Access applications.
- Binding of pre-existing Access policies to Access applications by UUID (D3).
- External (non-K8s) origins (D16).
- `Ready` and `Progressing` conditions on both CRDs (D8).
- Finalizers for owned Cloudflare resources.
- Helm OCI chart (D17) + CI/release pipeline (D18).

Deferred:

- Annotation-driven UX (D5, post-MVP convenience layer).
- `CloudflareAccessPolicy` CRD (D3).
- HTTPRoute and Service `sourceRef` derivation (Slice 3).
- Ingress source support.
- Private network CIDR routes.
- WARP routing.
- Device posture management.
- Gateway policy management.
- Full Cloudflare Gateway management.
- Multi-account support.
- Multi-cluster support (D9).
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
  tunnelName: homelab-rke2           # name of the tunnel inside Cloudflare
  dns:
    manage: true                     # D2 default
  cloudflared:
    namespace: cfzt-system           # default cfzt-system
    image: ghcr.io/cloudflare/cloudflared:2025.1.0   # operator pins a default
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
2. Reconcile tunnel identity (see D9 + `## Ownership and deletion semantics`): if `status.tunnelId` is set, `Get(id)` and verify name matches; if unset, `List(name=spec.tunnelName)` — zero hits → create; one or more hits → `Ready=False, Reason=ForeignTunnel`, no mutation.
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
      uuid: 0123abcd-4567-89ef-0123-456789abcdef
status:
  cloudflare:
    accessApplicationId: ""
    publicHostnameRouteHash: ""      # SHA of the rule the tunnel reconciler placed
    dnsRecordId: ""                  # only when D2 manage=true
  conditions: []
```

Kubebuilder markers:

- `+kubebuilder:resource:scope=Namespaced,shortName=cfe`
- `+kubebuilder:subresource:status`
- `+kubebuilder:printcolumn:name=Hostname,type=string,JSONPath=.spec.hostname`
- `+kubebuilder:printcolumn:name=Tunnel,type=string,JSONPath=.spec.tunnelRef.name`
- `+kubebuilder:printcolumn:name=Access,type=boolean,JSONPath=.spec.access.enabled`
- `+kubebuilder:printcolumn:name=Ready,type=string,JSONPath=.status.conditions[?(@.type=="Ready")].status`
- `+kubebuilder:printcolumn:name=Age,type=date,JSONPath=.metadata.creationTimestamp`

`CloudflareExposure` controller responsibilities:

1. Resolve the referenced `CloudflareTunnel` and read its credentials.
2. Validate origin and hostname.
3. Ensure the Access application exists for `spec.hostname` when `access.enabled: true`; bind the policy UUID (D3).
4. Ensure the proxied DNS CNAME exists for `spec.hostname` → `<tunnelId>.cfargotunnel.com` when the tunnel has `dns.manage: true`.
5. Enqueue the referenced `CloudflareTunnel` so the tunnel reconciler updates the ingress doc (D11).
6. Read back the route placement from the tunnel's `status.routes[]` and record `publicHostnameRouteHash` in this Exposure's status.
7. Set `Ready=True` once Access (if enabled), DNS (if managed), and the published route are all in their desired state.
8. On deletion, run a finalizer (`cfzt.reid.ee/finalizer`) that removes DNS record + Access app, then enqueues the tunnel for ingress-doc update.

`access.enabled: false` skips Access application creation; the hostname is reachable without auth.

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
- `spec.access.policyRef.uuid`: required when `access.enabled: true`, pattern UUID v4.

`CloudflareTunnel`:

- `spec.tunnelName`: required, minLength 1, maxLength 120.
- `spec.credentialsSecretRef.name`: required.
- `spec.credentialsSecretRef.keys.accountId`: optional, default `accountId`, maxLength 253.
- `spec.credentialsSecretRef.keys.apiToken`: optional, default `apiToken`, maxLength 253.
- Credentials Secret namespace is always `spec.cloudflared.namespace` (default `cfzt-system`); the API intentionally stores that namespace once.
- `spec.dns.manage`: bool, default `true`.
- `spec.cloudflared.image`: pattern `^[a-z0-9./-]+(:[a-zA-Z0-9._-]+)?$`, not allowed to end `:latest`.

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

## Ownership and deletion semantics

The operator is conservative. It only mutates Cloudflare resources whose ownership it can prove — **except for ingress rules inside the tunnel-config doc**, which are computed-from-K8s every reconcile and never adopted (D11).

**Ownership rules** (see D9 for rationale on tunnels):

- **Tunnels: tracked by ID, not by tag.** `CloudflareTunnel.status.tunnelId` is the authoritative ownership record. On reconcile: if `status.tunnelId` is set, `Get(id)` confirms the tunnel exists and `Name` matches `spec.tunnelName`; if `Get` returns 404, the local record is invalidated and the controller creates a new tunnel. If `status.tunnelId` is unset, the controller `List`s by `spec.tunnelName` — zero hits → create; one or more hits → `Ready=False, Reason=ForeignTunnel`, no mutation. Pre-existing tunnels with a colliding name are NOT auto-adopted; the operator user must delete the foreign tunnel or pre-populate `status.tunnelId` out-of-band (`kubectl patch --subresource=status`). cloudflare-go/v4 v4.6.0 does not surface a tunnel `comment` field, so no CF-side tag is written for tunnels. Safe under D12 leader-election.
- **Access applications**: `tags` field (or `aud` claim) carries `managed-by=cfzt-operator` and `source-uid=<CloudflareExposure.uid>`. Name = `<displayName-or-metadata.name>-cfzt`.
- **DNS records**: `comment` field carries `managed-by=cfzt-operator source-uid=<CloudflareExposure.uid>`.
- **Ingress rules: not tagged.** The entire doc is overwritten each reconcile from K8s desired state.

**Mutation rule.** Before update or delete of an Access app or DNS record, the operator MUST verify the resource's `source-uid` matches a current local CR of the expected kind. Mismatch or missing tag → `Ready=False, Reason=ForeignResource`, no destructive action. Tunnels follow the ID-based rule above instead.

**Hostname conflict rule.** If an Access app or DNS record already exists for a hostname and its `source-uid` does not match the reconciling Exposure → `Ready=False, Reason=HostnameConflict`. Do not touch the conflicting resource. Requeue with backoff. (Ingress-rule conflicts inside the doc are resolved at build time: builder errors if two Exposures claim the same hostname, surfacing on both as `HostnameConflict`.)

**Tunnel deletion rule.** A `CloudflareTunnel` with ≥1 referencing `CloudflareExposure` cannot complete deletion. The tunnel finalizer holds, sets `Ready=False, Reason=BlockedByExposures`.

**Exposure source GC.** When `spec.sourceRef` is set and resolves in the same namespace, the Exposure controller adds an `ownerReference` from itself to the source resource. K8s GC cascades source deletion into Exposure deletion → finalizer fires.

**Drift policy.** Operator-tagged resource mutated outside the operator → reconcile rewrites to desired state. Untagged or foreign-tagged resource for the same hostname → operator leaves it alone, surfaces conflict.

## Status and conditions

Every CRD exposes exactly two conditions:

- `Ready` — desired state fully realised. `Status: True/False/Unknown`. `Reason` and `Message` carry detail.
- `Progressing` — at least one reconcile step is in flight or backing off.

`status.cloudflare.*` carries CF resource IDs and route hashes. IDs are authoritative for ownership in the operator's local view; CF-side `source-uid` tags are the defence-in-depth check before mutation.

`Reason` values (non-exhaustive):

- `CredentialsMissing`, `CredentialsInvalid`
- `TunnelCreating`, `TokenFetchFailed`, `WorkloadNotReady`
- `OriginInvalid`, `HostnameConflict`, `ForeignResource`, `ForeignTunnel`
- `AccessAppPending`, `PolicyNotFound`, `DNSWriteFailed`
- `BlockedByExposures`
- `Reconciled`

## Credentials

Cloudflare credentials live in a single Kubernetes Secret per `CloudflareTunnel`, referenced by `spec.credentialsSecretRef`. Two keys (defaults `accountId` and `apiToken`, override via `keys`).

Secret MUST live in the same namespace as the cloudflared workload (default `cfzt-system`). Enforced by API shape: `credentialsSecretRef` names only the Secret, and the namespace is always `spec.cloudflared.namespace`.

API token MVP scopes:

- `Account:Cloudflare Tunnel:Edit`
- `Access: Apps and Policies:Edit`
- `Zone:DNS:Edit` on every zone covered by managed hostnames — **only** when any referenced `CloudflareTunnel` has `dns.manage: true`.

## RBAC (operator ServiceAccount)

| API group | Resource | Verbs | Scope |
|---|---|---|---|
| `cfzt.reid.ee` | `cloudflaretunnels`, `cloudflareexposures` | `get,list,watch,create,update,patch,delete` | cluster |
| `cfzt.reid.ee` | `cloudflaretunnels/status`, `cloudflareexposures/status` | `get,update,patch` | cluster |
| `cfzt.reid.ee` | `cloudflaretunnels/finalizers`, `cloudflareexposures/finalizers` | `update` | cluster |
| `""` | `secrets` | `get,list,watch` | cluster (read credentials Secrets + own token Secrets) |
| `""` | `secrets` | `create,update,patch,delete` | cluster — **operator contract**: only writes Secrets whose name matches `<CloudflareTunnel.metadata.name>-token` in `cloudflared.namespace`. Audit trail via Events. (K8s RBAC cannot pattern-match `resourceNames` so contract is enforced in code, not RBAC.) |
| `apps` | `daemonsets` | `get,list,watch,create,update,patch,delete` | cluster — operator contract: only writes DaemonSets named `cloudflared-<CloudflareTunnel.metadata.name>` in `cloudflared.namespace`. |
| `""` | `services` | `get,list,watch` | cluster (Slice 3) |
| `gateway.networking.k8s.io` | `httproutes` | `get,list,watch` | cluster (Slice 3, conditional on CRD presence) |
| `coordination.k8s.io` | `leases` | `get,list,watch,create,update,patch,delete` | namespaced (leader election in operator namespace) |
| `""` | `events` | `create,patch` | cluster |
| `apiextensions.k8s.io` | `customresourcedefinitions` | `get,list,watch` | cluster (HTTPRoute CRD detection at startup) |

## DNS management

When `CloudflareTunnel.spec.dns.manage: true` (default), operator creates a proxied CNAME for `spec.hostname` → `<tunnelId>.cfargotunnel.com` for each Exposure. Records tagged per ownership rules.

**Zone resolution.** Operator lists zones the API token can see at startup and on cache miss, then matches the longest zone-name suffix of `spec.hostname`. No PSL parsing.

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
leaderElection:
  enabled: true                    # D12, never disable in production
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
  - `golangci-lint run`
  - `make manifests generate` then `git diff --exit-code` (fail if generated drift)
  - `make test` (unit + envtest via `setup-envtest`)
  - `helm lint charts/cfzt-operator`
- `release.yaml` — on tag `v*`:
  - Build multi-arch image, push to `ghcr.io/andrewreid/cfzt-operator:<tag>` and `:latest` (latest only on non-prerelease tags).
  - Package chart, set `version` and `appVersion` from tag, push to `oci://ghcr.io/andrewreid/charts/cfzt-operator`.
  - Create GitHub Release with autogenerated notes.

Both workflows use `GITHUB_TOKEN` with `packages: write`.

## Observability

- **Metrics** (Prometheus via controller-runtime registry):
  - `cfzt_reconcile_total{controller, result}`
  - `cfzt_reconcile_duration_seconds{controller}`
  - `cfzt_cloudflare_api_total{endpoint, status}`
  - `cfzt_cloudflare_api_duration_seconds{endpoint}`
  - `cfzt_resource_ready{kind, namespace}` (gauge; intentionally no `name` label — avoid cardinality blow-up)
- **Logs** (logr/zap, structured). Every line carries `controller`; where relevant also `namespace`, `name`, `tunnelId`, `hostname`. Log level from `--zap-log-level` (default `info`).
- **Events**: emit Kubernetes Events for state transitions: `CreatedTunnel`, `CreatedAccessApp`, `HostnameConflict`, `ForeignTunnel`, `ReconcileFailed`, `TokenRotated`, `BlockedByExposures`.

## Suggested package layout

```text
api/v1alpha1/
  cloudflaretunnel_types.go
  cloudflareexposure_types.go
  groupversion_info.go
  zz_generated_deepcopy.go         # generated

internal/controller/
  cloudflaretunnel_controller.go
  cloudflareexposure_controller.go

internal/tunnelconfig/
  builder.go            # builds desired ingress[] from list of Exposures
  reconciler.go         # invoked by Tunnel controller; single-writer per tunnel

internal/cloudflare/
  client.go             # interface wrapping cloudflare-go/v4
  real.go               # SDK implementation
  fake.go               # in-memory fake for tests
  tunnels.go
  configurations.go
  access_applications.go
  dns.go
  zones.go              # zone cache + longest-suffix resolution

internal/origin/
  service.go            # Slice 3
  httproute.go          # Slice 3

internal/naming/
  names.go              # token Secret + DaemonSet + Access app naming
  tags.go               # source-uid tag formatting + parsing

internal/workload/
  daemonset.go          # cloudflared DaemonSet construction
  token_secret.go

cmd/
  main.go               # kubebuilder-generated manager entrypoint

config/                  # kustomize, generated by kubebuilder
  default/
  manager/
  rbac/
  crd/

charts/
  cfzt-operator/        # D17

.github/workflows/
  ci.yaml
  release.yaml

docs/
  plan.md               # implementation plan (per-slice, lives in repo)
```

## Naming conventions

- `CloudflareExposure` `metadata.name`: user-chosen. Defaults to source name when `sourceRef` set; no automatic mangling.
- Access app name: `<spec.displayName | metadata.name>-cfzt` (suffix avoids collision with hand-created apps).
- Token Secret name: `<CloudflareTunnel.metadata.name>-token`. Key: `token`.
- DaemonSet name: `cloudflared-<CloudflareTunnel.metadata.name>`.
- DNS record `comment`: `managed-by=cfzt-operator source-uid=<exposure-uid>`.
- Access app `tags`: `managed-by=cfzt-operator`, `source-uid=<exposure-uid>`.
- Tunnel ownership: tracked via `CloudflareTunnel.status.tunnelId` (no CF-side comment/tag — see D9).
- Finalizer string: `cfzt.reid.ee/finalizer` on both CRDs (D21).
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
4. Tunnel controller: credential resolution, ID-based ownership reconciliation (see `## Ownership and deletion semantics`), token fetch, Secret upsert, DaemonSet upsert.
5. Token-rotation handling via pod-template checksum annotation.
6. Finalizer (no-op while no Exposures exist).
7. envtest coverage.

**Definition of done**:

- `kubectl apply` of a `CloudflareTunnel` against an empty Cloudflare account creates the tunnel, populates `status.tunnelId`, creates Secret `<name>-token`, creates DaemonSet `cloudflared-<name>`.
- `Ready=True` once DaemonSet has ≥1 ready pod.
- Reapply is a no-op.
- A pre-existing tunnel in the account with the same `spec.tunnelName` and no local `status.tunnelId` record causes the CR to go `Ready=False, Reason=ForeignTunnel` with no mutation of the foreign tunnel.
- envtest tests pass: `TestTunnelCreate`, `TestTunnelAdopt`, `TestTunnelForeignTunnelRefuses`, `TestTunnelTokenRotation`, `TestTunnelFinalizerNoop`, `TestTunnelConditionsTransition`.
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

### Post-MVP

Annotation→Exposure convenience controller, `CloudflareAccessPolicy` CRD, parentRefs-based Gateway origin auto-resolution, Ingress source, private network routes.

## Non-goals for early implementation

The early implementation does not:

- Build a web UI.
- Replace `external-dns` broadly.
- Manage arbitrary Cloudflare DNS records (only proxied CNAMEs for managed hostnames).
- Manage arbitrary Zero Trust settings.
- Support every Access policy rule type (binds existing policies by UUID only).
- Implement multi-tenant or multi-cluster semantics.
- Depend on Helm operators, Ansible operators, or Crossplane.
- Ship a validating or conversion webhook in MVP.
