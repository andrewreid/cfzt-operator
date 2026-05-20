# cfzt-operator

Kubernetes operator for publishing workloads through Cloudflare Tunnel, Cloudflare Access, and managed DNS — all from a single custom resource alongside your workload.

Create one `CloudflareTunnel` per tunnel. Create one `CloudflareExposure` per hostname you want to publish. Optionally create `CloudflareAccessPolicy` resources for reusable managed Access policies. The operator handles tunnel lifecycle, cloudflared DaemonSet deployment, ingress-rule configuration, DNS CNAMEs, Access application creation, and policy binding by UUID or managed policy name.

Non-Kubernetes origins (a LAN device, home server, or any host reachable from cloudflared pods) work the same way — just set an explicit `origin` with no `sourceRef`. This keeps your entire tunnel topology in GitOps.

## Requirements

- Kubernetes 1.27+ (stable CEL CRD validation)
- Cloudflare account with an API token (scopes below)
- Helm 3

## Install

```sh
helm install cfzt-operator oci://ghcr.io/andrewreid/charts/cfzt-operator \
  --namespace cfzt-system \
  --create-namespace \
  --version 0.1.0
```

Or from the chart source:

```sh
helm install cfzt-operator charts/cfzt-operator \
  --namespace cfzt-system \
  --create-namespace
```

Create the credentials Secret in the cloudflared namespace (default `cfzt-system`):

```sh
kubectl -n cfzt-system create secret generic cloudflare-credentials \
  --from-literal=accountId='<cloudflare-account-id>' \
  --from-literal=apiToken='<cloudflare-api-token>'
```

Minimum API token scopes:

| Scope | When |
|---|---|
| Account / Cloudflare Tunnel: Edit | Always |
| Account / Access: Apps and Policies: Edit | Always |
| Zone / Zone: Read | When `dns.manage: true` so the operator can resolve hostnames to Cloudflare zone IDs |
| Zone / DNS: Edit | Only when `dns.manage: true` (default) |

## Quick start

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

Apply this and the operator:

1. Creates (or adopts) the Cloudflare tunnel
2. Fetches the tunnel token and stores it in Secret `homelab-token` in `cfzt-system`
3. Deploys a `cloudflared` DaemonSet named `cloudflared-homelab` in `cfzt-system`
4. Writes a published hostname route for `jellyfin.example.com` to the tunnel-config doc
5. Creates a proxied DNS CNAME `jellyfin.example.com` → `<tunnelId>.cfargotunnel.com`
6. Creates a Cloudflare Access application for `jellyfin.example.com` bound to the policy UUID

`kubectl get cft` and `kubectl get cfe -A` show `Ready` status columns.

## CloudflareTunnel reference

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareTunnel    # cluster-scoped, shortName: cft
metadata:
  name: homelab
spec:
  tunnelName: homelab-rke2          # name of the tunnel in Cloudflare
  credentialsSecretRef:
    name: cloudflare-credentials    # Secret in spec.cloudflared.namespace
    keys:
      accountId: accountId          # Secret key, default "accountId"
      apiToken: apiToken            # Secret key, default "apiToken"
  dns:
    manage: true                    # create proxied CNAMEs (default true)
  cloudflared:
    namespace: cfzt-system          # where cloudflared DaemonSet runs
    image: ""                       # pin a specific cloudflared image; operator uses a default
    hostNetwork: false              # set true to reach LAN origins
    resources: {}
    nodeSelector: {}
    tolerations: []
    affinity: {}
```

Status fields:

| Field | Description |
|---|---|
| `status.tunnelId` | Cloudflare tunnel UUID — authoritative ownership record |
| `status.tokenSecretRef.name` | Name of the operator-managed token Secret |
| `status.dnsMode` | `managed` or `external` |
| `status.routes[]` | Per-Exposure ingress-rule hashes written after each tunnel-config PUT |
| `status.conditions` | `Ready` and `Progressing` conditions |

**Tunnel adoption:** if `status.tunnelId` is already set, the operator verifies the existing tunnel. If `status.tunnelId` is unset and a tunnel with the same name exists in the account, the operator refuses and sets `Ready=False, Reason=ForeignTunnel` — it will not mutate a tunnel it does not own. To adopt a pre-existing tunnel, patch `status.tunnelId` out-of-band:

```sh
kubectl patch cloudflaretunnel homelab \
  --subresource=status --type=merge \
  --patch '{"status":{"tunnelId":"<tunnel-uuid>"}}'
