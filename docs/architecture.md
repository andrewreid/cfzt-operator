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

## Two controllers, one writer

`CloudflareTunnel` and `CloudflareExposure` are reconciled by separate controllers. The key asymmetry: **only the Tunnel controller writes the tunnel-config doc.**

Exposure controllers validate, ensure Access apps and DNS records, then enqueue the owning Tunnel. The Tunnel controller lists all referencing Exposures, computes the full ingress document, and PUTs it in a single call. This is safe because:

- D11: the doc is computed entirely from Kubernetes state on every reconcile
- D12: leader election means one operator process per cluster
- D19: `MaxConcurrentReconciles=1` on the Tunnel controller

No etag, no locking, no optimistic concurrency — last-write-wins is safe under those three invariants.

The Exposure controller **must never** call the tunnel-configurations endpoint.

## Cross-controller watches (D20)

Tunnel controller watches `CloudflareExposure` (cluster-wide, maps to owning Tunnel) so any Exposure change triggers a Tunnel reconcile and a tunnel-config PUT.

Exposure controller watches `CloudflareTunnel` (maps to all Exposures referencing that Tunnel) so Tunnel status writes (route hashes) propagate back to Exposure `Ready` gating.

## Ownership model

| Resource type | Ownership record |
|---|---|
| Cloudflare Tunnel | `CloudflareTunnel.status.tunnelId` (local, not a CF-side tag) |
| Access application | `tags` field: `managed-by=cfzt-operator`, `source-uid=<exposure-uid>` |
| DNS record | `comment` field: `managed-by=cfzt-operator source-uid=<exposure-uid>` |
| Ingress rules | Not tagged — entire doc overwritten from K8s state each reconcile |

**Mutation rule:** before updating or deleting an Access app or DNS record, the operator verifies the resource's `source-uid` matches a current local CR. Mismatch → `Ready=False, Reason=ForeignResource`, no write.

**Hostname conflict:** a resource exists for a hostname and its `source-uid` does not match → `Ready=False, Reason=HostnameConflict`. Do not touch. Requeue with backoff. Two Exposures claiming the same hostname → builder detects the collision at compile time and marks both `HostnameConflict`.

**Tunnel adoption:** if `status.tunnelId` is unset and a name-matched tunnel exists → `Ready=False, Reason=ForeignTunnel`, no mutation. Pre-existing tunnels must be adopted out-of-band by patching `status.tunnelId` directly.

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

Both CRDs use finalizer `cfzt.reid.ee/finalizer`.

**Exposure finalizer:** on deletion, removes the DNS record and Access app (for owned resources only), then enqueues the owning Tunnel so the ingress doc is rewritten without the deleted Exposure.

**Tunnel finalizer:** blocks deletion while any `CloudflareExposure` references this tunnel (`Reason=BlockedByExposures`). Once all Exposures are deleted, the finalizer removes the Cloudflare tunnel, token Secret, and DaemonSet.

Deletion only touches resources the local CR demonstrably owns. Partial cleanup on a previous delete attempt is handled gracefully — missing owned resources are not errors.

## Status conditions

Both CRDs expose exactly `Ready` and `Progressing`. Detail is in `Reason` and `Message`.

`Reason` values:

| Value | Meaning |
|---|---|
| `CredentialsMissing` | Secret not found or missing key |
| `CredentialsInvalid` | API token rejected |
| `TunnelCreating` | Waiting for Cloudflare tunnel to become available |
| `TokenFetchFailed` | Could not retrieve tunnel token |
| `WorkloadNotReady` | DaemonSet has no ready pods |
| `OriginInvalid` | Origin field validation failure |
| `HostnameConflict` | CF resource for hostname owned by different CR |
| `ForeignResource` | CF resource owned by unknown source |
| `ForeignTunnel` | Name-matching tunnel exists with no local ID record |
| `AccessAppPending` | Access app not yet confirmed ready |
| `PolicyNotFound` | Access policy UUID not found in account |
| `DNSWriteFailed` | DNS record creation or update failed |
| `BlockedByExposures` | Tunnel deletion blocked by referencing Exposures |
| `Reconciled` | All resources in desired state |

