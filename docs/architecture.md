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

## Three controllers, one tunnel-config writer

`CloudflareTunnel`, `CloudflareExposure`, and `CloudflareAccessPolicy` are reconciled by separate controllers. The key asymmetry: **only the Tunnel controller writes the tunnel-config doc.**

Exposure controllers validate, ensure Access apps and DNS records, then enqueue the owning Tunnel. The AccessPolicy controller manages reusable Cloudflare Access policies referenced by Exposure `policyRef.name`. The Tunnel controller lists all referencing Exposures, computes the full ingress document, and PUTs it in a single call. This is safe because:

- D11: the doc is computed entirely from Kubernetes state on every reconcile
- D12: leader election means one operator process per cluster
- D19: `MaxConcurrentReconciles=1` on the Tunnel controller

No etag, no locking, no optimistic concurrency — last-write-wins is safe under those three invariants.

The Exposure controller **must never** call the tunnel-configurations endpoint.

## Cross-controller watches (D20)

Tunnel controller watches `CloudflareExposure` (cluster-wide, maps to owning Tunnel) so any Exposure change triggers a Tunnel reconcile and a tunnel-config PUT.

Exposure controller watches `CloudflareTunnel` (maps to all Exposures referencing that Tunnel) so Tunnel status writes (route hashes) propagate back to Exposure `Ready` gating.

Exposure controller also watches `CloudflareAccessPolicy` (maps to Exposures using `policyRef.name`) so policy readiness and policy ID changes propagate into Access application binding. AccessPolicy controller watches `CloudflareExposure` (maps by `policyRef.name`) so `status.referencedBy[]` and deletion blocking stay current.

Exposure lookups are indexed by `spec.tunnelRef.name`, `spec.hostname`, and `spec.access.policyRef.name`; map functions and duplicate-host checks should use those indexes rather than cluster-wide scans.

## Ownership model

| Resource type | Ownership record |
|---|---|
| Cloudflare Tunnel | `CloudflareTunnel.status.tunnelId` (local, not a CF-side tag) |
| Access application | `tags` field: `managed-by=cfzt-operator`, `source-uid=<exposure-uid>` |
| Access policy | `CloudflareAccessPolicy.status.policyId` (local ID record; name is verified before mutation/delete) |
| DNS record | `comment` field: `managed-by=cfzt-operator source-uid=<exposure-uid>` |
| Ingress rules | Not tagged — entire doc overwritten from K8s state each reconcile |

**Mutation rule:** before updating or deleting an Access app or DNS record, the operator verifies the resource's `source-uid` matches a current local CR. Mismatch → `Ready=False, Reason=ForeignResource`, no write.

**Hostname conflict:** a resource exists for a hostname and its `source-uid` does not match → `Ready=False, Reason=HostnameConflict`. Do not touch. Requeue with backoff. Two Exposures claiming the same hostname → builder detects the collision at compile time and marks both `HostnameConflict`.

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

**Exposure finalizer:** on deletion, removes the DNS record and Access app (for owned resources only), then enqueues the owning Tunnel so the ingress doc is rewritten without the deleted Exposure.

**Tunnel finalizer:** blocks deletion while any `CloudflareExposure` references this tunnel (`Reason=BlockedByExposures`). Once all Exposures are deleted, the finalizer removes the Cloudflare tunnel, token Secret, and DaemonSet.

**AccessPolicy finalizer:** blocks deletion while any `CloudflareExposure` references this policy by name (`Reason=BlockedByExposures`). Once unreferenced, it verifies the tracked policy ID still has the expected name, deletes it, and removes the finalizer. Metadata-only reads are used in the delete path so unsupported Cloudflare rule variants cannot wedge cleanup.

Deletion only touches resources the local CR demonstrably owns. Partial cleanup on a previous delete attempt is handled gracefully — missing owned resources are not errors.

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
| `AccessAppPending` | Access app not yet confirmed ready |
| `PolicyNotFound` | Access policy UUID not found in account |
| `PolicyNotReady` | Referenced `CloudflareAccessPolicy` is missing, deleting, stale, or not ready |
| `DNSWriteFailed` | DNS record creation or update failed |
| `BlockedByExposures` | Tunnel deletion blocked by referencing Exposures |
| `ForeignPolicy` | Name-matching Access policy exists with no local ID record, or tracked policy name does not match |
| `UnsupportedDrift` | Tracked Access policy uses a Cloudflare rule variant outside the supported structured subset |
| `Reconciled` | All resources in desired state |

## Cloudflare client interface

All Cloudflare API calls go through `internal/cloudflare`. Controllers never import `cloudflare-go/v4` directly. The interface has a real implementation wrapping the SDK and a fake implementation for tests.

Rate limiting: per-token bucket inside `internal/cloudflare/client.go`, keyed by API token hash. Exponential backoff on 429 and 5xx responses.

Zone resolution: longest-suffix match against a cached zone list. The cache is populated on first use and refreshed on miss, so tokens used with managed DNS must be able to read the relevant zones. No PSL parsing.

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
  accesspolicy_hash.go             canonical policy rule hash
  conditions.go                    condition helpers
  httproute_discovery.go           startup CRD detection for HTTPRoute
  indexes.go                       Exposure field indexes