```

## CloudflareExposure reference

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure    # namespaced, shortName: cfe
metadata:
  name: jellyfin
  namespace: media
spec:
  displayName: Jellyfin               # optional, defaults to metadata.name; used in Access app name
  hostname: jellyfin.example.com      # RFC 1123 subdomain required
  tunnelRef:
    name: homelab
  sourceRef:                          # optional — see sourceRef section below
    apiVersion: v1
    kind: Service                     # Service or HTTPRoute
    name: jellyfin
  origin:
    protocol: http                    # http or https
    host: jellyfin.media.svc.cluster.local
    port: 8096
  access:
    enabled: true
    policyRef:
      # Exactly one of uuid or name when access.enabled: true.
      uuid: 00000000-0000-4000-8000-000000000001   # UUID v4 of an existing policy
      # name: family-only                          # CloudflareAccessPolicy name
```

`access.enabled: false` exposes the hostname without Access protection.

`dns.manage: false` on the `CloudflareTunnel` skips DNS record creation for all its exposures.

Status fields:

| Field | Description |
|---|---|
| `status.cloudflare.accessApplicationId` | Cloudflare Access app UUID |
| `status.cloudflare.dnsRecordId` | DNS record ID when managed |
| `status.cloudflare.publicHostnameRouteHash` | `sha256:<hash>` of the canonical ingress rule placed in tunnel config |
| `status.conditions` | `Ready` and `Progressing` conditions |

## CloudflareAccessPolicy reference

`CloudflareAccessPolicy` is cluster-scoped (`cfap`) and manages one reusable account-level Cloudflare Access policy. Exposures can bind to it with `spec.access.policyRef.name` instead of binding directly to a Cloudflare policy UUID.

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareAccessPolicy
metadata:
  name: family-only
spec:
  credentialsSecretRef:
    namespace: cfzt-system
    name: cloudflare-credentials
  # policyName defaults to "<metadata.name>-cfzt"; cannot change after creation.
  policyName: Family Only
  decision: allow
  rules:
    include:
      - emailDomain: example.com
    require:
      - geoCountryCode: US
  sessionDuration: 24h
  purposeJustification:
    required: false
```

Supported rule item types are `email`, `emailDomain`, `ip`, `everyone: true`, `serviceToken`, and `geoCountryCode`. Each rule item must set exactly one type; `everyone: false` is rejected because it is not a real rule.

Status fields:

| Field | Description |
|---|---|
| `status.policyId` | Cloudflare Access policy UUID — authoritative ownership record |
| `status.observedRulesHash` | `sha256:<hash>` of the reconciled rule set |
| `status.referencedBy[]` | Exposures currently using `policyRef.name` |
| `status.referencedByCount` | Count of referencing Exposures |
| `status.conditions` | `Ready` and `Progressing` conditions |

Name-colliding pre-existing Cloudflare policies are not auto-adopted. The controller sets `Ready=False, Reason=ForeignPolicy` and leaves the policy untouched. Unsupported Cloudflare rule variants on the tracked policy are surfaced as `Ready=False, Reason=UnsupportedDrift`; the controller does not silently treat them as equal or erase them.

## External (non-Kubernetes) origins

Any host reachable from cloudflared pods works as an origin — a LAN device, home server, or public IP. With `hostNetwork: true` on the tunnel, cloudflared shares the node network and can reach LAN hosts directly:

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
    hostNetwork: true
---
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: home-assistant
  namespace: home
spec:
  hostname: ha.example.com
  tunnelRef:
    name: homelab
  origin:
    protocol: http
    host: homeassistant.lan
    port: 8123
  access:
    enabled: true
    policyRef:
      uuid: 00000000-0000-4000-8000-000000000001
```

## sourceRef: Service and HTTPRoute

Setting `sourceRef` lets the operator derive missing fields from an existing Kubernetes resource and wire a garbage-collection cascade.

**Service source** — `origin.host` and `origin.port` are optional when `sourceRef.kind: Service`. The operator reads the Service and fills:
- `origin.host` → `<service>.<namespace>.svc.cluster.local`
- `origin.port` → the Service's single port (error if zero or multiple ports)

```yaml
spec:
  tunnelRef:
    name: homelab
  sourceRef:
    apiVersion: v1
    kind: Service
    name: jellyfin
  origin:
    protocol: http
    # host and port derived from the Service
```

**HTTPRoute source** — `spec.hostname` is optional when `sourceRef.kind: HTTPRoute`. The operator reads the HTTPRoute's single `spec.hostnames` entry (error if zero or multiple). `origin` remains explicit.

**Owner reference and GC cascade** — when `sourceRef` resolves in the same namespace, the operator adds an `ownerReference` from the `CloudflareExposure` to the source resource. Deleting the source garbage-collects the Exposure, which fires the Exposure finalizer and cleans Cloudflare resources.

