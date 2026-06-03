# cfzt-operator architecture

Reconciliation semantics, package layout, binding decisions, and invariants. Read this before making changes to controller or cloudflare-client code.

## Design philosophy

The hard part of this operator is not making Cloudflare API calls. It is:

- deciding what the operator owns
- preventing accidental deletion of resources it did not create
- handling partial failure without rollback
- making repeated reconciles idempotent
- reporting useful status when things go wrong

Every decision below follows from these constraints.

## Four controllers, one tunnel-config writer

`CloudflareTunnel`, `CloudflareExposure`, `CloudflareAccessPolicy`, and `CloudflareTunnelRoute` are reconciled by separate controllers. The key asymmetry: **only the Tunnel controller writes the tunnel-config doc.**

Exposure controllers validate, ensure Access applications and DNS records, then enqueue the owning Tunnel. The AccessPolicy controller manages reusable Cloudflare Access policies referenced by Exposure app policy bindings. The Tunnel controller lists all referencing Exposures, computes the full ingress document, and PUTs it in a single call. This is safe because:

- D11: the doc is computed entirely from Kubernetes state on every reconcile
- D12: leader election means one operator process per cluster
- D19: `MaxConcurrentReconciles=1` on the Tunnel controller

No etag, no locking, no optimistic concurrency — last-write-wins is safe under those three invariants.

The Exposure controller **must never** call the tunnel-configurations endpoint.

## Cross-controller watches (D20)

Tunnel controller watches `CloudflareExposure` (cluster-wide, maps to owning Tunnel) so any Exposure change triggers a Tunnel reconcile and a tunnel-config PUT. It also watches `CloudflareTunnelRoute` so Tunnel deletion blocking is recalculated when routes change.

Exposure controller watches `CloudflareTunnel` (maps to all Exposures referencing that Tunnel) so Tunnel status writes (route hashes) propagate back to Exposure `Ready` gating.

Exposure controller also watches `CloudflareAccessPolicy` (maps to Exposures using `access.applications[].policies[].policyRef.name`) so policy readiness and policy ID changes propagate into Access application binding. AccessPolicy controller watches `CloudflareExposure` (maps by those nested policy refs) so `status.referencedBy[]` and deletion blocking stay current.

TunnelRoute controller watches `CloudflareTunnel` (maps to routes by `spec.tunnelRef.name`) so route reconciliation follows Tunnel readiness and ID changes.

Exposure lookups are indexed by `spec.tunnelRef.name`, `spec.hostname`, `spec.access.applications[].policies[].policyRef.name`, and `spec.failover.group`; TunnelRoute lookups are indexed by `spec.tunnelRef.name`. Map functions, duplicate-host checks, and the failover group-conflict check should use those indexes rather than cluster-wide scans.

## Ownership model

| Resource type | Ownership record |
|---|---|
| Cloudflare Tunnel | `CloudflareTunnel.status.tunnelId` (local, not a CF-side tag) |
| Access application | `tags` field: `managed-by=cfzt-operator`, chunked `source-uid-<n>=...` values |
| Access policy | `CloudflareAccessPolicy.status.policyId` (local ID record; name is verified before mutation/delete) |
| DNS record | `comment` field: `managed-by=cfzt-operator source-uid=<exposure-uid>` |
| Tunnel route | `CloudflareTunnelRoute.status.routeId` plus compact `comment` field: `managed-by=cfzt source-uid=<route-uid>` |
| Ingress rules | Not tagged — entire doc overwritten from K8s state each reconcile |

**Mutation rule:** before updating or deleting an Access app, DNS record, or tunnel route, the operator verifies the resource's `source-uid` matches a current local CR. Mismatch → `Ready=False, Reason=ForeignResource` or route-specific `ForeignRoute`, no write.

