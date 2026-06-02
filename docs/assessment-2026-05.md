# cfzt-operator architecture assessment

Reviewer: senior Go / Kubernetes operator perspective.
Date: 2026-05-27.
Scope: dependency burden, structural complexity, resilience, best-practice gaps, and architectural decisions (D1–D25). Working codebase as baseline.

Prior review (`claude-review.md`, 2026-05) covered structural simplification with `spec.md` decisions held off-limits. Its findings have largely been actioned (see §1.2 below). This document complements that work and intentionally engages with the harder architectural questions.

The codebase is in good shape. Invariants are stated and respected; the SDK boundary is clean; fakes mirror reals; CEL validation pulls real weight. Most of what follows is targeted at *production-readiness* — turning a sound MVP into something resilient under operational stress — and at a small number of architectural choices where a different default would be meaningfully better.

---

## 1. Snapshot

### 1.1 Numbers

| Metric | Value |
|---|---|
| Production Go (excluding generated + tests) | ~5.6 k LoC |
| Test Go (envtest + unit) | ~5.1 k LoC |
| Direct go.mod requires | 10 |
| CRDs | 4 (Tunnel, Exposure, AccessPolicy, TunnelRoute) |
| Controllers | 4 |
| Cloudflare API surface (interfaces) | 8 (Tunnels, Configurations, AccessApplications, AccessTags, AccessPolicies, TunnelRoutes, DNSRecords, Zones) |
| Min K8s | 1.27 (D22) |
| Go | 1.25.7 |

Test/prod ratio is healthy. Generated drift is gated in CI. Helm chart is hand-written and in sync with kubebuilder via `make helm-sync-crds`.

### 1.2 Prior-review status

Spot-checked the 15 most-impactful findings from `claude-review.md`. Result:

- **Closed (14/15)**: shared `Base` reconciler with credentials loader and `setReady` helper; event-recorder adapter; index fallback removed; package-level zone + client caches keyed by credentials; `PolicyUUID`/`PolicyUUIDs` collapsed to a single representation on `AccessApplicationInput`; `internal/ownership` package owns comment + accesstag formats; tag-chunking dead branch removed; `ErrNotImplemented`, `EventReconcileFailed`, `EventForeignTunnel`, `ObservedTunnelUid` deleted; `cmd/main.go` webhook + metricsCert plumbing trimmed (186 lines); `config/manager|network-policy|prometheus|samples` removed; `Configurations.Get` dropped; conditional `Configurations.Update` via `Status.IngressDocHash`; Makefile pruned.
- **Open (1/15)**: `cover.out` still committed at repo root.

The structural cleanup work has landed. The remaining surface is mostly architectural, operational, and a smaller set of fresh code-level observations.

---

## 2. Architectural assessment

### 2.1 [H] Default cloudflared workload should be a Deployment, not a DaemonSet (challenge to D7)

**D7** pins cloudflared as a DaemonSet "only" in MVP. This is the wrong default for the operator's intended audience.

A DaemonSet means *one cloudflared pod per node*. That makes sense when:

- you need `hostNetwork: true` to reach LAN origins from every node, *and*
- every node is expected to be a tunnel ingress point.

In every other deployment shape — multi-node clusters, GKE/EKS clusters with autoscaling, clusters where only a subset of nodes should run egress workloads — a DaemonSet:

- scales connector count with node count rather than with traffic or HA requirements,
- wastes resources on nodes that will never serve tunnel traffic,
- prevents tuning replica count independently of cluster size (a 50-node cluster gets 50 cloudflared pods you don't need; a 2-node cluster gets 2 replicas you probably do want),
- complicates connector rollouts (every node simultaneously vs. a controlled rolling update),
- is not aligned with Cloudflare's own published documentation, which recommends a Deployment with `replicas: 2+` as the canonical pattern.

A Deployment with two replicas is the right default. Cloudflared supports N parallel connectors per tunnel and Cloudflare's edge load-balances across them — that's what the connector model is designed for.

**Recommendation.** Introduce `spec.cloudflared.workloadType: deployment | daemonset` on `CloudflareTunnel`. Default to `deployment` with `replicas: 2`. Keep `daemonset` as an opt-in for the LAN/hostNetwork scenario explicitly called out in D16. The CR change is additive (default flips behaviour for new tunnels; existing CRs untouched until they bump `workloadType`). Implementation cost: a small workload-builder switch in `internal/workload/` and one CRD field with CEL validation. Migration story: documented in `NOTES.txt`; the spec already accepts post-MVP workload-type expansion ("DaemonSet only in MVP").

This is the single highest-impact architectural change in this review.

### 2.2 [H] `MaxConcurrentReconciles=1` is the right invariant for Tunnel, the wrong one for the others (challenge to D19)

**D19** specifies `MaxConcurrentReconciles=1` for all MVP controllers. The justification on the Tunnel controller is sound — it is the sole writer of the tunnel-config doc (D11), and leader election plus serial reconciles give the single-writer-per-tunnel-per-cluster guarantee.

The other three controllers — Exposure, AccessPolicy, TunnelRoute — do not share that constraint:

- An `Exposure` reconcile writes its own Access app (one CF resource per Exposure) and its own DNS record (one CF resource per Exposure). Two Exposures for different hostnames are completely independent operations.
- An `AccessPolicy` reconcile writes one CF policy. Two policies are independent.
- A `TunnelRoute` reconcile writes one CF route. Two routes for different CIDRs are independent.

Serialising these throttles startup and large-scale reconciliation pointlessly. A cluster with 200 Exposures will reconcile them one at a time, each waiting on the prior CF round-trip. With the package-level rate limiter at 4 req/s, that's *minutes* of cold-start serialisation per Exposure batch.

**Recommendation.** Raise `MaxConcurrentReconciles` to a sensible default — e.g. `4` — for Exposure, AccessPolicy, and TunnelRoute. Keep Tunnel at `1`. Make it configurable via a flag (`--max-concurrent-reconciles`) so operators can tune for their CF API quota. Update `spec.md` D19 to read "Tunnel controller `MaxConcurrentReconciles=1` (D11 invariant); other controllers default to 4, configurable."

There is no behavioural risk: the per-token rate limiter remains the bottleneck and the central ownership tag scheme prevents cross-reconcile collisions on the CF side.

### 2.3 [M] Drift detection has no defined cadence

The operator reacts to Kubernetes events (CR create/update/delete via Watch) and to leader election. It does *not* periodically resync against Cloudflare state. controller-runtime's default `SyncPeriod` is 10 hours (and randomised); no explicit `Reconciler.SyncPeriod` is set in `cmd/main.go`.

This means: if a Cloudflare dashboard operator (or another tool) mutates a managed resource — flips a DNS record's proxied flag, changes an Access app's policy binding, retags a tunnel route — the operator will not notice until either (a) the K8s CR changes for an unrelated reason, (b) the controller-runtime resync window elapses, or (c) the operator restarts.

For a tool whose contract is "Cloudflare state matches K8s state", that's a gap.

**Recommendation.** Pick one of:

1. **Explicit `RequeueAfter` on every successful reconcile** — e.g. `ctrl.Result{RequeueAfter: 10 * time.Minute}` at the success tail of each reconciler. Cheap, predictable, doesn't fight the rate limiter (10 min × N exposures = comfortable). Simple to revert.
2. **Controller-runtime `SyncPeriod`** — set globally to a value like 30 minutes when the manager is constructed. Affects all controllers uniformly.

I'd take option 1: it's per-controller, so noisy resources (Exposures) can poll more frequently than quiet ones (Tunnels). It also surfaces in metrics directly as reconcile rate, which is useful operationally.

Either way, document the choice in `AGENTS.md ## Reconciliation Rules`.

### 2.4 [M] HTTPRoute discovery at startup is a footgun

`controller.HTTPRouteCRDPresent` runs once in `main.go:129` and the result wires (or doesn't wire) the Exposure controller's HTTPRoute support for the entire process lifetime. If a cluster operator installs Gateway API CRDs *after* the operator boots (which they will, in iterative rollouts), HTTPRoute-source Exposures will continue to fail until the operator pod is restarted.

The current log message — `"HTTPRoute CRD not found, controller disabled"` — surfaces this at boot but not at the point a user actually hits it.

**Recommendation.** Either:

- **Watch the CRD itself** (operator already has `apiextensions.k8s.io/customresourcedefinitions` read RBAC). If the HTTPRoute CRD appears, restart the manager via `mgr.GetCache().WaitForCacheSync` + a sentinel error that the entrypoint loops on. Adds complexity.
- **Document operator restart requirement** in `NOTES.txt` and in `AGENTS.md`. Add a Kubernetes Event on the failing Exposure (`Reason=HTTPRouteSourceDisabled`) so users see the actual cause rather than a generic GVK miss. Minimal cost; preferred unless watch-and-restart is judged important.

Recommend the second option — operators install platform CRDs first in practice, and the cost/benefit doesn't justify hot-reload complexity.

### 2.5 [M] Token retrieval costs an API call per reconcile

`cloudflaretunnel_controller.go:111` calls `cfClient.Tunnels().Token(ctx, cfTunnel.ID)` on every successful reconcile that gets past the tunnel-identity step. The retrieved token is then compared (via checksum) against the in-Secret token to decide whether to roll the DaemonSet.

This is one Cloudflare API call per reconcile for a value that essentially never changes. With drift detection (§2.3) reconciling each tunnel every 10 minutes, that's 144 calls per tunnel per day per process. For a fleet of N tunnels it's 144N — non-trivial against CF rate limits.

Tokens are stable: they only rotate when the user explicitly rotates them via the dashboard or API. The operator does not currently expose a rotate-on-request flow.

**Recommendation.** Cache the token in the existing operator-owned Secret and only refresh when:

1. The Secret is missing or its checksum annotation doesn't match the expected value.
2. The pod-template `cfzt.reid.ee/token-checksum` annotation diverges from the Secret (manual edit).
3. A user-triggered rotation event fires — out of MVP scope; a future `cfzt.reid.ee/rotate-token: "true"` annotation on the `CloudflareTunnel` CR would suffice when needed.

This drops Tunnel reconcile to zero Cloudflare API calls in the steady state. It does require trusting the K8s Secret as the source of truth between operator restarts; that trust is already implicit in everything downstream.

### 2.6 [L] CloudflareTunnel controller does too much; acceptable for MVP

The Tunnel reconciler resolves credentials, reconciles the CF tunnel identity, fetches and writes the token Secret, creates/updates the DaemonSet, checks DaemonSet readiness, builds the ingress doc, PUTs it, and writes status. That's a lot for one reconciler — but each step is small and the linear dependency chain matches the actual dependency order (you can't write the config doc without a tunnel ID; you can't render the connector without a token).

A split — `TunnelIdentityReconciler` for steps 1–3, `ConnectorWorkloadReconciler` for steps 4–5, `TunnelConfigReconciler` for steps 6–7 — would add three reconciler scaffolds for clarity benefits that are marginal at this size. Recommend leaving as-is.

If the spec later grows (post-MVP: multiple connectors per tunnel, different connector workload types, traffic shaping), revisit this split before adding more responsibility to the existing reconciler.

### 2.7 [L] D24 / D25 (managed Access policy and tunnel route CRDs) are correctly modelled

I considered whether D24 (managed `CloudflareAccessPolicy`) is over-engineered for MVP — the simpler alternative is UUID-only references to policies created in the dashboard (the pre-D24 design D3). Two factors make the managed-CRD path correct:

1. **Reuse**. A typical small Cloudflare deployment has 2–4 reusable policies (`family-only`, `engineering`, `public-readonly`) bound to many apps. The CRD lets that reuse live in GitOps.
2. **Drift**. Without a managed CRD, a policy mutated in the dashboard remains divergent from the user's intent silently. The managed CRD path treats policies as desired-state K8s objects, which the operator reconciles.

Same argument for D25 (`CloudflareTunnelRoute`). Keep both.

### 2.8 [L] D2 (operator manages DNS by default) is the right default

The alternative is delegating to external-dns by emitting standard annotations. That would drop the `Zone:DNS:Edit` token scope requirement and remove the zone-resolution / DNS reconcile path from the operator (~250 LoC). But it forces a coexistence dance (which tool writes which record?), adds an operational dependency, and complicates the "everything in GitOps" UX that the project is explicitly chasing per `spec.md`.

Keep D2. The `dns.manage: false` opt-out already exists for users who want external-dns to own DNS.

### 2.9 [L] D11 last-write-wins on tunnel config is acceptable

I considered whether to recommend etag/If-Match semantics on the tunnel-config doc. The Cloudflare configurations endpoint does not expose etags in v4.6.0 of the SDK, so this would require either a PUT-then-GET-and-compare pattern (slow, race-y) or accepting last-write-wins. Combined with D12 (leader election ON) + D19 Tunnel `MaxConcurrentReconciles=1`, the cluster-internal contract holds.

The remaining race — operator + dashboard editing the same tunnel doc simultaneously — is documented under "Drift policy" in `spec.md`. No change recommended.

---

## 3. Resilience and best-practice gaps

These are areas where the codebase is correct but the operational story can be improved.

### 3.1 [H] cloudflared image is pinned to `2025.1.0`

`internal/workload/daemonset.go:15` — `DefaultCloudflaredImage = "cloudflare/cloudflared:2025.1.0"`. Today is 2026-05-27. The pin is ~17 months stale.

Cloudflared has known security and reliability fixes in subsequent releases. The pin is overridable by `spec.cloudflared.image`, but the default applies to every user who doesn't override.

**Recommendation.** Bump to the current cloudflared release (verify via Cloudflare's release notes; do not blindly track `:latest` — the CEL validation already forbids that). Add a CI check that flags when the pinned image is more than 6 months old. Optional: surface in a Kubernetes Event when the operator boots so cluster operators see the version it'll deploy.

### 3.2 [H] Rate-limit retry has a hot-loop hazard on context cancellation

`internal/cloudflare/real.go:129` — `withRetry` uses `time.After(base + jitter - base/4)`. On context cancellation during a long backoff, the select on `ctx.Done()` returns correctly, but the backoff `time.Duration` calculation overflows for `attempt = maxRetries`:

```
base := time.Duration(500<<uint(attempt)) * time.Millisecond  // attempt=5 → 16s
```

That's fine for `maxRetries=5`. But if `maxRetries` is ever raised (the constant is a knob), `500<<uint(attempt)` overflows at `attempt >= 22`. Defensive: clamp `attempt` for the shift, or use `min(attempt, 8)` when computing the base. Cheap fix.

Also: `math/rand` is used for jitter rather than `math/rand/v2`. `math/rand` is now the soft-deprecated package; `v2` is the modern entry. Trivial swap.

### 3.3 [M] Status-update DeepCopy compare uses `equality.Semantic.DeepEqual(before, obj)`

`base.go:69` — the conditional update skips `Status().Update` when the in-memory `before/after` are semantically equal. Correct as far as it goes, but it does not protect against concurrent writers from other reconciles or other process instances modifying status between the `Get` (in the per-CR `setXxxStatus`) and this `Update`. The pattern is:

```
latest = Get(...)        // line setExposureStatus:535
mutate(latest)           // via setReady
Status().Update(latest)  // line setReady:72
```

Between `Get` and `Update` another reconcile can land. controller-runtime returns a conflict; the current call returns it as an error, which causes a re-reconcile via workqueue backoff. That's correct behaviour, but the resulting log noise can be loud during transient hot paths.

**Recommendation.** Use Server-Side Apply for status writes:

```go
patch := &cfztv1alpha1.CloudflareExposure{
    TypeMeta:   exposure.TypeMeta,
    ObjectMeta: metav1.ObjectMeta{Name: exposure.Name, Namespace: exposure.Namespace},
    Status:     desiredStatus,
}
return r.Status().Patch(ctx, patch, client.Apply, client.FieldOwner("cfzt-operator"), client.ForceOwnership)
```

SSA is the canonical pattern for operators in controller-runtime 0.20+. Drops the Get-then-Update conflict window and reads cleaner. Cost: rewrite the four `setXxxStatus` helpers; ~80 LoC delta. Benefit: status writes never conflict; logs get quieter; the existing `setReady` helper can become a one-liner builder of the conditions slice rather than a stateful mutator.

### 3.4 [M] Error-class to reconcile-result mapping is inconsistent

Reviewed how each error type drives requeue behaviour:

| Error | Current handling | Effect |
|---|---|---|
| `errHostnameConflict` | `setExposureStatusAndRequeue` → `RequeueAfter: 30s` | Fixed-interval retry |
| `errForeignResource` | `setExposureStatusAndRequeue` → 30s | Fixed-interval retry |
| `errPolicyNotFound` | `setExposureStatus`, no requeue | Wait for Watch on AccessPolicy |
| `errPolicyNotReady` | `setExposureStatus` + `RequeueAfter: 30s` | Both Watch AND fixed |
| DNS write failure | `setExposureStatusAndBackoff` → returns `fmt.Errorf` | Exponential backoff via controller-runtime |
| Credentials missing (tunnel) | `setTunnelStatus` + `RequeueAfter: 30s` | Fixed-interval retry |
| Cloudflare 5xx (real.go) | retried inside `withRetry` (up to 5 attempts) | SDK-level retry |
| Token fetch failed | returns `fmt.Errorf` | Exponential backoff |

This is not wrong, but it's ad-hoc. The reasoning for "this one waits 30s, this one backs off exponentially" is not stated anywhere. Operationally it makes the operator's behaviour under failure hard to predict.

**Recommendation.** Define a small classification table in `AGENTS.md ## Reconciliation Rules` and codify it in a helper:

```
TransientFailure  → return err (controller-runtime backoff)
StateConflict     → RequeueAfter: 30s
WaitingForObject  → no requeue (rely on Watch)
PermanentFailure  → no requeue, status Ready=False, no error
```

Each error type gets a single classification. The helper accepts `errorClass + cf-status-mutate + reason + message` and returns the right `ctrl.Result` shape. ~50 LoC of consolidation; large clarity gain.

### 3.5 [M] No circuit-breaker on Cloudflare API failures

The rate limiter prevents *flooding* Cloudflare with calls, but if the CF API is broadly degraded (region issue, account suspended, token revoked), every reconciler will repeatedly hit the failing endpoint inside `withRetry`'s 5-attempt loop, consume the token bucket, and slow other reconciles.

**Recommendation.** Add a per-credential circuit breaker (open after N consecutive failures; half-open after M seconds). The standard `golang.org/x/sync` package doesn't provide one; `github.com/sony/gobreaker` is the well-known small library (~300 LoC, zero deps). Optional: write it yourself in `internal/cloudflare/circuit.go` — it's about 80 lines.

Lower priority than items above; relevant when the operator is run against larger fleets where one broken account shouldn't impair others.

### 3.6 [M] No structured metrics on Cloudflare API calls

`spec.md ## Observability` defines four metrics including `cfzt_cloudflare_api_total{endpoint, status}` and `cfzt_cloudflare_api_duration_seconds{endpoint}`. I see no implementation in `internal/cloudflare/real.go`. Without these, operators cannot diagnose:

- whether the operator is rate-limited,
- which endpoints dominate API call budget,
- which endpoints fail most often,
- 95p latency to Cloudflare.

**Recommendation.** Wrap `withRetry` with Prometheus `prometheus.HistogramVec` + `CounterVec` keyed by endpoint name and status class. Use the controller-runtime registered metrics registry (`ctrlmetrics.Registry`). Each sub-interface method becomes a 3-line wrapper: `start := time.Now(); defer record(...)`. ~80 LoC total. Pair with a `make metrics-sample` target that dumps a curl of `/metrics` from a `make run` instance.

### 3.7 [M] No periodic leader-lease sanity check

Leader election (D12) uses controller-runtime's default. The lease will normally re-acquire on the same pod across restarts; on a brief network partition between the leader and the API server, the leader may step down and another replica may pick up. Standard pattern.

What's missing is observability: `cfzt_leader{namespace, pod}` gauge to make leadership transitions visible. Without it, "why did reconciles pause for 30 seconds at 14:03" requires reading manager logs after the fact.

**Recommendation.** Emit a leader-election Event on acquire/lose, plus a gauge. controller-runtime provides hooks for this via `LeaderElectionResourceLockInterface`. Small.

### 3.8 [L] Cloudflare-side route comment length budget is fragile

`internal/controller/cloudflaretunnelroute_controller.go:300` builds `routeOwnershipComment`:

```
prefix := ownership.From(route.UID).CompactComment()    // "managed-by=cfzt source-uid=<36>"
return prefix + " | " + route.Spec.Comment              // user text up to 34 chars
```

`spec.md ## Naming conventions` claims the total fits Cloudflare's 100-char route comment limit. Counting:

- `"managed-by=cfzt source-uid="` = 27 chars
- UID = 36 chars
- ` | ` = 3 chars
- User comment up to 34 chars

Total = 100 chars exactly. There is no margin and no test that exercises the maximum-length case. A future change to the compact format (e.g. adding a version byte) overflows silently.

**Recommendation.** Add a unit test in `internal/ownership/` that asserts `MaxCommentLength` is honoured for max-length user comments. Make `MaxCommentLength` a package constant rather than implicit. Document the budget at the call site.

### 3.9 [L] `gen-config` lint feedback loop is good; suppression list grows fast

`.golangci.yml` suppresses `dupl` for `internal/*`. Prior review §3.7 noted this. Now that the shared `Base` and ownership packages exist, drop the suppression and let the linter prevent regression. Same for `gocyclo` once `cmd/main.go` is under the threshold (it is now).

### 3.10 [L] No NetworkPolicy templates in the Helm chart

The operator pod itself reaches the CF API via egress. The cloudflared pods reach origin services in cluster. No `NetworkPolicy` is shipped. For clusters with default-deny baselines, both pods will silently lose connectivity.

**Recommendation.** Add opt-in `networkPolicy.enabled` value (default `false`) that, when set, ships two policies:

- Operator pod: egress to API server + `api.cloudflare.com:443` + cluster DNS.
- cloudflared pod: egress to `*.argotunnel.com:7844` (or the configured CF edge), cluster DNS, and the configured origins (best-effort — origins are arbitrary).

Documented as "enable if you run a default-deny baseline".

---

## 4. Dependency burden

Full audit of direct requires plus material indirect ones.

### 4.1 Direct deps

| Module | Used by | Verdict |
|---|---|---|
| `cloudflare-go/v4` | `internal/cloudflare/real.go` (5 files via subpackages) | Required. No alternative. Wrap is tight (single package). |
| `google/uuid` | `internal/cloudflare/fake.go` only | **Replace.** See §4.3. |
| `onsi/ginkgo/v2`, `onsi/gomega` | Test code only | Required for envtest. Standard. |
| `golang.org/x/time` | `internal/cloudflare/real.go` (rate.Limiter) | Required. Stdlib has no token bucket. |
| `k8s.io/api`, `apimachinery`, `client-go` | Everywhere | Required. Core. |
| `sigs.k8s.io/controller-runtime` | Everywhere | Required. Core. |

No dependency in this list is bloat. The operator's dep graph is appropriately minimal for what it does.

### 4.2 Indirect deps worth flagging

- `tidwall/gjson`, `tidwall/sjson`, `tidwall/pretty` — pulled by `cloudflare-go/v4` for its internal JSON manipulation. Stable, small. Out of our control.
- `prometheus/client_golang` + `client_model` + `common` + `procfs` — pulled by controller-runtime metrics. Required.
- `go.opentelemetry.io/*` — pulled by controller-runtime + grpc. Not directly used. Cost is binary size; can't easily strip.
- `cel.dev/expr` + `google/cel-go` — pulled by apiextensions for CRD CEL validation. Required since D22.

None of these can be removed without giving up framework benefits we want. Accept.

### 4.3 [M] Replace `google/uuid` with stdlib

`fake.go` uses `uuid.New().String()` in seven places to mint synthetic IDs for fake-Cloudflare resources. The dependency is small but it ships in the production binary because `fake.go` lives in package `cloudflare` (not `cloudflare_test`). Replacing it costs ~5 lines:

```go
import "crypto/rand"

func randID() string {
    var b [16]byte
    _, _ = rand.Read(b[:])
    b[6] = (b[6] & 0x0f) | 0x40   // version 4
    b[8] = (b[8] & 0x3f) | 0x80   // RFC 4122 variant
    return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
```

Drops one direct require and the transitive `google/uuid` from go.sum. Smaller change than it looks; `fake.go` is the only call site.

(Separately: consider moving `fake.go` to `fake/` subpackage so it's never compiled into the manager binary in production. Today it's a constant in the dep graph even though no controller imports it; the Go compiler will dead-code-eliminate the functions but not their type definitions and not the `google/uuid` package init. The split costs about an afternoon of test file updates and shaves a small fraction of binary size — low priority but worth doing once.)

### 4.4 [L] Go version pin

`go.mod` specifies `go 1.25.7`. Recent. Fine.

`Dockerfile` builds with `golang:1.25` (minor-version-only tag, line 2). Mismatch with `go.mod`'s patch level. CI will use whatever 1.25.x is current at build time. Recommend pinning to the specific patch (`golang:1.25.7`) so reproducible builds work.

---

## 5. Code-level findings (delta from prior review)

Items the prior review did not surface or that surface differently now.

### 5.1 [M] `accessNewBody` / `accessUpdateBody` duplication remains

`real.go:710` and `real.go:727` — 95% duplicate functions, returning two different SDK union types. Prior review §1.14 noted these are "structurally hard to unify because the SDK's union shape diverges". A `commonAccessFields` helper already exists between them (line 752). Take the next step: collapse into one private constructor that takes a callback for the union-typed body builder:

```go
func selfHostedBody[B any](in AccessApplicationInput, build func(accessCommonFields) B) B {
    return build(commonAccessFields(in))
}
```

Marginal LoC saving but eliminates the "did we update both?" hazard. Optional; skip if the function shapes drift further as Cloudflare SDK evolves.

### 5.2 [M] `Makefile manifests` target still generates webhook configs

`Makefile:48`:
```
"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..."
```

`webhook` produces `config/webhook/manifests.yaml`. No `+kubebuilder:webhook:` markers exist; the file is empty boilerplate. Drop `webhook` from the controller-gen invocation. Removes one stale path.

### 5.3 [M] `Configurations.Update` race on shared config doc not surfaced in metrics

The skip-when-hash-matches optimisation (now landed) is correct. What's missing is observability: if two operator pods race during a leader transition and both write before the new leader stabilises, the second write loses. With D12 + D19 this is "should never happen", but a counter for `cfzt_tunnel_config_writes_skipped_total{tunnel}` and `cfzt_tunnel_config_writes_total{tunnel, result}` would make the invariant observable.

### 5.4 [M] `desiredCloudflareTunnelName` collision footgun

`cloudflaretunnel_controller.go:347`:
```
return fmt.Sprintf("%s-cfzt-%x", tunnel.Spec.TunnelName, sum[:4])
```

`sha256[:4]` = 8 hex chars = 4 bytes = 32 bits of entropy. Birthday collision becomes non-negligible (~1%) at ~9,000 tunnels per cluster. That's a lot — but a deterministic-name collision between two `CloudflareTunnel` CRs would be a silent data corruption event. Bump to `sum[:8]` (16 hex chars, 64 bits) at the cost of an 8-char-longer name. Plenty of headroom in the 120-char tunnel-name budget. One-line change. Out of caution.

### 5.5 [L] `PolicyName` with spaces bypasses `-cfzt` collision protection

Prior review §7 flagged this. Still open. `desiredPolicyName`:

```go
base := policy.Name
if policy.Spec.PolicyName != "" {
    base = policy.Spec.PolicyName
}
return base + "-cfzt"
```

If a user sets `spec.policyName: "Engineering Team Access"`, the resulting CF name is `Engineering Team Access-cfzt`. That works (CF allows spaces in policy names) but the suffix is no longer doing collision-protection work — a hand-created `Engineering Team Access-cfzt` policy in the dashboard would collide. The collision-protection contract assumes a charset that hand-creation does not naturally produce. With spaces allowed, that assumption breaks.

**Recommendation.** Either:

- **Hash the policy name** instead: `<sanitized-name>-cfzt-<hash4(uid)>`, matching the tunnel pattern. Bulletproof.
- Or **forbid spaces** in `spec.policyName` via the CEL pattern, leaving the suffix-only collision protection.

I'd take the first option — it matches the tunnel pattern already documented and removes the "must not collide with hand-created names" assumption that the suffix can't fully enforce.

### 5.6 [L] `image` CEL pattern rejects valid registry-with-port

Also surfaced in prior review §7. Pattern is `^[a-z0-9./-]+(:[a-zA-Z0-9._-]+)?$`. `registry.local:5000/cloudflared:2025.1.0` has two colons; the regex matches only the first as the tag separator. Rewrite as:

```
^([a-z0-9.-]+(:[0-9]+)?/)?[a-z0-9.-]+(/[a-z0-9.-]+)*(:[a-zA-Z0-9._-]+)?(@sha256:[a-f0-9]{64})?$
```

…and add positive/negative test cases. Or use a Go `regexp.MustCompile` in the controller and do the strict check there, leaving CEL with a coarser surface check.

### 5.7 [L] `setExposureStatusAndBackoff` shape returns an error to drive backoff

`cloudflareexposure_controller.go:546`:

```go
return fmt.Errorf("%s: %s", reason, message)
```

The pattern works — controller-runtime backs off on error returns — but the error has no `errors.Is` target and no underlying-error chain. Logs read `"DNSWriteFailed: <message>"` with no way to distinguish "DNS quota exceeded" from "auth failure" from upstream code without string-matching. Wrap with a typed error (`type ReconcileError struct { Class string; Reason string; Cause error }`) so callers can `errors.As` and so metrics can label by error class.

### 5.8 [L] `ensureCloudflaredStopped` reads pods to detect lingering pods

`cloudflaretunnel_controller.go:230`. On Tunnel deletion the controller deletes the DaemonSet, then lists pods matching the tunnel's label selector to confirm they're gone before deleting the CF tunnel. The pod list ensures the connector is gone before the tunnel ID is released — correct ordering, prevents a brief "tunnel deleted but cloudflared still trying to dial it" window.

However, the read is unindexed (label selector List). For clusters with many pods, this is O(N) on every Tunnel deletion poll. Add a field-index or rely on the DaemonSet's `status.numberAvailable == 0` instead. The DaemonSet readiness was already trustworthy enough to gate Ready; trust it on the way out too.

### 5.9 [L] `Status.IngressDocHash` is computed from JSON marshal

`cloudflaretunnel_controller.go:469`:

```go
data, err := json.Marshal(config)
```

`json.Marshal` of a Go struct is *not* deterministic for maps — Go 1.12+ orders map keys, so it's actually fine here as long as the struct contains no maps. `cloudflare.TunnelConfiguration` is a slice-of-struct: deterministic. Worth a comment so a future contributor doesn't add a map field and break the hash invariant silently.

### 5.10 [L] `cover.out` still committed

Prior review §2 last surviving open item. Add to `.gitignore`; `git rm` the file.

---

## 6. CRD / API hygiene (delta)

### 6.1 [M] Status writes use `latest.Status = ...` mutation pattern

The four `setXxxStatus` helpers all re-Get the CR and mutate `latest.Status`. This is a fine pattern for `Status().Update` but not for SSA (§3.3). When the SSA migration happens, the helpers can simplify: build a `Status` value directly from inputs and apply, no Get needed.

### 6.2 [L] Generation-aware condition observedGeneration is tracked correctly

`base.go:63` — `setCondition(..., generation)`. Conditions carry `observedGeneration`. Verified the policy-resolution path in `resolveAccessPolicyUUID` checks `ready.ObservedGeneration != policy.Generation` to detect stale Ready. Good practice; rare in operators of this size.

---

## 7. Tests

The prior review noted ~5.1k test LoC for ~5.6k prod. That ratio has held. Specific notes:

- **CEL validation tests** (`*_validation_test.go`) are still per-CRD. Consolidating into one `apply_test.go` is still a win (prior review §6) — ~600 LoC drops to ~300. Not urgent.
- **`test/live/cloudflare_smoke_test.go`** — verify line count; harness extraction recommendation from prior review may have landed.
- **No fuzz tests** on the ownership tag parser, network CIDR canonicaliser, or tunnelconfig hash. Easy wins:
  - `FuzzOwnershipParseComment(uid types.UID)` — round-trip render/parse.
  - `FuzzCanonicalNetwork(network string)` — `netip.ParsePrefix`; reject non-canonical without panic.
  - `FuzzTunnelconfigBuild(...)` — sort-stability invariant under shuffled inputs.

Add fuzz tests if planning to ship 1.0.

---

## 8. Recommendations, prioritised

Each item is tagged with effort (S = ≤1 day, M = 2–4 days, L = >4 days) and risk (low/med/high). All are net-positive; the prioritisation is by impact-per-effort.

### Tier 1 — ship before 1.0

| # | Item | Effort | Risk | Why |
|---|---|---|---|---|
| 1 | §3.1 Bump cloudflared image pin | S | Low | Stale by 17 months; security-relevant default. |
| 2 | §2.5 Cache token; skip per-reconcile API call | S | Low | Cuts steady-state API call rate by ~50% on tunnel reconciles. |
| 3 | §2.2 Raise `MaxConcurrentReconciles` for non-Tunnel controllers | S | Low | Throughput unblock; one constant per controller. |
| 4 | §2.3 Explicit drift-detection cadence | S | Low | Closes the silent-drift gap. |
| 5 | §3.6 Cloudflare API metrics | M | Low | Operational visibility; spec promised them. |
| 6 | §5.10 Remove `cover.out`; gitignore | S | Low | Hygiene. |
| 7 | §4.3 Replace `google/uuid` with stdlib | S | Low | Drops a direct dep. |

### Tier 2 — meaningfully better, plan a follow-up release

| # | Item | Effort | Risk | Why |
|---|---|---|---|---|
| 8 | §2.1 Deployment-default for cloudflared workload | M | Med | Right default; users will hit DaemonSet limits eventually. |
| 9 | §3.3 SSA for status writes | M | Med | Cleaner conflict story; canonical pattern. |
| 10 | §3.4 Reconcile error classification table | M | Low | Operational predictability. |
| 11 | §5.4 Wider tunnel-name hash | S | Low | Collision headroom; one-line + migration note. |
| 12 | §5.5 Policy-name collision protection via hash | M | Med | Bulletproof the `-cfzt` contract. |
| 13 | §3.10 Opt-in NetworkPolicy templates | M | Low | Default-deny ecosystems. |
| 14 | §3.5 Circuit breaker on Cloudflare API | M | Low | Fault-isolation in larger fleets. |

### Tier 3 — opportunistic

| # | Item | Effort | Risk |
|---|---|---|---|
| 15 | §5.2 Drop `webhook` from controller-gen | S | Low |
| 16 | §5.7 Typed reconcile errors | M | Low |
| 17 | §5.8 Use DaemonSet status to gate Tunnel deletion | S | Low |
| 18 | §3.8 Ownership comment max-length constant + test | S | Low |
| 19 | §3.9 Drop `dupl` / `gocyclo` lint suppressions | S | Low |
| 20 | §3.7 Leader-election Events + gauge | S | Low |
| 21 | §3.2 Defensive clamp on `withRetry` shift; swap to `math/rand/v2` | S | Low |
| 22 | §2.4 Document HTTPRoute discovery + add Event | S | Low |
| 23 | §5.6 Image-pattern regex fix | S | Low |
| 24 | §5.1 Collapse `accessNewBody`/`accessUpdateBody` | S | Low |
| 25 | §4.4 Pin `golang:1.25.7` in Dockerfile | S | Low |

### Things I considered and recommend *not* changing

- D11 last-write-wins on tunnel config doc. Safe under D12 + D19.
- D7 → DaemonSet *only*: the rejection is in §2.1; keep DaemonSet as an option, not the default.
- D2 operator-managed DNS by default. Coexistence story with external-dns is cleaner this way.
- D6 / D15 / D17 / D18 / D20–D25. Each carries a specific decision that holds up.
- The 8-sub-interface Cloudflare client surface. Acceptable for the breadth of CF API touched.
- Tunnel reconciler combining identity + connector + config writes. Split adds complexity; current shape is tractable.
- Helm chart shape and OCI distribution.
- HTTPRoute discovery via `unstructured` to avoid the gateway-api/v1 dep.

---

## 9. Implementation plan

The plan is structured as four bundled slices, each shippable as a single release. The grouping minimises cross-cutting churn and lets each slice be reviewed and merged independently.

### Slice A — Tier 1 hygiene & throughput (one PR per item, ~1 week total)

Goal: ship before 1.0. Zero risk; obvious wins.

1. **A1: Bump cloudflared image pin** to current release. Update `internal/workload/daemonset.go:15`, add release-note. Add CI check: `make check-cloudflared-pin` greps the pinned version against `cloudflare/cloudflared` GitHub release feed and warns if older than 6 months.
2. **A2: Cache token; skip per-reconcile API call.** In `cloudflaretunnel_controller.go:111`, gate `cfClient.Tunnels().Token` on Secret-missing + checksum-mismatch. Add envtest `TestTunnelTokenCachedReuseAfterReconcile`. Refresh path stays for the user-rotation case.
3. **A3: Raise `MaxConcurrentReconciles` for Exposure / AccessPolicy / TunnelRoute.** Default `4` (constant). Add `--max-concurrent-reconciles` flag in `main.go` for operator-tunable override. Update D19 in `spec.md` and `AGENTS.md`. envtest already exercises parallel-write paths; verify nothing breaks.
4. **A4: Explicit drift-detection cadence.** Add `RequeueAfter: 10 * time.Minute` at the success tail of each reconciler. Configurable via `--drift-interval` flag, default 10m. Add envtest `TestReconcileRequeueAfter`.
5. **A5: Cloudflare API metrics.** Add `internal/cloudflare/metrics.go` wiring `prometheus.HistogramVec` + `CounterVec` to `ctrlmetrics.Registry`. Wrap `withRetry` with a per-endpoint label. Update `spec.md ## Observability` if metric names diverge from spec.
6. **A6: `cover.out` purge.** `git rm cover.out`; append to `.gitignore`.
7. **A7: Replace `google/uuid` with stdlib UUIDv4 in `fake.go`.** Update `go.mod`. (Optional follow-up: move `fake.go` to `internal/cloudflare/fake/` subpackage.)

End-of-slice gate: `make test` + `make helm-lint` + `live-smoke.yaml` clean.

### Slice B — Tier 2 architectural alignment (~2 weeks)

Goal: align defaults and patterns with production reality.

1. **B1: cloudflared Deployment workload type (D7 update).** Introduce `spec.cloudflared.workloadType: deployment | daemonset` on `CloudflareTunnel`, default `deployment`, with CEL validation. Implement `internal/workload/deployment.go` next to `daemonset.go`. Tunnel reconciler dispatches on `workloadType`. Deployment defaults to `replicas: 2`, configurable via `spec.cloudflared.replicas` (with CEL `min=1, max=10`). Update `spec.md` D7 to add `workloadType` and note the default change. Update `NOTES.txt` with migration guidance. envtest coverage: `TestTunnelDeploymentWorkload`, `TestTunnelDaemonSetWorkload`, `TestTunnelWorkloadTypeMigration`.
2. **B2: SSA for status writes.** Rewrite the four `setXxxStatus` helpers to use `client.Apply` patches with `client.FieldOwner("cfzt-operator")`. Single helper builder in `internal/controller/status.go`. Drop the per-reconciler `setReady` indirection. Add envtest `TestStatusSSANoConflict` simulating two-reconciler races.
3. **B3: Reconcile error classification.** Add `internal/controller/reconcile_errors.go` with `TransientError`, `StateConflictError`, `WaitingForObjectError`, `PermanentError` types and a single `resultFor(err)` helper. Migrate all `setXxxStatusAndRequeue` / `setXxxStatusAndBackoff` call sites. Update `AGENTS.md`.
4. **B4: Tunnel-name hash widening.** `desiredCloudflareTunnelName` uses `sum[:8]` (16 hex chars). Document migration: existing tunnels keep their 8-char-suffix name until the CR's UID changes; new tunnels get the wider suffix. The legacy match in `reconcileCloudflareTunnel:332` already handles the migration.
5. **B5: Policy-name collision via hash.** Update `desiredPolicyName` to `<sanitized-base>-cfzt-<hash4(uid)>`. Add legacy-name recovery (matching B4 pattern): if an existing CF policy with the pre-hash name is found and exact rules match, rename it in place.

End-of-slice gate: `make test`; live smoke includes a Deployment-workload-type test; chart upgrade test on a kind cluster verifying existing DaemonSet-mode tunnels keep working.

### Slice C — Tier 2 operational depth (~1 week)

Goal: production-readiness polish.

1. **C1: Opt-in NetworkPolicy chart templates.** Add `templates/networkpolicy-operator.yaml` and `templates/networkpolicy-cloudflared.yaml`, gated on `networkPolicy.enabled: true`. Document the egress list.
2. **C2: Circuit breaker per Cloudflare credential.** Implement `internal/cloudflare/circuit.go` (~80 LoC). Wrap `withRetry`. Expose metrics `cfzt_cloudflare_circuit_state{credential, state}`. Default thresholds tuned by integration test.
3. **C3: Leader-election Events + gauge.** Implement via controller-runtime's `LeaderElectionResourceLockInterface` hooks. Add envtest checking emission.

### Slice D — Tier 3 cleanup (~3 days, opportunistic)

Each item is independent; pick up as the area is next touched.

1. **D1: Drop `webhook` from `make manifests`.**
2. **D2: Typed reconcile errors with `errors.As` support.**
3. **D3: Use DaemonSet/Deployment `status` to gate Tunnel deletion (replace pod-list scan).**
4. **D4: `MaxCommentLength` constant + boundary test in `internal/ownership/`.**
5. **D5: Drop `dupl` and `gocyclo` lint suppressions.**
6. **D6: Defensive shift clamp + `math/rand/v2` in `withRetry`.**
7. **D7: Document HTTPRoute discovery startup-only behaviour; emit Event on disabled-source Exposures.**
8. **D8: Fix image regex to allow registry-port in `image` CEL.**
9. **D9: Collapse `accessNewBody` / `accessUpdateBody` via a small generic.**
10. **D10: Pin `golang:1.25.7` in Dockerfile.**
11. **D11: Add ownership / canonical-network / tunnelconfig fuzz tests.**

### Out-of-scope for any slice

The "things I considered and recommend not changing" list stays as-is.

---

## 10. Open questions for the maintainer

Before scheduling Slice A:

1. **Deployment default (B1).** Is the homelab/LAN scenario expected to remain the primary deployment shape? If yes, the DaemonSet default is correct and B1 should be downgraded to "DaemonSet remains default; Deployment is an opt-in workload type." If the operator targets a broader audience, the flip is justified. This shapes the migration story.
2. **Drift cadence (A4).** Is 10 minutes the right default? Some operators tune this aggressively (1m) for tight conformance; others (30m) for quieter API calls. I'd default to 10 minutes and document the trade-off.
3. **MaxConcurrentReconciles default for non-Tunnel (A3).** Default `4` is a guess. For homelab clusters with one tunnel and a handful of Exposures, `1` is fine; the value matters under larger fleets. Suggestion: ship `4`, allow override via flag, document trade-off.
4. **SSA migration (B2).** Touches every controller. Worth gating on a successful Slice A so the bar to revert is clean.
5. **Tunnel-name hash widening (B4).** Existing tunnels created with `sum[:4]` keep their name and ID. New tunnels widen. Acceptable, or do you want a one-shot rename for existing ones too?

Happy to extend or revise once these are settled.