## Cloudflare client interface

All Cloudflare API calls go through `internal/cloudflare`. Controllers never import `cloudflare-go/v4` directly. The interface has a real implementation wrapping the SDK and a fake implementation for tests.

Rate limiting: per-token bucket inside `internal/cloudflare/client.go`, keyed by API token hash. Exponential backoff on 429 and 5xx responses.

Zone resolution: longest-suffix match against a cached zone list. Cache is refreshed on miss and on 401 responses. No PSL parsing.

## Package layout

```
api/v1alpha1/
  cloudflaretunnel_types.go        CRD types, cluster-scoped
  cloudflareexposure_types.go      CRD types, namespaced
  groupversion_info.go
  zz_generated.deepcopy.go         generated — do not edit

internal/controller/
  cloudflaretunnel_controller.go   Tunnel reconcile loop
  cloudflareexposure_controller.go Exposure reconcile loop
  conditions.go                    condition helpers
  httproute_discovery.go           startup CRD detection for HTTPRoute

internal/tunnelconfig/
  builder.go                       builds ingress[] from Exposure list; hostname-sorted, 404 catch-all last
  reconciler.go                    invoked by Tunnel controller; PUT full doc

internal/cloudflare/
  client.go                        interface + rate limiter
  real.go                          cloudflare-go/v4 implementation
  fake.go                          in-memory fake for tests
  tunnels.go                       tunnel + token methods
  configurations.go                tunnel-config doc methods
  access_applications.go           Access app + policy binding
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
```

## Tunnel-config doc

The tunnel-config doc is the `cfd_tunnel/{id}/configurations` resource in the Cloudflare API. It contains an `ingress[]` array — one rule per published hostname, plus a final catch-all `http_status:404`.

The operator owns the entire doc. Pre-existing rules in the doc are overwritten on first reconcile. Individual rules are not tagged — the doc is derived entirely from Kubernetes state.

Ingress rules are sorted by hostname (lexicographic) for determinism. The 404 catch-all is always last.

## cloudflared DaemonSet

The operator manages one DaemonSet named `cloudflared-<tunnel-name>` in `spec.cloudflared.namespace`. Token rotation is signalled via a pod-template annotation `cfzt.reid.ee/token-checksum=<sha256(token)>` — changing the checksum triggers a rolling update. The DaemonSet is never deleted and recreated for token rotation.

`updateStrategy: RollingUpdate, maxUnavailable: 1`.

When `hostNetwork: true`, `dnsPolicy` is set to `ClusterFirstWithHostNet`.

The cloudflared image is pinned to a specific version as a Go constant in `internal/workload/daemonset.go`. The `:latest` tag is rejected by CRD validation.

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
| D9 | Tunnel ownership via local `status.tunnelId`, not a CF-side tag. Name collision without local ID → `ForeignTunnel`. Access/DNS resources tagged with `source-uid`. |
| D10 | Origin always explicit in MVP. Slice 3 adds Service defaulting. |
| D11 | Single writer for tunnel-config doc: Tunnel controller only. Exposure controller never calls the configurations endpoint. |
| D12 | Leader election required ON. |
| D13 | SDK: `github.com/cloudflare/cloudflare-go/v4`. Wrapped behind internal interface. |
| D14 | Helm OCI chart + container image on GHCR. |
| D15 | API is `v1alpha1`. Breaking changes allowed. Upgrade = export Tunnel/Exposure/AccessPolicy CRs, delete-and-recreate. |
| D16 | External (non-Kubernetes) origins are first-class. |
| D17 | Helm chart under `charts/cfzt-operator/`. CRDs in `crds/` (install-only). Leader election is always enabled. |
| D18 | CI in GitHub Actions: lint, test, generated-drift gate on PR; image + chart publish on tag. |
| D19 | `MaxConcurrentReconciles=1` on both controllers. |
| D20 | Cross-controller watches: Tunnel watches Exposure, Exposure watches Tunnel. |
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
rtk make manifests generate
rtk make test
rtk go test ./...
```