internal/tunnelconfig/
  builder.go                       builds ingress[] from Exposure list; hostname-sorted, 404 catch-all last

internal/cloudflare/
  client.go                        interface + rate limiter
  real.go                          cloudflare-go/v4 implementation
  fake.go                          in-memory fake for tests
  tunnels.go                       tunnel + token methods
  configurations.go                tunnel-config doc methods
  access_applications.go           Access app + policy binding
  access_policies.go               reusable Access policy methods
  dns.go                           DNS record CRUD
  zones.go                         zone list cache + longest-suffix resolution

internal/origin/
  service.go                       Service sourceRef — derives host + port
  httproute.go                     HTTPRoute sourceRef — derives hostname

internal/naming/
  names.go                         token Secret, DaemonSet, Access app naming
  tags.go                          source-uid tag formatting + parsing

internal/workload/
  daemonset.go                     cloudflared DaemonSet construction
  token_secret.go                  token Secret construction

cmd/
  main.go                          manager entrypoint, leader election, controller wiring

config/                            kustomize (kubebuilder-generated)
charts/cfzt-operator/              Helm chart (hand-written)
.github/workflows/
  ci.yaml                          lint + test + generated-drift gate
  release.yaml                     image + chart publish on tag

test/live/
  cloudflare_smoke_test.go         opt-in live Cloudflare lifecycle smoke
```

## Tunnel-config doc

The tunnel-config doc is the `cfd_tunnel/{id}/configurations` resource in the Cloudflare API. It contains an `ingress[]` array — one rule per published hostname, plus a final catch-all `http_status:404`.

The operator owns the entire doc. Pre-existing rules in the doc are overwritten on first reconcile. Individual rules are not tagged — the doc is derived entirely from Kubernetes state.

Ingress rules are sorted by hostname (lexicographic) for determinism. The 404 catch-all is always last.

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
| D9 | Tunnel ownership via local `status.tunnelId`, not a CF-side tag. Name collision without local ID → `ForeignTunnel`. Access apps and DNS records are tagged with `source-uid`; Access policies are tracked by `status.policyId`. |
| D10 | Origin may be explicit, or partially derived by `sourceRef`: Service can derive host/port; HTTPRoute can derive hostname. |
| D11 | Single writer for tunnel-config doc: Tunnel controller only. Exposure controller never calls the configurations endpoint. |
| D12 | Leader election required ON. |
| D13 | SDK: `github.com/cloudflare/cloudflare-go/v4`. Wrapped behind internal interface. |
| D14 | Helm OCI chart + container image on GHCR. |
| D15 | API is `v1alpha1`. Breaking changes allowed. Upgrade = export Tunnel/Exposure/AccessPolicy CRs, delete-and-recreate. |
| D16 | External (non-Kubernetes) origins are first-class. |
| D17 | Helm chart under `charts/cfzt-operator/`. CRDs in `crds/` (install-only). Leader election is always enabled. |
| D18 | CI in GitHub Actions: lint, test, generated-drift gate on PR; image + chart publish on tag. |
| D19 | `MaxConcurrentReconciles=1` on all controllers. |
| D20 | Cross-controller watches: Tunnel watches Exposure, Exposure watches Tunnel, Exposure watches AccessPolicy, and AccessPolicy watches Exposure. |
| D21 | Finalizer string: `cfzt.reid.ee/finalizer` on all owning CRDs. |
| D22 | Minimum Kubernetes 1.27 (stable CEL CRD validation). |
| D23 | Helm CRDs are install-only. ArgoCD/Flux users: document in NOTES.txt. |
| D24 | `CloudflareAccessPolicy` CRD is in scope. Exposures bind exactly one of `policyRef.uuid` or `policyRef.name` when Access is enabled. |

## Testing requirements

- Reconciliation-semantics changes (ownership, finalizers, tunnel-config builder) require envtest coverage.
- Unit tests use `internal/cloudflare/fake.go`. Real SDK is not reached in unit tests.
- Finalizer and deletion paths must have explicit tests.
- Hostname conflict, ForeignResource, and BlockedByExposures must each have a dedicated test.
- No skipping tests to fix later.

After any `api/v1alpha1` change, run `make manifests generate` and commit generated output with the API change.

```sh
make manifests generate
make test
go test ./...
go test -tags=live ./test/live -run TestCloudflarePreflight -count=1
```

Live Cloudflare tests are excluded from normal test runs. The release workflow invokes the Go live test package directly; Cloudflare verification and cleanup use typed clients instead of shell JSON parsing.

For local macOS smoke runs, copy `.env.live.example` to `.env.live`, fill in real Cloudflare values, then run `hack/live-cloudflare-local.sh preflight` or `hack/live-cloudflare-local.sh lifecycle`. The lifecycle command can start Colima, creates or reuses a local `kind` cluster, builds the operator image with Docker, loads it into kind, and runs the same Go live test package. `hack/live-cloudflare-local.sh down` deletes the kind cluster and stops Colima when the local runtime is Colima-backed.