**Failover exception (D26):** for an Exposure with `spec.failover`, the shared Access application set and DNS CNAME carry the **failover-group ID** as their `source-uid` instead of the per-CR uid (`ownership.FromFailoverGroup`), so either cluster recognizes them as owned. `MatchesComment`/`MatchesTags` accept either a per-CR uid (non-failover) or a group ID (failover). See [DR failover](#dr-failover-d26).

**Hostname conflict:** a resource exists for a hostname and its `source-uid` does not match → `Ready=False, Reason=HostnameConflict`. Do not touch. Requeue after 30 seconds. Two Exposures claiming the same hostname → builder detects the collision at compile time and marks both `HostnameConflict`.

**Tunnel adoption:** if `status.tunnelId` is unset and a name-matched tunnel exists → `Ready=False, Reason=ForeignTunnel`, no mutation. Pre-existing tunnels must be adopted out-of-band by patching `status.tunnelId` directly.

**Policy adoption:** if `CloudflareAccessPolicy.status.policyId` is unset and a name-matched Cloudflare Access policy exists → `Ready=False, Reason=ForeignPolicy`, no mutation. The operator does not auto-adopt existing policies.

## Reconcile idempotency

Every reconcile follows:

1. Observe Kubernetes desired state
2. Observe Cloudflare actual state
3. Compute desired Cloudflare state
4. Verify ownership before any mutation of persistent CF resources
5. Create missing resources
6. Update differing resources
7. Delete only inside finalizers, only for owned resources
8. Write status
9. Requeue only on retryable error or periodic resync

If you cannot prove a code path is idempotent, it is wrong.

## Finalizers

All owning CRDs use finalizer `cfzt.reid.ee/finalizer`.

**Exposure finalizer:** on deletion, removes the DNS record and Access applications (for owned resources only), then enqueues the owning Tunnel so the ingress doc is rewritten without the deleted Exposure. For a failover Exposure it first proves *live* lease ownership and removes the shared CNAME / Access app set only when this site holds the lease, otherwise removing just its own lease record (see [DR failover](#dr-failover-d26)).

**Tunnel finalizer:** blocks deletion while any `CloudflareExposure` references this tunnel (`Reason=BlockedByExposures`) or any `CloudflareTunnelRoute` references it (`Reason=BlockedByRoutes`). Once all dependants are deleted, the finalizer removes the Cloudflare tunnel, token Secret, and DaemonSet.

**AccessPolicy finalizer:** blocks deletion while any `CloudflareExposure` references this policy by name (`Reason=BlockedByExposures`). Once unreferenced, it verifies the tracked policy ID still has the expected name, deletes it, and removes the finalizer. Metadata-only reads are used in the delete path so unsupported Cloudflare rule variants cannot wedge cleanup.

**TunnelRoute finalizer:** verifies the tracked route ID and compact ownership comment before deleting the Cloudflare private-network route. Missing owned routes are treated as already gone.

Deletion only touches resources the local CR demonstrably owns. Partial cleanup on a previous delete attempt is handled gracefully — missing owned resources are not errors.

## DR failover (D26)

Active-passive multi-cluster DR is opt-in per Exposure via `spec.failover`. Two clusters apply the same Exposure (matching `spec.failover.group`, each with its own `CloudflareTunnel` and its own `--site-id`) and cooperate over one hostname. Exactly one cluster is Primary and writes the shared public CNAME + Access application set; the others are warm Standbys. Nothing in this section runs for Exposures without `spec.failover`.

### Best-effort lease, not a lock

Coordination is a single Cloudflare **DNS TXT lease record** at `_cfzt-lease.<hash8(group)>.<zone>` (zone resolved by the same longest-suffix match as the hostname). Cloudflare DNS has **no conditional-write precondition and no TXT-uniqueness guarantee**, so the lease is an optimistic, eventually-consistent coordination record — explicitly **not** linearizable, **not** a compare-and-swap lock. Correctness does not depend on lease atomicity. Safety rests on two data-plane properties:

1. There is one public CNAME, targeting one tunnel ID at a time — two sites can never serve divergent origins.
2. A failed primary's `cloudflared` drops its edge connection within seconds, so Cloudflare stops routing to its tunnel regardless of lease state.

The lease only elects a single writer to damp CNAME flapping between two *healthy* sites. The earlier "CAS-by-record_id" design was abandoned because the API cannot express it; `internal/cloudflare` exposes plain `Create`/`Update` (TXT-aware) and the fake models real semantics (a `Create` race can produce duplicate TXT records).

### Lease payload

Single TXT string: `v=1 site=<site-id> tunnel=<tunnel-id> exp=<unix> renewed=<unix>`. Parsed/serialised in `internal/dr/lease.go` (order-agnostic parse, fixed-order serialise; unknown/duplicate keys and wrong version rejected). The record also carries the group-ID ownership comment so the controller never touches a foreign record at the lease name.

### Role gate (top of Exposure reconcile)

For a failover Exposure the controller resolves role **before** any shared Access/DNS write:

1. Reject if `--site-id` is the chart default (`FailoverRequiresDistinctSiteID`) or the referenced tunnel has `dns.manage: false` (`FailoverRequiresManagedDNS`).
2. Reject if another Exposure in the cluster shares the group (`FailoverGroupConflict`, cluster-wide — see below).
3. List group-owned lease records. `>1` → deterministic resolution (below). Foreign/unparseable → fail closed (`LeaseConflict`).
4. `internal/dr.Decide` (pure state machine over now / site / previous role / observed lease / leaseSeconds / force token) returns one of: **Wait** (peer holds live lease → Standby, early return, no shared writes), **Acquire** (absent or expired or force), **Renew** (self-owned), **SplitBrain** (was Primary, peer now holds a live lease → demote).
5. Acquire/Renew write the lease then **read back and verify** the surviving single record names this site; otherwise demote. This bounds the dual-writer window to ~one reconcile.

Only a verified Primary proceeds to the shared Access/DNS writes (with the group-ID `source-uid`) and requeues at `leaseSeconds/2` to renew. A promoted standby re-lists the group-owned Access applications remotely, so the shared set is discovered from Cloudflare rather than stale local status.

### Deterministic duplicate resolution

`internal/dr.Resolve` adjudicates a `>1` lease set identically on every site (no coordination): among unexpired records the lowest `site-id` wins (ties on record ID); the winner deletes the others, a non-winner deletes only its own duplicate and demotes. Converges to one record within a reconcile or two. This is the only place a simultaneous-acquire race is healed; renewals operate on the single owned record and never create duplicates.

### Cluster-wide group uniqueness

A failover `group` identifies **one** logical exposure. Within a cluster there must be exactly one member: the lease name has no namespace/hostname and all Exposures in a cluster share one `--site-id`, so two same-group members would share a lease and each read it as self-owned. The guard (`hasFailoverGroupConflict`, indexed by `spec.failover.group`) is **cluster-wide**, not per-namespace. The legitimate cross-cluster pair lives in separate apiservers, so each operator only ever lists its own cluster and never trips it.

### Force-promote (GitOps-safe)

`cfzt.reid.ee/force-promote` carries a caller-chosen **token**. A Standby acquires regardless of expiry only when the token differs from `status.failover.lastForcePromoteToken`; the controller records the honored token and **never mutates the annotation**, so a re-applied (Flux) token does not replay. Change the token to force again.

### Deletion proves live ownership

The Exposure finalizer reads the **live** lease before tearing down the shared CNAME/Access and removes them only when exactly one group-owned record exists and names this site (`holdsLiveLease`). A stale `status.failover.role`, a duplicate set, or a peer-held lease all fail safe — the finalizer removes only this site's own lease record(s) and leaves shared resources for the live owner.

### Observability

Events: `PromotedToPrimary`, `DemotedToStandby`, `LeaseAcquired`, `LeaseRenewed`, `LeaseLost`, `LeaseConflict`, `SplitBrainDetected`, `ForcePromoted`. Metrics (labelled `namespace`/`name`/`group`/`site_id`): `cfzt_failover_role` (0/1/2), `cfzt_failover_lease_renew_total`, `cfzt_failover_promotion_total`.

## Status conditions

All owning CRDs expose exactly `Ready` and `Progressing`. Detail is in `Reason` and `Message`.

`Reason` values:

| Value | Meaning |
|---|---|
| `CredentialsMissing` | Secret not found or missing key |
| `TunnelCreating` | Waiting for Cloudflare tunnel to become available |
| `TokenFetchFailed` | Could not retrieve tunnel token |
| `WorkloadNotReady` | DaemonSet has no ready pods |
| `OriginInvalid` | Origin field validation failure |
| `HostnameConflict` | CF resource for hostname owned by different CR |
| `ForeignResource` | CF resource owned by unknown source |
| `ForeignTunnel` | Name-matching tunnel exists with no local ID record |
| `AccessAppPending` | Access app set not yet confirmed ready |
| `PolicyNotFound` | Access policy UUID not found in account |
| `PolicyNotReady` | Referenced `CloudflareAccessPolicy` is missing, deleting, stale, or not ready |
| `DNSWriteFailed` | DNS record creation or update failed |
| `BlockedByExposures` | Tunnel deletion blocked by referencing Exposures |
| `ForeignPolicy` | Name-matching Access policy exists with no local ID record, or tracked policy name does not match |
| `TunnelNotReady` | Referenced Tunnel is missing, deleting, or lacks a Cloudflare tunnel ID |
| `NetworkInvalid` | TunnelRoute CIDR cannot be parsed or canonicalized |
| `ForeignRoute` | Existing Cloudflare private-network route is not owned by this CR |
| `RouteWriteFailed` | Cloudflare private-network route create/update failed |
| `BlockedByRoutes` | Tunnel deletion blocked by referencing TunnelRoutes |
| `UnsupportedDrift` | Tracked Access policy uses a Cloudflare rule variant outside the supported structured subset |
| `Standby` | Failover Exposure is healthy but not the lease holder (warm standby) |
| `LeaseConflict` | Failover lease is ambiguous (duplicate/foreign/unparseable); failing closed, no shared write |
| `FailoverRequiresManagedDNS` | `spec.failover` set but the referenced tunnel has `dns.manage: false` |
| `FailoverRequiresDistinctSiteID` | `spec.failover` set but `--site-id` is the chart default |
| `FailoverGroupConflict` | Another Exposure in this cluster shares the `spec.failover.group` |
| `Reconciled` | All resources in desired state |

## Cloudflare client interface

All Cloudflare API calls go through `internal/cloudflare`. Controllers never import `cloudflare-go/v4` directly. The interface has a real implementation wrapping the SDK and a fake implementation for tests.

Rate limiting: per-token bucket inside `internal/cloudflare/client.go`, keyed by API token hash. Exponential backoff on 429 and 5xx responses.

Zone resolution: longest-suffix match against a cached zone list. The cache is populated on first use and refreshed on miss, so tokens used with managed DNS must be able to read the relevant zones. No PSL parsing.

DNS `Create`/`Update` are record-type aware (CNAME for published hostnames, TXT for the D26 failover lease) and carry no conditional-write or CAS semantics — the Cloudflare API has none. The fake mirrors this: `Create` is non-atomic and a race can yield duplicate TXT records, which the failover controller resolves itself (see [DR failover](#dr-failover-d26)).

Access policy `List` and `GetMetadata` return metadata only and intentionally do not parse rule bodies; this prevents unsupported rule variants on unrelated policies from blocking name or finalizer checks. `Get` parses rule bodies and returns `ErrUnsupportedAccessRule` so the controller can surface tracked unsupported drift instead of silently treating it as equal.

## Package layout

```
api/v1alpha1/
  cloudflaretunnel_types.go        CRD types, cluster-scoped
  cloudflareexposure_types.go      CRD types, namespaced
  cloudflareaccesspolicy_types.go  CRD types, cluster-scoped
  groupversion_info.go
  zz_generated.deepcopy.go         generated — do not edit

internal/controller/
  cloudflaretunnel_controller.go   Tunnel reconcile loop
  cloudflareexposure_controller.go Exposure reconcile loop
  cloudflareaccesspolicy_controller.go  managed Access policy reconcile loop
  cloudflaretunnelroute_controller.go   tunnel private-network route reconcile loop
  accesspolicy_hash.go             canonical policy rule hash
  exposure_failover.go             D26 role gate, lease read/write/verify, resolution, deletion proof
  failover_metrics.go              cfzt_failover_* metrics (labelled with site_id)
  base.go                          shared reconciler dependencies and event recorder adapter
  conditions.go                    condition helpers
  httproute_discovery.go           startup CRD detection for HTTPRoute
  indexes.go                       Exposure field indexes (incl. spec.failover.group)
  mapper.go                        typed watch map helper

internal/tunnelconfig/
  builder.go                       builds ingress[] from Exposure list; hostname-sorted, 404 catch-all last

internal/cloudflare/
  client.go                        interface + rate limiter
  real.go                          cloudflare-go/v4 implementation
  fake.go                          in-memory fake for tests
  tunnels.go                       tunnel + token methods
  configurations.go                tunnel-config doc methods
  access_applications.go           Access app + policy binding
  access_tags.go                   Access app tag helper for Cloudflare's tag length limit
  access_policies.go               reusable Access policy methods
  dns.go                           DNS record CRUD
  zones.go                         zone list cache + longest-suffix resolution

internal/dr/                       D26 DR failover primitives (pure; no K8s, no CF API)
  lease.go                         TXT lease parse / serialise
  jitter.go                        symmetric acquire jitter
  resolve.go                       deterministic duplicate-lease resolution
  role.go                          role state machine (Decide)

internal/origin/
  service.go                       Service sourceRef — derives host + port
  httproute.go                     HTTPRoute sourceRef — derives hostname

internal/naming/
  names.go                         token Secret, DaemonSet, Access app naming, failover lease TXT name

internal/ownership/
  owner.go                         ownership facade for comments and Access tags
  comment.go                       DNS and TunnelRoute source-uid comments
  accesstag.go                     chunked Access app source-uid tags

internal/workload/
  daemonset.go                     cloudflared DaemonSet construction
  token_secret.go                  token Secret construction

cmd/
  main.go                          manager entrypoint, leader election, controller wiring

config/                            kustomize (kubebuilder-generated)
charts/cfzt-operator/              Helm chart (hand-written)
.github/workflows/
  ci.yaml                          lint + test + generated-drift gate
  live-smoke.yaml                  manual live Cloudflare smoke gate
  release.yaml                     manual release gate, image + chart publish

test/live/
  cloudflare_smoke_test.go         opt-in live Cloudflare lifecycle smoke
  failover_smoke_test.go           opt-in live DR failover lifecycle smoke
  harness.go                       live smoke Kubernetes and Cloudflare helpers
```

## Tunnel-config doc

The tunnel-config doc is the `cfd_tunnel/{id}/configurations` resource in the Cloudflare API. It contains an `ingress[]` array — one rule per published hostname, plus a final catch-all `http_status:404`.

The operator owns the entire doc. Pre-existing rules in the doc are overwritten on first reconcile. Individual rules are not tagged — the doc is derived entirely from Kubernetes state.

Ingress rules are sorted by hostname (lexicographic) for determinism. The 404 catch-all is always last.

`CloudflareTunnel.status.ingressDocHash` stores the desired document hash from the last successful write. If the recomputed hash is unchanged, the Tunnel controller skips the Cloudflare configuration update.

## cloudflared DaemonSet

The operator manages one DaemonSet named `cloudflared-<tunnel-name>` in `spec.cloudflared.namespace`. Token rotation is signalled via a pod-template annotation `cfzt.reid.ee/token-checksum=<sha256(token)>` — changing the checksum triggers a rolling update. The DaemonSet is never deleted and recreated for token rotation.

`updateStrategy: RollingUpdate, maxUnavailable: 1`.

When `hostNetwork: true`, `dnsPolicy` is set to `ClusterFirstWithHostNet`.

The cloudflared image is pinned to a specific version as a Go constant in `internal/workload/daemonset.go`. The `:latest` tag is rejected by CRD validation. Generated pods set `automountServiceAccountToken: false`, run with `seccompProfile: RuntimeDefault`, and expose cloudflared metrics on container port `2000`.

## Binding decisions

| # | Decision |
|---|---|
| D1 | Remotely-managed tunnel config only. cloudflared pods get no `config.yml`. |
| D2 | DNS managed by default. `spec.dns.manage: false` → operator creates zero DNS records. |
| D3 | Superseded by D24. Existing Access policy UUID binding remains supported. |
| D4 | Per-tunnel token stored in operator-managed Secret. No `cert.pem`. |
| D5 | CR-only interface. No annotation controller. |
| D6 | `CloudflareTunnel` is cluster-scoped. cloudflared workload is namespaced. |
| D7 | DaemonSet only. No Deployment option. |
| D8 | `Ready` + `Progressing` conditions only. Detail in Reason/Message. |
| D9 | Tunnel ownership via local `status.tunnelId`, not a CF-side tag. Name collision without local ID → `ForeignTunnel`. Access applications, DNS records, and TunnelRoutes carry `source-uid` ownership markers; Access policies are tracked by `status.policyId`. Superseded by D26 for failover Exposures (group-ID `source-uid` on the shared Access application set + CNAME). |
| D10 | Origin may be explicit, or partially derived by `sourceRef`: Service can derive host/port; HTTPRoute can derive hostname. |
| D11 | Single writer for tunnel-config doc: Tunnel controller only. Exposure controller never calls the configurations endpoint. |
| D12 | Leader election required ON. |
| D13 | SDK: `github.com/cloudflare/cloudflare-go/v4`. Wrapped behind internal interface. |
| D14 | Helm OCI chart + container image on GHCR. |
| D15 | API is `v1alpha1`. Breaking changes allowed. Upgrade = export Tunnel/Exposure/AccessPolicy CRs, delete-and-recreate. |
| D16 | External (non-Kubernetes) origins are first-class. |
| D17 | Helm chart under `charts/cfzt-operator/`. CRDs in `crds/` (install-only). Leader election is always enabled. |
| D18 | CI in GitHub Actions: lint, test, generated-drift gate on PR; release workflow gates and publishes the image + chart. |
| D19 | `MaxConcurrentReconciles=1` on all controllers. |
| D20 | Cross-controller watches: Tunnel watches Exposure and TunnelRoute, Exposure watches Tunnel and AccessPolicy, AccessPolicy watches Exposure, and TunnelRoute watches Tunnel. |
| D21 | Finalizer string: `cfzt.reid.ee/finalizer` on all owning CRDs. |
| D22 | Minimum Kubernetes 1.27 (stable CEL CRD validation). |
| D23 | Helm CRDs are install-only. ArgoCD/Flux users: document in NOTES.txt. |
| D24 | `CloudflareAccessPolicy` CRD is in scope. Exposures bind Access applications through nested `access.applications[].policies[].policyRef` entries when Access is enabled. |
| D25 | `CloudflareTunnelRoute` CRD is in scope. Tunnel private-network routes are reconciled independently and block Tunnel deletion while present. |
| D26 | Active-passive multi-cluster DR is in scope as a per-Exposure opt-in (`spec.failover`), supersedes D9 for failover Exposures. Mandatory `--site-id` per process. Coordination via a best-effort Cloudflare DNS TXT lease (not linearizable). DNS-only — no external coordination (Workers KV / LB / etcd). Active-active and cross-cluster federation remain out of scope. See [DR failover](#dr-failover-d26). |

## Testing requirements

- Reconciliation-semantics changes (ownership, finalizers, tunnel-config builder) require envtest coverage.
- Unit tests use `internal/cloudflare/fake.go`. Real SDK is not reached in unit tests.
- Finalizer and deletion paths must have explicit tests.
- Hostname conflict, ForeignResource, and BlockedByExposures must each have a dedicated test.
- DR failover (D26) paths require dedicated tests: `internal/dr` lease serde / `Resolve` / jitter unit tests, and Exposure envtests for promotion, auto-promote on expiry, returning-primary self-demote, duplicate-lease resolution, group conflict (incl. cross-namespace), distinct-site-id, force-promote token replay, and fail-safe deletion under duplicates.
- No skipping tests to fix later.

After any `api/v1alpha1` change, run `make manifests generate` and commit generated output with the API change.

```sh
make manifests generate
make helm-sync-crds
make lint
make test
go test ./...
go test -tags=live ./test/live -run TestCloudflarePreflight -count=1
```

Live Cloudflare tests are excluded from normal test runs. The release workflow invokes the Go live test package directly; Cloudflare verification and cleanup use typed clients instead of shell JSON parsing.

For local macOS smoke runs, copy `.env.live.example` to `.env.live`, fill in real Cloudflare values, then run `hack/live-cloudflare-local.sh preflight` or `hack/live-cloudflare-local.sh lifecycle`. The lifecycle command can start Colima, creates or reuses a local `kind` cluster, builds the operator image with Docker, loads it into kind, and runs the same Go live test package. `hack/live-cloudflare-local.sh down` deletes the kind cluster and stops Colima when the local runtime is Colima-backed.