**HTTPRoute controller** is enabled only when the `gateway.networking.k8s.io` CRD is present at operator startup. If absent, the operator logs `HTTPRoute CRD not found, controller disabled` and continues normally. Adding the CRD after the operator is running requires an operator restart.

## Ownership and safety

The operator refuses to mutate Cloudflare resources it does not own.

- **Tunnels** are tracked by ID in `status.tunnelId`. A name collision with no local ID record → `Reason=ForeignTunnel`, no mutation.
- **Access applications and DNS records** carry `managed-by=cfzt-operator source-uid=<exposure-uid>` tags. A resource with a different UID → `Reason=ForeignResource` or `Reason=HostnameConflict`, no mutation.
- **Access policies** are tracked by ID in `CloudflareAccessPolicy.status.policyId`. A name collision with no local ID record → `Reason=ForeignPolicy`, no mutation.
- **Ingress rules** inside the tunnel-config doc are always fully rewritten from Kubernetes state — they are not tagged individually.

Repeated reconciles are idempotent. Drift in the Cloudflare dashboard is corrected on the next reconcile.

## Deletion

Deleting a `CloudflareExposure` removes:
- The Cloudflare Access application (when enabled)
- The DNS CNAME record (when managed)
- The ingress rule from the tunnel-config doc

Deleting a `CloudflareTunnel` while Exposures still reference it is blocked. The tunnel stays, conditions show `Ready=False, Reason=BlockedByExposures`. Delete all Exposures first, then delete the tunnel.

Deleting a `CloudflareAccessPolicy` while Exposures still reference it is also blocked with `Reason=BlockedByExposures`. Delete or update the referencing Exposures first.

## CRD upgrades

CRDs live in `charts/cfzt-operator/crds/`. Helm 3 installs them but does not upgrade them on `helm upgrade`. The API is `v1alpha1` — breaking changes are allowed without a conversion webhook. To upgrade after a breaking CRD change: export `CloudflareTunnel`, `CloudflareExposure`, and `CloudflareAccessPolicy` manifests, uninstall, install the new chart version, reapply.

## Observability

The chart can expose the controller-runtime metrics endpoint by setting `metrics.enabled: true`; it defaults off. Managed cloudflared DaemonSets also run cloudflared with `--metrics` on container port `2000`.

Kubernetes Events include `CreatedTunnel`, `CreatedAccessApp`, `CreatedAccessPolicy`, `UpdatedAccessPolicy`, `TokenRotated`, `HostnameConflict`, `ForeignTunnel`, `BlockedByExposures`, and `ReconcileFailed`.

## What is not supported

These are deliberate scope decisions, not gaps:

- **Annotation-driven UX** — the operator has no annotation controller. `CloudflareExposure` is the only user-facing surface.
- **Ingress source** — `sourceRef.kind: Ingress` is not supported.
- **Private network / WARP routing** — not in scope.
- **Cloudflare Gateway management** — not in scope.
- **Multi-cluster / multi-account** — single cluster, single account per operator instance.
- **Conversion webhooks** — `v1alpha1` is delete-and-recreate only.
- **Multi-account coordination** — credentials are configured on Tunnel and AccessPolicy CRs, but the operator does not model account boundaries. Keep each Tunnel, its Exposures, and any referenced `CloudflareAccessPolicy` in the same Cloudflare account.

## Development

```sh
rtk make manifests generate    # regenerate CRDs + deepcopy after api/ changes
rtk make test                  # unit + envtest (installs setup-envtest automatically)
rtk go test ./...              # raw test pass
rtk go test -tags=live ./test/live -run TestCloudflarePreflight -count=1
rtk helm lint charts/cfzt-operator
rtk helm template cfzt-operator charts/cfzt-operator --namespace cfzt-system
```

Regenerate manifests and deepcopy after any `api/v1alpha1` change, and commit the generated output alongside the API change. CI fails on uncommitted generated drift.

The live Cloudflare smoke harness is opt-in via the `live` build tag and uses the repo Cloudflare client plus a disposable kind cluster. It requires `CF_ACCOUNT_ID`, `CF_API_TOKEN`, and `CF_TEST_ZONE`; set `CF_ZONE_ID` when the token can manage DNS records but cannot list zones. For local runs on macOS:

```sh
cp .env.live.example .env.live
$EDITOR .env.live

hack/live-cloudflare-local.sh preflight   # no kind cluster needed
hack/live-cloudflare-local.sh lifecycle   # starts Colima if needed, creates/reuses kind, runs full test
hack/live-cloudflare-local.sh down        # deletes kind cluster and stops Colima when using it
```

See [docs/architecture.md](docs/architecture.md) for reconciliation semantics, package layout, and design decisions.

## License

Apache-2.0.
