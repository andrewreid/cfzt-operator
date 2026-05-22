# cfzt-operator implementation plan

## 1. Context

Operational plan for shipping the cfzt-operator MVP in five slices (Tunnel/connector → Exposure → sourceRef derivation → managed Access policies → private network CIDR routes). Source of truth for architecture, decisions D1–D25, CRD shapes, RBAC, and DoD lists is `spec.md`. Operating handbook (bootstrap, commands, delegation, code rules) is `AGENTS.md`. This plan turns those into ordered, single-session subtasks with cited spec sections and test names.

Slice 4 (`CloudflareAccessPolicy` CRD, D24) added under `## 3. Slice plan` after Slice 3 ships — managed Access policies are now in scope per `spec.md ## Decisions` D24.

Slice 5 (`CloudflareTunnelRoute` CRD, D25) added under `## 3. Slice plan` after Slice 4 ships — private network CIDR routes are now in scope per `spec.md ## Decisions` D25.

Not covered: post-MVP work (annotation UX, Ingress source, WARP, Gateway, OLM, multi-cluster, additional Access rule types beyond Slice 4 subset). Decisions are not re-derived here — see `spec.md ## Decisions`.

## 2. Current state

Bootstrap (section 4) complete on 2026-05-19. Kubebuilder scaffold present:
`api/v1alpha1` types for `CloudflareTunnel` (cluster-scoped per D6) and
`CloudflareExposure` (namespaced); controllers wired in `cmd/main.go` with
leader-election ON (D12), `MaxConcurrentReconciles=1` on both (D19), and a
compile-time-disabled HTTPRoute discovery placeholder for Slice 3. Helm chart
hand-written at `charts/cfzt-operator/` (D17) with NOTES.txt GitOps caveat
(D23). CI workflows in `.github/workflows/ci.yaml` + `release.yaml` (D18).
Makefile `test` target runs envtest via `setup-envtest`; `helm-package` +
`helm-sync-crds` targets added. Chart installs always pass
`--leader-elect=true`; D12 is not exposed as a Helm value.

Completed on 2026-05-19:
- Subtask 1: `CloudflareTunnel` types + CRD validation markers. API amended
  so credentials Secret namespace is derived from `spec.cloudflared.namespace`
  instead of duplicated under `credentialsSecretRef`; image `:latest`
  validation uses bounded `endsWith` CEL. The CRD post-processing helper was
  removed. `make test` is green.
- Subtask 2: `internal/cloudflare` interface + fake (client.go, tunnels.go,
  real.go, fake.go, fake_test.go). cloudflare-go/v4 v4.6.0 added. SDK does
  not surface tunnel `comment` field — D9 amended (see spec) to track
  ownership via `status.tunnelId` rather than CF-side tag. Real client uses a
  shared per-token rate limiter keyed by API token hash.
- Subtask 3: `internal/naming/` names + ownership tag helpers.
- Subtasks 4–8: `internal/workload` token Secret + DaemonSet builders,
  pinned cloudflared image const, token checksum rollout annotation, tunnel
  reconciler for credential resolution / ID-based D9 tunnel create-adopt /
  token Secret upsert / DaemonSet upsert, token rotation, finalizer cleanup,
  `Ready` + `Progressing` conditions, and events (`CreatedTunnel`,
  `TokenRotated`). RBAC markers regenerated for Secrets, DaemonSets, Events.
  Tunnel-owned Secrets and DaemonSets have ownerReferences and are watched by
  the Tunnel controller. DaemonSet builder sets `ClusterFirstWithHostNet` when
  `hostNetwork: true`.
  Tests added for workload builders and tunnel create/adopt/foreign
  refusal/token rotation/finalizer/condition transitions.
- Subtask 9: Helm CRDs synced from `config/crd/bases` into
  `charts/cfzt-operator/crds/`; chart lint and template render pass; NOTES
  credential example now includes both `accountId` and `apiToken`;
  `make test` is green.

Completed on 2026-05-19:
- Slice 2 subtasks 1-10: `CloudflareExposure` API + CRD validation, deterministic
  `internal/tunnelconfig` builder, Cloudflare configurations / Access / DNS /
  zones interfaces with fake + real SDK wiring, Tunnel-owned tunnel-config doc
  writes and route hashes, Exposure Access/DNS/status/finalizer paths, D20
  cross-controller watches, hostname-conflict and foreign-resource guards,
  tunnel `BlockedByExposures`, external-origin coverage, regenerated CRDs,
  and Helm CRD sync. Tests added for CRD validation, fake CF resources, builder
  determinism/collisions/empty doc, Exposure create / DNS off / Access off /
  hostname conflict / foreign resource / finalizer / Ready gating, D20 maps,
  Tunnel config writes, and Tunnel blocked deletion.

Completed on 2026-05-19:
- Slice 3 subtasks 1-6: Service `sourceRef` origin defaulting, same-namespace
  ownerReference wiring for GC cascade, HTTPRoute hostname derivation via
  unstructured Gateway API reads, startup-time HTTPRoute CRD discovery with
  disabled log line, CRD validation relaxation for Service no-origin and
  HTTPRoute no-hostname cases, and RBAC/Helm updates for Services,
  HTTPRoutes, and CRD discovery. Tests added for single-port Service defaulting,
  multi-port rejection, source deletion cleanup path, HTTPRoute hostname
  derivation, CRD-absent discovery, and Slice 3 CRD validation relaxation.

Completed on 2026-05-20:
- Slice 4 subtask 3: `internal/cloudflare/access_policies.go` adds
  `AccessPolicies` interface + value types (`AccessPolicy`,
  `AccessPolicyInput`, package-local `AccessRule` discriminated union)
  with List/Get/Create/Update/Delete. `Client` interface extended.
  `FakeClient` gains `accessPolicies` map + deep-copying
  `fakeAccessPolicies`. `RealClient` adds `realAccessPolicies` wrapping
  every call in `withRetry`; 404 → `ErrNotFound`; boundary `toDecision`
  validates the four MVP decision values; `toAccessRuleParams` /
  `fromAccessRules` translate the six MVP rule variants via the SDK's
  `AsUnion()` accessor (unknown variants on response silently skipped).
  `AccessPolicyListParams` has no name field — `List` returns all and
  the controller filters by name (spec.md SDK mapping note).
  cloudflare-go/v4 `AccessPolicy*` types carry no comment/tag field,
  so ownership tracking is ID-only (mirrors D9 tunnel pattern;
  spec.md:506). Tests `TestFakeAccessPolicyCreateGetDelete`,
  `TestFakeAccessPolicyListByName`,
  `TestFakeAccessPolicyUpdateRulesIdempotent` cover round-trip,
  client-side name filtering, and update idempotence. `make test`
  green.
- Slice 4 subtask 2: `CloudflareExposure.spec.access.policyRef.name` added
  (RFC 1123 subdomain pattern, maxLength 253) and the access CEL rule
  rewritten to require exactly one of `policyRef.uuid` or `policyRef.name`
  when `access.enabled: true`. `TestExposurePolicyRefOneOfValidation`
  covers uuid-alone, name-alone, both-set reject, neither-set-when-enabled
  reject. Existing "requires policy UUID" test message string updated to
  match the new CEL message. `make test` green.
- Slice 4 subtask 1: `CloudflareAccessPolicy` cluster-scoped types +
  CRD validation markers (`api/v1alpha1/cloudflareaccesspolicy_types.go`).
  CRD generated at `config/crd/bases/cfzt.reid.ee_cloudflareaccesspolicies.yaml`.
  Two spec deviations vs. plan, both forced by CRD machinery:
  (a) `spec.rules.{include,exclude,require}` lists are not `omitempty` and
  carry `+kubebuilder:default={}` — required for the CEL `size(self.rules.*)`
  rule to evaluate when callers omit the field; (b) `MaxItems=64` on each
  rule list plus `MaxLength` bounds on `AccessRule` string fields
  (Email 320, EmailDomain 253, IP 43, ServiceToken 36, GeoCountryCode 2)
  added to keep the per-rule discriminated-union CEL inside the cost
  estimator budget. `TestCloudflareAccessPolicyCRDValidation` covers valid
  manifest, decision-enum reject, two-field-rule reject, zero-field-rule
  reject, empty-rules reject, bad `sessionDuration`, missing
  `credentialsSecretRef.namespace`, missing `decision`. `make test`
  green.
- Slice 4 subtask 4: `CloudflareAccessPolicy` controller core added
  (`internal/controller/cloudflareaccesspolicy_controller.go`) and wired in
  `cmd/main.go` with `MaxConcurrentReconciles=1`. Reconcile resolves
  cluster-scoped credentials, creates a CF Access policy when no local
  `status.policyId` exists, refuses name-colliding policies with
  `Reason=ForeignPolicy`, verifies status-ID name matches before reporting
  ready, and guards finalizer delete against stale/foreign status IDs by
  reading the policy first. RBAC markers were regenerated and deploy surfaces
  were kept coherent now that the manager watches the new kind:
  `config/crd/kustomization.yaml` includes the CFAP CRD,
  `charts/cfzt-operator/crds/cloudflareaccesspolicy.yaml` is synced, Helm
  ClusterRole includes CFAP verbs, and stale chart `CloudflareExposure` CRD
  policyRef.name drift was synced. Tests added/expanded:
  `TestAccessPolicyCreate`, `TestAccessPolicyCreateUsesSpecPolicyName`,
  `TestAccessPolicyForeignRefuses`, `TestAccessPolicyStaleStatusIDRecreates`,
  `TestAccessPolicyForeignStatusIDRefuses`,
  `TestAccessPolicyFinalizerBlocksForeignStatusID`. `make test`,
  `go test ./...`, and `helm lint charts/cfzt-operator` are green.
- Slice 4 subtasks 5-9: Managed policy reconciliation is complete in
  controllers. `internal/controller/accesspolicy_hash.go` computes
  deterministic `sha256:` hashes over canonical Access rules, with rule lists
  sorted and include/exclude/require encoded in fixed order. Policy reconcile
  now writes `status.observedRulesHash`, updates the CF policy on spec or CF
  drift, records `status.referencedBy[]` / `referencedByCount` from
  `CloudflareExposure.spec.access.policyRef.name`, watches Exposures to keep
  those references fresh, and blocks finalizer deletion with
  `Reason=BlockedByExposures` while references exist. Exposure reconcile now
  resolves `policyRef.name` to the referenced `CloudflareAccessPolicy`
  `status.policyId`, reports `Reason=PolicyNotFound` or
  `Reason=PolicyNotReady` when binding cannot proceed, binds the resolved UUID
  through Access application create/update, and watches AccessPolicy status
  changes to requeue referencing Exposures. Events include
  `UpdatedAccessPolicy` and `BlockedByExposures`. Tests added/expanded:
  `TestAccessPolicyRulesHashCanonical`, `TestAccessPolicyRulesDrift`,
  `TestAccessPolicyReferencedByPopulated`,
  `TestAccessPolicyReferencedByDecrements`,
  `TestAccessPolicyFinalizerBlockedByExposures`,
  `TestAccessPolicyFinalizerUnblocks`, `TestAccessPolicyConditionsTransition`,
  `TestExposurePolicyRefName`, `TestExposurePolicyRefNamePolicyNotReady`,
  `TestExposurePolicyRefNameMissingPolicyCR`, and
  `TestPolicyStatusUpdatePropagatesToExposures`.
- Slice 4 subtask 10: RBAC markers for CFAP and exposure policy reads were
  regenerated with `make manifests generate`. Helm CRDs and ClusterRole
  were already synced in subtask 4 and remain current. `make test`,
  `go test ./...`, and `helm lint charts/cfzt-operator` are green.

Completed on 2026-05-21:
- Slice 5 subtasks 1-7 + 9: `CloudflareTunnelRoute` cluster-scoped API,
  generated CRD/deepcopy/RBAC, Helm CRD sync, Cloudflare route wrapper
  (`TunnelRoutes`) with fake + real SDK implementation, route controller
  create/update/delete reconciliation, compact `managed-by=cfzt source-uid=...`
  comment ownership guard, controller-side CIDR canonicalization, drift
  correction, finalizer cleanup, Tunnel deletion blocking with
  `Reason=BlockedByRoutes`, Route↔Tunnel cross-watches, and status/events
  (`TunnelNotReady`, `NetworkInvalid`, `ForeignRoute`, `RouteWriteFailed`,
  `Reconciled`, `CreatedRoute`, `DeletedRoute`, `BlockedByRoutes`). Tests added:
  `TestCloudflareTunnelRouteCRDValidation`, `TestFakeRouteCreateGetDelete`,
  `TestFakeRouteListByCanonicalCIDRAndVNet`,
  `TestFakeRouteListOmitsVNetWhenUnset`, `TestFakeRouteEditIdempotent`,
  `TestRouteCreate`, `TestRouteCreateIPv6`, `TestRouteInvalidNetwork`,
  `TestRouteForeignRefuses`, `TestRouteTunnelNotReady`,
  `TestRouteDriftCorrection`, `TestRouteVNetDriftCorrection`,
  VNet-clear validation, `TestRouteEditPreflightForeignRefuses`, `TestRouteCommentDrift`,
  `TestRouteFinalizerDeletes`, `TestRouteFinalizerLeavesForeign`,
  `TestTunnelBlockedByRoutes`, `TestRouteConditionsTransition`, and
  `TestRouteRetriesOnTunnelID`.
- Slice 5 subtask 8: live Cloudflare smoke harness now creates a
  `CloudflareTunnelRoute`, asserts CF-side route ownership, verifies
  idempotency across operator restart, refuses a foreign route collision, and
  cleans up both route CRs and CF routes. `hack/live-cloudflare-local.sh` and
  `.env.live.example` document `CF_SMOKE_ROUTE_CIDR` /
  `CF_SMOKE_ROUTE_CONFLICT_CIDR`.
Completed on 2026-05-22:
- Slice 6 subtask 1: dead-code + dead-branch purge completed. Removed the
  unused Cloudflare error/event constants, `ObservedTunnelUid`, the dead
  Access tag direct branch, the tunnel-route NotFound recursion and foreign
  preflight branch, the stale tunnel SDK caveat, and the committed
  `cover.out`. Chart metadata now calls out the placeholder appVersion/tag
  values.
- Slice 6 subtask 2: index fallback removal + mapper helper landed. The
  client-side index fallback is gone, watch mappers now route through the
  generic `enqueueNamed` helper, and the old mapper functions were deleted.
- Slice 6 subtask 3: real Cloudflare client + zone caches now live at package
  scope keyed by account ID and API-token hash, so SDK clients, connection
  pools, rate limiters, and zone lookups are reused across reconciles.
- Slice 6 subtask 4: tunnel config writes are now skipped when the desired
  ingress document hash and route status already match. `status.ingressDocHash`
  records the last written document, and the unused configurations `Get`
  wrapper was removed.
- Slice 6 subtask 5: Cloudflare client surface cleanup completed. Real client
  404 mapping now runs through `mapAPIError`, Access application create/update
  ensure tags at the wrapper boundary, tunnel listing now takes a plain name
  string, and the route-list SDK subset/superset quirk is documented at the
  wrapper boundary.
- Slice 6 subtask 6: Event recorder wiring now uses controller-runtime v1
  recorders from `mgr.GetEventRecorderFor(...)`. The four reconcilers use
  `k8s.io/client-go/tools/record.EventRecorder` directly, local event helpers
  and nil-recorder guards were removed, and event call sites now call
  `Recorder.Eventf(obj, type, reason, message, args...)`.
- Slice 6 subtask 7: Access application write input now keeps the single
  `PolicyUUID` scalar, while read-side `AccessApplication` exposes only
  `PolicyUUIDs`. Drift detection uses the read-side slice so foreign policy
  attachments are reconciled away.
- Slice 6 subtask 8: Access policy rule hashing and CF drift comparison now
  share one canonicaliser over the internal Cloudflare rule type. The duplicate
  API-vs-CF canonicalisation helpers were removed.
- Slice 6 subtask 9: `cmd/main.go` no longer wires unused webhook server
  setup, webhook certificate flags, metrics certificate flags, or HTTP/2 TLS
  options. The manager still preserves metrics bind/secure flags, health
  probe bind, leader election default-on, log flags, controller registration,
  and HTTPRoute CRD startup discovery.
- Slice 6 subtask 10: managed Access policy names now always append `-cfzt`,
  including when `spec.policyName` is set; spec/API docs were updated to make
  `policyName` a base name. Cloudflared image validation now accepts registry
  hosts with ports while still rejecting `:latest`.
- Slice 6 subtask 11: config tree and Makefile prune completed. Removed
  unused Kubebuilder network-policy, Prometheus, samples, manager deployment,
  metrics Service/patches, and scaffolded admin/editor/viewer RBAC helpers;
  collapsed `config/default/kustomization.yaml`; deleted unsupported
  Makefile deploy/install/installer/buildx targets and unused Kustomize/Kubectl
  plumbing.
- Slice 6 subtask 12: ownership package extraction completed. Source-UID
  comment rendering/parsing and chunked Access app tag matching now live under
  `internal/ownership`; `internal/naming` retains only naming constants and
  functions. Exposure and TunnelRoute reconcilers now call
  `ownership.From(uid)` methods for comments, tags, and ownership checks.
- Slice 6 subtask 13: shared reconciler Base landed. The four reconcilers now
  embed `Base`, share credential loading and Cloudflare client construction,
  use the shared Ready/Progressing status updater where practical, and Exposure
  access/DNS reconcile steps mutate in-scope status instead of returning the
  old four-value status tuple.
- Slice 6 subtask 14: requeue/error policy standardisation completed. Waiting
  reasons now use 30s explicit requeues, while transient Cloudflare write/API
  failures update status and return errors for controller-runtime backoff.
  `AGENTS.md` now documents the split.

Next: Slice 6 subtask 15.

## 3. Slice plan

### Slice 1 — Tunnel and connector

Per `spec.md ## Implementation slices` → Slice 1. Outcome: `CloudflareTunnel` CR adopts/creates the Cloudflare tunnel, persists the token, runs the cloudflared DaemonSet.

**Subtasks**

1. **Define `CloudflareTunnel` types + validation markers.**
   - Files: `api/v1alpha1/cloudflaretunnel_types.go`, `api/v1alpha1/groupversion_info.go`.
   - Implements: `spec.md ## CRD model` (CloudflareTunnel), `## CRD validation` (CloudflareTunnel rules including single namespace source via `spec.cloudflared.namespace`, image `:latest` reject), kubebuilder markers from spec.
   - Tests: `api/v1alpha1` round-trip deepcopy test; `TestCloudflareTunnelCRDValidation` (envtest applies bad manifests, expects rejection — covers `:latest` image case and defaulting).
   - Run `make manifests generate`; commit generated CRD + deepcopy.

2. **Scaffold `internal/cloudflare` interface + fake.**
   - Files: `internal/cloudflare/client.go` (interface), `internal/cloudflare/fake.go`, `internal/cloudflare/tunnels.go`, `internal/cloudflare/real.go` (Tunnels + Token methods only at this point).
   - Implements: `spec.md ## Cloudflare SDK method mapping` (tunnel + token rows), `AGENTS.md ## Cloudflare Rules` (token bucket, backoff). Stub the per-token bucket + 429/5xx retry path now; configurations/access/DNS/zones methods stub to `not implemented` until Slice 2.
   - Tests: `TestFakeTunnelsCreateGetDelete`, `TestFakeTokenIdempotent`. Real SDK paths covered only by interface compile.

3. **Naming + tag helpers.**
   - Files: `internal/naming/names.go`, `internal/naming/tags.go`.
   - Implements: `spec.md ## Naming conventions`, `## Ownership and deletion semantics` (tag format `managed-by=cfzt-operator source-uid=<uid>`).
   - Tests: `TestTagRoundTrip`, `TestTokenSecretName`, `TestDaemonSetName`.

4. **Workload builders (token Secret + DaemonSet).**
   - Files: `internal/workload/token_secret.go`, `internal/workload/daemonset.go`, plus a Go const for the pinned cloudflared image.
   - Implements: `spec.md ## Cloudflared pod spec`, D4 token-only auth, D7 DaemonSet-only, naming rules. Sets pod-template annotation `cfzt.reid.ee/token-checksum=<sha256>` per spec; `updateStrategy: RollingUpdate maxUnavailable: 1`.
   - Tests: `TestDaemonSetSpecMatchesSpec` (golden compare against `## Cloudflared pod spec`), `TestTokenChecksumAnnotation`, `TestHostNetworkPropagates`.

5. **Tunnel controller reconcile loop (no Exposure dependency yet).**
   - Files: `internal/controller/cloudflaretunnel_controller.go`, wiring in `cmd/main.go`.
   - Implements: `spec.md ## CRD model` (CloudflareTunnel responsibilities 1–5, 8), `## Ownership and deletion semantics` (D9 status-ID ownership + name collision guard → `Reason=ForeignTunnel`), `AGENTS.md ## Reconciliation Rules`. D19 `MaxConcurrentReconciles=1`.
   - Tests: `TestTunnelCreate`, `TestTunnelAdopt`, `TestTunnelForeignTunnelRefuses`.

6. **Token rotation + checksum-driven rollout.**
   - Files: extend `internal/controller/cloudflaretunnel_controller.go`, `internal/workload/daemonset.go`.
   - Implements: `spec.md ## CRD model` (Tunnel responsibility 5), `AGENTS.md ## Reconciliation Rules` (token rotation never delete+recreate DS).
   - Tests: `TestTunnelTokenRotation`.

7. **Finalizer (no-op while no Exposures exist).**
   - Files: extend tunnel controller.
   - Implements: D21 finalizer string `cfzt.reid.ee/finalizer`; deletion path removes Cloudflare tunnel + token Secret + DaemonSet only when finalizer cleared. `BlockedByExposures` branch stubbed until Slice 2.
   - Tests: `TestTunnelFinalizerNoop`.

8. **Status conditions + events.**
   - Files: extend tunnel controller; small helper in `internal/controller/conditions.go`.
   - Implements: `spec.md ## Status and conditions` (D8 — `Ready` + `Progressing` only; reasons `CredentialsMissing`, `TunnelCreating`, `TokenFetchFailed`, `WorkloadNotReady`, `Reconciled`), `## Observability` (events `CreatedTunnel`, `TokenRotated`).
   - Tests: assertions woven into existing tunnel envtests; one focused `TestTunnelConditionsTransition`.

9. **Helm chart sync + CI green.**
   - Files: existing `charts/cfzt-operator/Chart.yaml`, `values.yaml`, `crds/{cloudflaretunnel,cloudflareexposure}.yaml` (copied from `config/crd` by `make helm-sync-crds`), `templates/{deployment,serviceaccount,clusterrole,clusterrolebinding,role-leader-election,rolebinding-leader-election,NOTES.txt}`. `.github/workflows/ci.yaml` exercise.
   - Implements: D17 chart layout, `spec.md ## Helm chart layout`, `## RBAC` table (minus Service / HTTPRoute rows — those land in Slice 2/3), D23 NOTES.txt GitOps caveat.
   - Tests: `helm lint charts/cfzt-operator`; `make manifests generate && git diff --exit-code` clean in CI; `make test` green.

**Definition of done** (from `spec.md ## Implementation slices ### Slice 1`):

- `kubectl apply` of a `CloudflareTunnel` against an empty Cloudflare account creates the tunnel, populates `status.tunnelId`, creates Secret `<name>-token`, creates DaemonSet `cloudflared-<name>`.
- `Ready=True` once DaemonSet has ≥1 ready pod.
- Reapply is a no-op.
- envtest tests pass: `TestTunnelCreate`, `TestTunnelAdopt`, `TestTunnelTokenRotation`, `TestTunnelFinalizerNoop`.
- `ci.yaml` green.
- `helm install` against a fresh cluster works.

Subtask-derived additions: `TestCloudflareTunnelCRDValidation`, `TestTunnelForeignTunnelRefuses`, `TestTunnelConditionsTransition`, `TestDaemonSetSpecMatchesSpec`, `TestTokenChecksumAnnotation`, and `TestHostNetworkPropagates` also pass; `helm lint` clean.

**Risks**

- SDK-shape uncertainty (G1 / D13). `cloudflare-go/v4` Tunnels.Token surface may differ from `spec.md ## Cloudflare SDK method mapping`. Use Cloudflare MCP server before writing real client; isolate inside `internal/cloudflare`.
- DaemonSet readiness racing initial token write — implemented order is Secret first, DS second; DS ready-replica count gates `Ready=True` per spec Tunnel responsibility 8.
- Foreign-tunnel detection ambiguity: account already has a tunnel of the same name. Spec mandates `Reason=ForeignTunnel`, no mutation (`## Ownership and deletion semantics`). `TestTunnelForeignTunnelRefuses` covers the name-collision/no-local-ID case.

### Slice 2 — Exposure: routes, DNS, Access

Per `spec.md ## Implementation slices ### Slice 2`. Outcome: `CloudflareExposure` drives ingress doc, DNS CNAME, Access app, policy binding; tunnel finalizer blocks while Exposures reference it; end-to-end traffic with auth.

**Subtasks**

1. **Define `CloudflareExposure` types + CRD validation.**
   - Files: `api/v1alpha1/cloudflareexposure_types.go`.
   - Implements: `spec.md ## CRD model` (CloudflareExposure), `## CRD validation` (CloudflareExposure rules incl. hostname RFC1123 regex, origin presence CEL, port range, UUID pattern on `access.policyRef.uuid`).
   - Tests: `TestCloudflareExposureCRDValidation` (hostname regex, port range, UUID pattern, missing-policy-when-access-enabled).
   - Regenerate manifests + commit.

2. **Tunnel-config builder.**
   - Files: `internal/tunnelconfig/builder.go`.
   - Implements: D11 (single-writer doc, computed-from-K8s, never adopted), `spec.md ## Tunnel configuration concurrency`, `## Naming conventions` (deterministic hostname-sorted order, `http_status:404` catch-all last). Hostname-collision detection at build time → both Exposures get `HostnameConflict`.
   - Tests: `TestTunnelConfigBuilderDeterministic`, `TestTunnelConfigBuilderHostnameCollision`, `TestTunnelConfigBuilderEmptyExposureList` (still emits catch-all).

3. **Extend `internal/cloudflare` for configurations, access, DNS, zones.**
   - Files: `internal/cloudflare/configurations.go`, `access_applications.go`, `dns.go`, `zones.go`, extend `fake.go`.
   - Implements: remaining rows of `spec.md ## Cloudflare SDK method mapping`; zone longest-suffix cache per `## DNS management`. Rate-limit budget shared with Slice 1 client.
   - Tests: `TestZoneLongestSuffix`, `TestFakeAccessAppRoundTrip`, `TestFakeDNSRecordIdempotent`, `TestFakeConfigurationsPutOverwrites`.

4. **Tunnel-config reconciler invocation inside Tunnel controller.**
   - Files: `internal/tunnelconfig/reconciler.go`, extend `internal/controller/cloudflaretunnel_controller.go`.
   - Implements: D11/D19/D20 — Tunnel controller lists referencing Exposures, calls builder, PUTs full doc, writes per-Exposure route hashes into `status.routes[]` (`spec.md ## CRD model` Tunnel responsibilities 6–7).
   - Tests: `TestTunnelWritesIngressDoc`, `TestTunnelRouteHashesPersisted`.

5. **Exposure controller core: validation + Access app + policy binding.**
   - Files: `internal/controller/cloudflareexposure_controller.go`, wiring in `cmd/main.go`.
   - Implements: `spec.md ## CRD model` (Exposure responsibilities 1–3), `## Ownership and deletion semantics` (Access app `source-uid` tag, hostname conflict guard), naming `<displayName|metadata.name>-cfzt`. D19 `MaxConcurrentReconciles=1`.
   - Tests: `TestExposureCreate` (partial — Access path), `TestExposureAccessDisabled`, `TestExposureForeignResource`, `TestExposureHostnameConflict` (Access leg).

6. **DNS reconcile path.**
   - Files: extend Exposure controller; reuse `internal/cloudflare/dns.go` and `zones.go`.
   - Implements: D2 (`dns.manage` opt-out emits zero records and zero annotations), `spec.md ## DNS management`, ownership tagging on DNS record comment.
   - Tests: `TestExposureDNSManagedOff`, `TestExposureDNSCreatesProxiedCNAME`, `TestExposureDNSForeignRecordConflict`.

7. **Cross-controller watches (D20).**
   - Files: extend both controllers' `SetupWithManager`.
   - Implements: `spec.md ## Tunnel configuration concurrency` watch maps — `exposureToTunnel`, `tunnelToExposures`.
   - Tests: `TestExposureEnqueuesTunnel`, `TestTunnelStatusUpdatePropagatesToExposures`.

8. **Exposure status hashes + Ready gating.**
   - Files: extend Exposure controller.
   - Implements: `spec.md ## CRD model` Exposure responsibility 6 (read `publicHostnameRouteHash` from owning Tunnel `status.routes[]`) and 7 (`Ready=True` only when Access+DNS+route all reconciled).
   - Tests: `TestExposureReadyGating`.

9. **Exposure finalizer + tunnel `BlockedByExposures`.**
   - Files: extend both controllers.
   - Implements: D21 finalizer; `spec.md ## Ownership and deletion semantics` tunnel deletion rule; Exposure finalizer removes DNS + Access app then enqueues tunnel for ingress-doc rewrite.
   - Tests: `TestExposureFinalizer`, `TestTunnelBlockedByExposures`.

10. **External-origin coverage + Helm/RBAC update.**
    - Files: extend envtest fixtures; update `charts/cfzt-operator/templates/clusterrole.yaml` if RBAC rows change; verify `cloudflared.hostNetwork: true` round-trips through DaemonSet builder (already from Slice 1 subtask 4 — test added now).
    - Implements: D16 first-class external origin, `spec.md ## Primary user experience` external-origin example.
    - Tests: `TestExposureExternalOriginHostNetwork`.

**Definition of done** (from `spec.md ## Implementation slices ### Slice 2`):

- `kubectl apply` of a `CloudflareExposure` results in: ingress rule in tunnel-config doc, proxied DNS CNAME, Access app with policy bound; `Ready=True`.
- External-origin example (D16) works against a host reachable from cloudflared pods.
- `kubectl delete` of the Exposure removes all three CF resources and the ingress rule.
- `kubectl delete` of the Tunnel while Exposures exist is blocked with `Reason=BlockedByExposures`.
- envtest tests pass: `TestExposureCreate`, `TestExposureDNSManagedOff`, `TestExposureAccessDisabled`, `TestExposureHostnameConflict`, `TestExposureForeignResource`, `TestExposureFinalizer`, `TestTunnelBlockedByExposures`, `TestTunnelConfigBuilderDeterministic`.
- Manual verification: `curl https://<hostname>` returns Access challenge; after auth, reaches origin.

Subtask-derived additions: `TestCloudflareExposureCRDValidation`, `TestZoneLongestSuffix`, `TestTunnelWritesIngressDoc`, `TestExposureExternalOriginHostNetwork`, `TestExposureDNSCreatesProxiedCNAME`, `TestExposureReadyGating` also pass.

**Risks**

- D11 single-writer invariant relies on D12 leader election + D19 `MaxConcurrentReconciles=1`. Any "helpful" parallelism or direct configurations-endpoint call from Exposure controller silently corrupts the doc. Code review must check both — see `AGENTS.md ## Code Rules`.
- Hostname conflict semantics differ between builder (both sides marked) and Access/DNS (only the latecomer marked). Easy to ship inconsistent reasons. Centralise the reason string and test all three legs.
- Zone-list pagination + caching shape unknown until SDK probed (G1). Wrong cache invalidation lets stale zone lists outlive a token rotation. Add a TTL + on-401 invalidation; cover with `TestZoneCacheInvalidatedOn401` if scope allows.
- D9 ownership: tagged-but-foreign-UID resources must never be deleted, even inside finalizers. Finalizer paths get explicit tests per `AGENTS.md ## Verification`.

### Slice 3 — sourceRef derivation

Per `spec.md ## Implementation slices ### Slice 3`. Outcome: `sourceRef` to Service/HTTPRoute derives missing fields and wires ownerReference for GC cascade.

**Subtasks**

1. **Service-source origin defaulting.**
   - Files: `internal/origin/service.go`, extend Exposure controller.
   - Implements: `spec.md ## Origin resolution` (Service rule — defaults `host` to `<svc>.<ns>.svc.cluster.local`, defaults `port` to the single port; error on zero/multiple).
   - Tests: `TestSourceRefServiceSinglePort`, `TestSourceRefServiceMultiPortRejected`.

2. **CRD validation relaxation.**
   - Files: `api/v1alpha1/cloudflareexposure_types.go` (CEL rule update).
   - Implements: `spec.md ## Implementation slices ### Slice 3` step 5 — `origin` no longer required when `sourceRef.kind == Service`.
   - Tests: `TestExposureCRDValidationSliceThreeRelaxed`.

3. **OwnerReference wiring (same-namespace only).**
   - Files: extend Exposure controller.
   - Implements: `spec.md ## Ownership and deletion semantics` Exposure source GC paragraph; `## Primary user experience` `sourceRef` example.
   - Tests: `TestSourceRefDeletionCascades`.

4. **HTTPRoute hostname defaulting.**
   - Files: `internal/origin/httproute.go`, extend Exposure controller.
   - Implements: `spec.md ## Origin resolution` HTTPRoute rule — derive `spec.hostname` from single HTTPRoute `spec.hostnames` entry; origin remains explicit.
   - Tests: `TestHTTPRouteHostnameDerivation`.

5. **HTTPRoute CRD discovery gate at startup.**
   - Files: `cmd/main.go`, possibly `internal/controller/httproute_discovery.go`.
   - Implements: `spec.md ## RBAC` HTTPRoute conditional row, slice 3 step 4 (operator boots clean with log line when CRD absent), AGENTS Repo Map ("conditional on CRD discovery").
   - Tests: `TestHTTPRouteCRDAbsentBootsClean`.

6. **RBAC + Helm chart update for Services + HTTPRoutes.**
   - Files: kubebuilder RBAC markers, `charts/cfzt-operator/templates/clusterrole.yaml` (regenerated).
   - Implements: `spec.md ## RBAC` Service + HTTPRoute rows.
   - Tests: `make manifests` diff clean; `helm lint` clean.

**Definition of done** (from `spec.md ## Implementation slices ### Slice 3`):

- Exposure with `sourceRef: Service` and no origin fields reconciles using derived `<svc>.<ns>.svc.cluster.local:<port>`.
- Deleting the Service garbage-collects the Exposure (verified by `kubectl delete service` → Exposure disappears within GC interval, CF state cleaned).
- HTTPRoute controller absent → operator boots clean with a log line "HTTPRoute CRD not found, controller disabled".
- envtest tests pass: `TestSourceRefServiceSinglePort`, `TestSourceRefServiceMultiPortRejected`, `TestSourceRefDeletionCascades`, `TestHTTPRouteHostnameDerivation`, `TestHTTPRouteCRDAbsentBootsClean`.

Subtask-derived additions: `TestExposureCRDValidationSliceThreeRelaxed` also passes; RBAC + Helm regenerated.

**Risks**

- GC cascade timing is non-deterministic. `TestSourceRefDeletionCascades` must use envtest's GC controller (or simulate via direct finalizer trigger) — naïve `kubectl delete` poll loops are flaky.
- Multi-port Service ambiguity — rejecting is correct per spec but produces confusing UX; ensure `Reason=OriginInvalid` carries port-count hint in `Message`.
- Conditional HTTPRoute controller wiring must not panic when CRD appears after operator start. Spec only requires startup-time discovery; document in NOTES.txt that adding HTTPRoute CRD post-install needs operator restart.

### Slice 4 — Managed Access policies

Per `spec.md ## Implementation slices ### Slice 4` (D24). Outcome: `CloudflareAccessPolicy` CR creates and maintains a reusable account-level Cloudflare Access policy. `CloudflareExposure.spec.access.policyRef.name` binds an Exposure to a managed policy as an alternative to `uuid`.

**Subtasks**

1. **Define `CloudflareAccessPolicy` types + CRD validation.**
   - Files: `api/v1alpha1/cloudflareaccesspolicy_types.go`, `api/v1alpha1/groupversion_info.go` (register).
   - Implements: `spec.md ## CRD model` (CloudflareAccessPolicy), `## CRD validation` (CloudflareAccessPolicy block — credentialsSecretRef + namespace required, policyName default, decision enum, discriminated-union rule items with CEL exactly-one-of, non-empty rules CEL, sessionDuration pattern, purposeJustification shape), kubebuilder markers from spec (cluster-scoped, shortName `cfap`).
   - Tests: `TestCloudflareAccessPolicyCRDValidation` (decision enum, rule discriminated-union, empty-rules rejection, sessionDuration pattern); `api/v1alpha1` deepcopy round-trip.
   - Run `make manifests generate`; commit generated CRD + deepcopy.

2. **Extend Exposure CRD: `policyRef.name` + relaxed CEL.**
   - Files: `api/v1alpha1/cloudflareexposure_types.go`.
   - Implements: `spec.md ## CRD model` Exposure schema update (`policyRef.name`) + `## CRD validation` rewrite of the access CEL rule to exactly-one-of {uuid, name} when `access.enabled: true`.
   - Tests: `TestExposurePolicyRefOneOfValidation` (uuid alone OK; name alone OK; both → reject; neither → reject when enabled; neither → OK when disabled).
   - Regenerate manifests + commit.

3. **`internal/cloudflare/access_policies.go` interface + fake + real.**
   - Files: `internal/cloudflare/access_policies.go`, extend `internal/cloudflare/client.go`, `fake.go`, `real.go`.
   - Implements: SDK mapping rows for `ZeroTrust.Access.Policies.{List,Get,New,Update,Delete}` per `spec.md ## Cloudflare SDK method mapping`. Verify exact paths via Cloudflare MCP first (G1 risk). Reuse per-token rate-limit bucket from Slice 1 client.
   - Tests: `TestFakeAccessPolicyCreateGetDelete`, `TestFakeAccessPolicyListByName`, `TestFakeAccessPolicyUpdateRulesIdempotent`.

4. **Policy controller core: credentials, ID-record reconcile, ForeignPolicy guard.**
   - Files: `internal/controller/cloudflareaccesspolicy_controller.go`, wiring in `cmd/main.go` (`MaxConcurrentReconciles=1`).
   - Implements: `spec.md ## CRD model` (CloudflareAccessPolicy responsibilities 1–3, 8), `## Ownership and deletion semantics` policy ownership rule (mirrors D9 tunnel pattern), `Reason=ForeignPolicy` on name-collision without local ID.
   - Tests: `TestAccessPolicyCreate`, `TestAccessPolicyForeignRefuses`.

5. **Rule-hash drift detection + update path.**
   - Files: extend policy controller; helper `internal/controller/accesspolicy_hash.go` for canonical rules JSON → sha256.
   - Implements: `spec.md ## CRD model` CloudflareAccessPolicy responsibility 4. Canonical JSON: rule fields sorted, groups in fixed order (include, exclude, require), empty groups omitted.
   - Tests: `TestAccessPolicyRulesHashCanonical`, `TestAccessPolicyRulesDrift`.

6. **`referencedBy[]` + `referencedByCount` from Exposure cross-watch.**
   - Files: extend policy controller `SetupWithManager` with `.Watches(&CloudflareExposure{}, exposureToPolicy)`; reuse helpers in `internal/controller/conditions.go`.
   - Implements: `spec.md ## CRD model` responsibility 5 + `## Tunnel configuration concurrency` Slice 4 additions (Policy↔Exposure watches).
   - Tests: `TestAccessPolicyReferencedByPopulated`, `TestAccessPolicyReferencedByDecrements`.

7. **Policy finalizer + `BlockedByExposures`.**
   - Files: extend policy controller.
   - Implements: D21 finalizer on `CloudflareAccessPolicy`; `spec.md ## Ownership and deletion semantics` policy deletion rule. Mutation guard: only delete CF policy when `source-uid` tag matches (fall back to ID equality if SDK has no tag field).
   - Tests: `TestAccessPolicyFinalizerBlockedByExposures`, `TestAccessPolicyFinalizerUnblocks`.

8. **Exposure controller: resolve `policyRef.name` → bind via `Applications.Policies.Update`.**
   - Files: extend `internal/controller/cloudflareexposure_controller.go`; add `policyToExposures` watch map.
   - Implements: `spec.md ## CRD model` Exposure responsibility 3 rewrite (uuid OR name resolution), `Reason=PolicyNotReady` when target Policy CR exists but is not yet `Ready=True` or `status.policyId` empty. D20 Slice 4 addition: Exposure `.Watches(&CloudflareAccessPolicy{}, policyToExposures)`.
   - Tests: `TestExposurePolicyRefName` (happy path), `TestExposurePolicyRefNamePolicyNotReady`, `TestExposurePolicyRefNameMissingPolicyCR` (→ `Reason=PolicyNotFound`).

9. **Status conditions + events.**
   - Files: extend policy controller.
   - Implements: D8 conditions on `CloudflareAccessPolicy`; reasons `ForeignPolicy`, `PolicyNotReady`, `BlockedByExposures`, `Reconciled`. Events: `CreatedAccessPolicy`, `UpdatedAccessPolicy`, `BlockedByExposures`.
   - Tests: `TestAccessPolicyConditionsTransition`.

10. **RBAC + Helm chart sync.**
    - Files: kubebuilder RBAC markers for `cloudflareaccesspolicies{,/status,/finalizers}`; regenerate `charts/cfzt-operator/templates/clusterrole.yaml`; `make helm-sync-crds` copies new CRD to `charts/cfzt-operator/crds/`.
    - Implements: `spec.md ## RBAC` Slice 4 rows.
    - Tests: `make manifests generate && git diff --exit-code` clean; `helm lint charts/cfzt-operator` clean.

**Definition of done** (from `spec.md ## Implementation slices ### Slice 4`):

- `kubectl apply` of a `CloudflareAccessPolicy` creates a CF Access policy, populates `status.policyId`, sets `Ready=True`.
- A pre-existing CF policy with name collision and no local ID record → `Ready=False, Reason=ForeignPolicy`, no mutation of the foreign policy.
- An Exposure with `policyRef.name` binds the policy ID once the Policy CR becomes ready.
- `kubectl delete` of a Policy CR with referencing Exposures is blocked (`Reason=BlockedByExposures`); succeeds once references are removed.
- Editing `spec.rules` on a Policy CR rewrites the CF policy and propagates a reconcile to all referencing Exposures.
- envtest tests pass: `TestAccessPolicyCreate`, `TestAccessPolicyForeignRefuses`, `TestAccessPolicyRulesDrift`, `TestAccessPolicyFinalizerBlockedByExposures`, `TestAccessPolicyFinalizerUnblocks`, `TestExposurePolicyRefName`, `TestExposurePolicyRefNamePolicyNotReady`, `TestExposurePolicyRefOneOfValidation`.
- Manual: dashboard shows policy created; `kubectl edit cfap` rolls rules in CF within one reconcile.

Subtask-derived additions: `TestCloudflareAccessPolicyCRDValidation`, `TestFakeAccessPolicyCreateGetDelete`, `TestFakeAccessPolicyListByName`, `TestFakeAccessPolicyUpdateRulesIdempotent`, `TestAccessPolicyRulesHashCanonical`, `TestAccessPolicyReferencedByPopulated`, `TestAccessPolicyReferencedByDecrements`, `TestExposurePolicyRefNameMissingPolicyCR`, `TestAccessPolicyConditionsTransition` also pass.

**Risks**

- SDK uncertainty (G1 / D13). `ZeroTrust.Access.Policies` surface in cloudflare-go/v4 may differ from the spec mapping; reusable account-level policies are a distinct surface from `Applications.Policies.*`. Probe via Cloudflare MCP before writing real client. Isolate inside `internal/cloudflare`.
- Tag/comment field on Access policies may not be surfaced by the SDK (mirrors D9 tunnel-comment gap). If absent, fall back to `status.policyId`-only ownership tracking; document the fallback in the implementing PR and tighten the mutation guard to ID-equality. Safe under D12 leader-election.
- Rule canonicalisation must be deterministic across Go map iteration and JSON marshalling — flaky hash → spurious reconciles → CF rate-limit pressure. `TestAccessPolicyRulesHashCanonical` covers this with explicit field-order assertions.
- Cross-watch fan-out: a single Policy CR rule edit enqueues every referencing Exposure. With `MaxConcurrentReconciles=1` on Exposure, large fan-out throttles. Acceptable in MVP; revisit if Slice 4 ships into a large fleet.
- D24 policy deletion: tagged-but-foreign policies must never be deleted, even inside the finalizer. Mirror Slice 2 D9 finalizer test discipline — `TestAccessPolicyFinalizerUnblocks` must include a foreign-tag negative case.

### Slice 5 — Tunnel private network routes

Per `spec.md ## Implementation slices ### Slice 5` (D25). Outcome:
`CloudflareTunnelRoute` CR creates and maintains a single Cloudflare Tunnel
private-network route (CIDR → tunnel binding). Tunnel deletion is blocked while
routes reference the tunnel (`Reason=BlockedByRoutes`).

**Subtasks**

1. **Define `CloudflareTunnelRoute` types + CRD validation.**
   - Files: `api/v1alpha1/cloudflaretunnelroute_types.go`, `api/v1alpha1/groupversion_info.go` (register).
   - Implements: `spec.md ## CRD model` (CloudflareTunnelRoute), `## CRD validation` (coarse IPv4 / IPv6 CIDR regex, controller-side `net/netip.ParsePrefix` for both families, general UUID VNet, explicit empty VNet as unset, immutable tunnelRef CEL, reject clearing VNet after set, `spec.comment` maxLength 34), kubebuilder markers (cluster-scoped, shortName `cftr`).
   - Tests: `TestCloudflareTunnelRouteCRDValidation` (good IPv4, good IPv6, bad IPv4 octets, bad IPv6, immutable tunnelRef change rejected, explicit empty VNet, bad VNet UUID, clearing VNet after set, overlong comment); `api/v1alpha1` deepcopy round-trip.
   - Run `make manifests generate`; commit generated CRD + deepcopy.

2. **`internal/cloudflare/routes.go` interface + fake + real.**
   - Files: `internal/cloudflare/routes.go`, extend `internal/cloudflare/client.go`, `fake.go`, `real.go`.
   - Implements: SDK mapping rows for `ZeroTrust.Networks.Routes.{List,New,Get,Edit,Delete}` per `spec.md ## Cloudflare SDK method mapping`. Use the non-deprecated route-ID endpoints, route `comment`, and per-token rate-limit bucket from Slice 1 client. Do not use deprecated `Routes.Networks.*`.
   - List implementation: expose wrapper filters for active `cfd_tunnel` routes, optional tunnel ID, optional VNet ID only when the CR sets `spec.virtualNetworkId`, and canonical exact CIDR matching in code via `network_subset` / `network_superset` where useful.
   - Tests: `TestFakeRouteCreateGetDelete`, `TestFakeRouteListByCanonicalCIDRAndVNet`, `TestFakeRouteListOmitsVNetWhenUnset`, `TestFakeRouteEditIdempotent`.

3. **Route controller core: credentials, tunnel-ID gate, ID-record reconcile, ForeignRoute guard.**
   - Files: `internal/controller/cloudflaretunnelroute_controller.go`, wiring in `cmd/main.go` (`MaxConcurrentReconciles=1`).
   - Implements: `spec.md ## CRD model` (CloudflareTunnelRoute responsibilities 1–5), `## Ownership and deletion semantics` (route ownership rule), `Reason=NetworkInvalid` on controller-side CIDR parse failure, `Reason=ForeignRoute` on collision without local ID/matching compact `source-uid`, `Reason=TunnelNotReady` while referenced Tunnel is absent/deleting or lacks `status.tunnelId`.
   - Tests: `TestRouteCreate`, `TestRouteCreateIPv6`, `TestRouteInvalidNetwork`, `TestRouteForeignRefuses`, `TestRouteTunnelNotReady`.

4. **Drift correction (Edit path).**
   - Files: extend route controller.
   - Implements: `spec.md ## CRD model` CloudflareTunnelRoute responsibility 4. On spec change or CF-side drift, preflight the target CIDR/VNet for foreign active routes before calling `Edit`. Network and non-empty VNet changes are allowed via update; clearing VNet after it has been set is blocked by CEL because Slice 5 omits `virtual_network_id` when unset. TunnelRef changes are blocked by CEL (subtask 1).
   - Tests: `TestRouteDriftCorrection` (network change), `TestRouteVNetDriftCorrection`, `TestRouteEditPreflightForeignRefuses`, `TestRouteCommentDrift`.

5. **Finalizer + foreign-route safety.**
   - Files: extend route controller.
   - Implements: D21 finalizer on `CloudflareTunnelRoute`; spec ownership/deletion rule. On delete: `Get(routeId)`, verify compact comment `source-uid`, then `Delete`. Foreign-tagged or missing-tag routes are left alone — finalizer removed without API delete.
   - Tests: `TestRouteFinalizerDeletes`, `TestRouteFinalizerLeavesForeign`.

6. **Tunnel finalizer extension + Route↔Tunnel cross-watches.**
   - Files: extend `internal/controller/cloudflaretunnel_controller.go` (block deletion with `Reason=BlockedByRoutes` when ≥1 route references the tunnel); extend both controllers' `SetupWithManager` (`routeToTunnel`, `tunnelToRoutes` map funcs).
   - Implements: `spec.md ## Ownership and deletion semantics` extended tunnel-deletion rule, `## Tunnel configuration concurrency`-style cross-watch additions. Route controller retries when the referenced Tunnel gains `status.tunnelId` or is deleted.
   - Tests: `TestTunnelBlockedByRoutes`, `TestRouteRetriesOnTunnelID`.

7. **Status conditions + events.**
   - Files: extend route controller; reuse `internal/controller/conditions.go`.
   - Implements: D8 conditions on `CloudflareTunnelRoute`; reasons `TunnelNotReady`, `NetworkInvalid`, `ForeignRoute`, `RouteWriteFailed`, `Reconciled`. Events: `CreatedRoute`, `DeletedRoute`, `ForeignRoute`, `BlockedByRoutes` (emitted from tunnel controller).
   - Tests: `TestRouteConditionsTransition`.

8. **Live Cloudflare smoke coverage.**
   - Files: `test/live/cloudflare_smoke_test.go`, `hack/live-cloudflare-local.sh`, `.env.live.example` if present.
   - Implements: `spec.md ## CI / CD` live-smoke addition for routes. Extends `TestCloudflareLifecycle` in place (no new top-level test function) so `release.yaml` and `hack/live-cloudflare-local.sh lifecycle` exercise the new phase automatically. Accepts that actual packet routing through the tunnel is NOT validated — smoke proves that `CloudflareTunnelRoute` reaches `Ready=True`, the CF route appears in `Routes.List`, is idempotent across operator restart, refuses a foreign-route collision, and is removed by `cleanup()`.
   - Concrete edits to `test/live/cloudflare_smoke_test.go`:
     - `smokeConfig` gains `tunnelRouteName string`, `tunnelRouteCIDR string`, `tunnelRouteConflictCIDR string`. `loadSmokeConfig` sets `tunnelRouteName = "cfzt-smoke-route-" + runSuffix`; defaults `tunnelRouteCIDR = envDefault("CF_SMOKE_ROUTE_CIDR", "100.64.207.0/24")` and `tunnelRouteConflictCIDR = envDefault("CF_SMOKE_ROUTE_CONFLICT_CIDR", "100.64.208.0/24")` (RFC 6598 CGNAT range, unlikely to clash with real customer routes).
     - `TestCloudflareLifecycle` adds a new phase after `status.tunnelId` is available and before the existing idempotency phase: `h.createTunnelRoute()` → `h.waitTunnelRouteReady(7*time.Minute)` → capture `routeIDBefore`; `h.assertOneTunnelRoute(routeIDBefore)` (CF-side `Routes.List` filter by tunnel ID + canonical CIDR, expects exactly one match with compact `managed-by=cfzt source-uid=...` in the comment).
     - Idempotency phase: add `h.updateTunnelRouteNoop()` next to the existing `updateTunnelNoop` / `updateAccessPolicyNoop`; after `restartOperator()` add `route = h.waitTunnelRouteReady(4*time.Minute)` and `assertEqual(t, "tunnel route ID", routeIDBefore, route.Status.RouteId)`.
     - Foreign-route safety phase (parallel to existing foreign-DNS conflict block): create a CF route directly via `h.cf.Routes().Create(ctx, …)` with `cfg.tunnelRouteConflictCIDR` and the smoke tunnel ID, comment `cfzt-live-smoke-foreign-route`. Apply a matching `CloudflareTunnelRoute` CR; assert it reports `Reason=ForeignRoute` within 4 minutes via `h.waitTunnelRouteForeignReason`; assert the conflict route still exists with its original comment / ID.
     - `cleanup()` extension: delete both Route CRs (happy path + foreign-collision); poll for the CF route at `tunnelRouteCIDR` to be gone; explicitly `h.cf.Routes().Delete(ctx, conflictRouteID)` so the test leaves no residue at `tunnelRouteConflictCIDR`.
     - Wait helpers `waitTunnelRouteReady`, `waitTunnelRouteForeignReason` follow the shape of `waitTunnelReady` / `waitExposureConflictReason`.
   - `hack/live-cloudflare-local.sh` edits: extend the env-var block in `usage()` to mention `CF_SMOKE_ROUTE_CIDR` and `CF_SMOKE_ROUTE_CONFLICT_CIDR` overrides. No new top-level subcommand — the new phase runs inside `TestCloudflareLifecycle` and so is exercised by `hack/live-cloudflare-local.sh lifecycle`.
   - `.env.live.example`: append commented placeholders `# CF_SMOKE_ROUTE_CIDR=100.64.207.0/24` and `# CF_SMOKE_ROUTE_CONFLICT_CIDR=100.64.208.0/24` so operators know where to override (skip silently if the example file is not present in the worktree).
   - Tests: `TestCloudflareLifecycle` (existing) extended in place — no new top-level test function. `TestCloudflarePreflight` unchanged (route credentials piggyback on the existing tunnel-edit API token scope).
   - Acceptance: end-to-end packet routing through the route is explicitly out of scope for the smoke (no WARP client available in kind). Lifecycle (create, idempotent reconcile, foreign-collision refusal, clean teardown) is sufficient.

9. **RBAC + Helm chart sync.**
   - Files: kubebuilder RBAC markers for `cloudflaretunnelroutes{,/status,/finalizers}`; regenerate `charts/cfzt-operator/templates/clusterrole.yaml`; `make helm-sync-crds` copies new CRD to `charts/cfzt-operator/crds/`.
   - Implements: `spec.md ## RBAC` Slice 5 rows.
   - Tests: `make manifests generate && git diff --exit-code` clean; `helm lint charts/cfzt-operator` clean.

**Definition of done** (from `spec.md ## Implementation slices ### Slice 5`):

- `kubectl apply` of a `CloudflareTunnelRoute` against a tunnel with `status.tunnelId` creates the CF route, populates `status.routeId`, sets `Ready=True`.
- `kubectl delete cloudflaretunnelroute <name>` removes the CF route.
- Pre-existing CF route with same target CIDR/VNet and no local ID record or mismatching compact `source-uid` comment → `Ready=False, Reason=ForeignRoute`, no mutation.
- `kubectl delete cloudflaretunnel <name>` while a route references it is blocked with `Reason=BlockedByRoutes`.
- Editing `spec.network`, non-empty `spec.virtualNetworkId`, or `spec.comment` preflights target CIDR/VNet conflicts, then rewrites the CF route within one reconcile when no foreign route blocks the change. Clearing a previously set `spec.virtualNetworkId` is rejected; delete + recreate to return to the account default VNet.
- envtest tests pass: `TestCloudflareTunnelRouteCRDValidation`, `TestRouteCreate`, `TestRouteCreateIPv6`, `TestRouteInvalidNetwork`, `TestRouteForeignRefuses`, `TestRouteDriftCorrection`, `TestRouteEditPreflightForeignRefuses`, `TestRouteFinalizerDeletes`, `TestRouteFinalizerLeavesForeign`, `TestRouteTunnelNotReady`, `TestTunnelBlockedByRoutes`, `TestRouteConditionsTransition`.
- Live smoke (`hack/live-cloudflare-local.sh lifecycle`) creates, re-reconciles, refuses a foreign-route collision, and cleans up the `CloudflareTunnelRoute` in addition to the existing Slice 1–4 phases.
- Manual: dashboard Networks → Routes shows the route with compact `managed-by=cfzt` source-uid in the comment.

Subtask-derived additions: `TestFakeRouteCreateGetDelete`, `TestFakeRouteListByCanonicalCIDRAndVNet`, `TestFakeRouteListOmitsVNetWhenUnset`, `TestFakeRouteEditIdempotent`, `TestRouteVNetDriftCorrection`, `TestRouteCommentDrift`, `TestRouteRetriesOnTunnelID` also pass.

**Risks**

- SDK uncertainty (G1 / D13). Route methods exist in `cloudflare-go/v4` v4.6.0, but confirm request params via Cloudflare MCP before writing real client. Isolate inside `internal/cloudflare/routes.go`.
- CIDR validation gap. CEL regex is intentionally coarse; controller-side `net/netip.ParsePrefix` for IPv4 and IPv6 is mandatory and covered by `TestRouteInvalidNetwork`.
- CIDR overlap across VNets. CEL cannot enforce cross-CR CIDR uniqueness. Controller relies on active-route List plus canonical exact filtering and fails closed on ambiguous or foreign matches.
- VNet omission semantics. When `spec.virtualNetworkId` is unset, do not resolve the account default VNet and do not send `virtual_network_id`; tests cover omitted VNet params and foreign exact-CIDR refusal. Clearing a previously set VNet is intentionally rejected rather than sending API null.
- Tunnel-readiness coupling. Route registration gates on `status.tunnelId`, not full Tunnel `Ready=True`; cover with `TestRouteRetriesOnTunnelID`.
- Live smoke CIDR collision — the chosen documentation/CGNAT CIDRs must not collide with any real route on the test account. Override via `CF_SMOKE_ROUTE_CIDR` / `CF_SMOKE_ROUTE_CONFLICT_CIDR` in `.env.live` if defaults conflict.
- WARP scope creep. Route registration is the only intent of this slice. WARP client routing, Gateway policies, and split-tunnel config remain deferred.

### Slice 6 — Pre-MVP cleanup

Per `claude-review.md`. Outcome: ~720 LoC of duplicated scaffolding, dead
branches, and stale kustomize removed; one Cloudflare API call saved per tunnel
reconcile (1.11); real zone-cache hits restored (1.4); ownership-tag handling
centralised; reconciler scaffolds share a `Base`; `policyName` collision
protection no longer bypassable. Acts on every item in `claude-review.md`
(§§1–8). Planning decisions baked in: always append `-cfzt` to managed Access
policy names (§7.1); transient errors return errors for controller-runtime
exponential backoff, "waiting" reasons return `RequeueAfter: 30s` (§1.15);
`AccessApplicationInput.PolicyUUID string` for writes,
`AccessApplication.PolicyUUIDs []string` for reads (§1.5); shared `Base` uses a
simple callback helper, no generics (§1.1); reconcile steps mutate in-scope
status and return directly (§1.8); aggressive prune of `config/` and
`make deploy` targets (§1.9/§1.10). CR shapes, finalizer strings, label
schemes, D1–D25, and `AGENTS.md ## Reconciliation Rules` invariants are
preserved. Subtask order follows `claude-review.md ## 10` — small isolated
cleanups first so the bigger reshapes don't fight rebases.

**Subtasks**

1. **Dead-code + dead-branch purge.**
   - Files: `internal/cloudflare/client.go` (delete `ErrNotImplemented`),
     `internal/controller/conditions.go` (delete `EventReconcileFailed`,
     `EventForeignTunnel`), `api/v1alpha1/cloudflareexposure_types.go` (delete
     `ObservedTunnelUid` + regen deepcopy + CRD YAML), repo root (delete
     `cover.out`, add to `.gitignore`),
     `internal/controller/cloudflareexposure_controller.go` (lines ~634–653
     tag-chunk "direct" branch + line ~617 `directUID` short-circuit removed;
     parser collapses to single chunked path),
     `internal/controller/cloudflaretunnelroute_controller.go` lines ~258–276
     `preflightRouteTarget` simplified to one-CIDR-one-route iteration
     (review §3.3), lines ~211–214 `reconcileCloudflareRoute` NotFound
     recursion linearised into a fall-through into the create path (review
     §3.4), `internal/cloudflare/tunnels.go` lines 5–8 SDK comment about D9
     marker updated/removed, `charts/cfzt-operator/Chart.yaml` +
     `values.yaml` document the `tag: ""` placeholder explicitly.
   - Implements: `claude-review.md ## 2` dead-code table, §1.7 unreachable
     tag-chunk branch, §3.3 unsound foreign-route check, §3.4 recursion
     linearisation.
   - Tests: existing tests must remain green — pure deletions. Verify
     `make manifests generate && git diff --exit-code` clean after CRD regen
     for `ObservedTunnelUid` removal.

2. **Index fallback removal + mapper-func generic helper.**
   - Files: `internal/controller/indexes.go` (delete `isFieldIndexUnavailable`,
     delete the `List → client-side filter` fallback in
     `listCloudflareExposuresByField` + `listCloudflareTunnelRoutesByField`,
     drop the `keep func(...) bool` parameter); the four reconciler call
     sites simplified to e.g.
     `listExposuresByTunnel(ctx, r.Client, tunnel.Name)`; new
     `internal/controller/mapper.go` adds
     `enqueueNamed[T client.Object](extract func(T) []types.NamespacedName)
     handler.MapFunc` replacing the five existing mapper funcs.
   - Implements: review §1.3, §3.1.
   - Tests: existing `TestExposureEnqueuesTunnel`,
     `TestTunnelStatusUpdatePropagatesToExposures`,
     `TestPolicyStatusUpdatePropagatesToExposures` remain green without
     edits. New: `TestEnqueueNamedExtracts`.

3. **Zone cache + RealClient cache hoisted out of per-reconcile construction.**
   - Files: `internal/cloudflare/real.go` — drop
     `RealClient.{mu, zoneCache, zoneCacheReady}`; introduce package-level
     `zoneCacheByCred sync.Map` keyed by
     `cacheKey{ accountID string; tokenHash [32]byte }` mirroring the
     existing `limiterByToken sync.Map`; introduce package-level
     `clientCacheByCred sync.Map` so the SDK connection pool is reused
     across reconciles.
   - Implements: review §1.4.
   - Tests: `TestRealClientZoneCacheServesAcrossInstances`,
     `TestRealClientReuse`.

4. **Conditional `Configurations.Update` + drop unused `Configurations.Get`.**
   - Files: `internal/controller/cloudflaretunnel_controller.go` lines
     ~247–260 — sha256 the marshalled `result.Config`, store on
     `CloudflareTunnel.Status.IngressDocHash` (new `+optional` field, regen
     CRD + deepcopy), skip the PUT when hash matches and `RouteHashes`
     reflects the same exposure set; `internal/cloudflare/configurations.go`
     — delete `Configurations.Get` from interface + fake + real;
     `api/v1alpha1/cloudflaretunnel_types.go` — add `IngressDocHash string`
     to `CloudflareTunnelStatus`.
   - Implements: review §1.11, §5.2.
   - Tests: `TestTunnelConfigUpdateSkippedWhenUnchanged`,
     `TestTunnelConfigUpdateOnDrift`; existing `TestTunnelWritesIngressDoc`
     adjusted to assert one PUT on creation only.

5. **Cloudflare client surface cleanups.**
   - Files: `internal/cloudflare/real.go` — extract
     `mapAPIError(err error) error` and apply at every `withRetry` callback
     boundary (~10 sites, review §1.13); refactor `accessNewBody` /
     `accessUpdateBody` to share a `commonAccessFields(...)` helper so
     divergence becomes visible (review §1.14); push tag-ensure inside
     `realAccessApplications.Create` and `.Update` so the controller drops
     its tag-ensure loop at `cloudflareexposure_controller.go:248` (review
     §5.1); change `Tunnels.List(ctx, name string)` (drop
     `ListTunnelsFilter` struct, review §5.3); add wrapper-boundary comment
     on `ListTunnelRoutesFilter.Network` workaround at line ~254 documenting
     the SDK subset/superset quirk (review §5.4).
   - Implements: review §1.13, §1.14, §5.1, §5.3, §5.4.
   - Tests: `TestMapAPIErrorNotFound`,
     `TestFakeAccessApplicationsEnsuresTagsImplicitly`,
     `TestTunnelsListString`. Existing `TestExposureCreate` adjusted.

6. **Event-recorder switch to controller-runtime v1.**
   - Files: `cmd/main.go` — wire `record.EventRecorder` (from
     `k8s.io/client-go/tools/record`) for each of the four reconcilers via
     `mgr.GetEventRecorderFor(...)`; delete the local `event(...)` helpers
     and `nil`-recorder guards in `cloudflaretunnel_controller.go:450`,
     `cloudflareexposure_controller.go:686`,
     `cloudflaretunnelroute_controller.go:299`,
     `cloudflareaccesspolicy_controller.go:244`; replace call sites with
     `r.Recorder.Eventf(obj, type, reason, fmt, args...)`.
   - Implements: review §1.2.
   - Tests: existing event-emitting tests (`TestExposureFinalizer`
     `BlockedByExposures` event, `TestTunnelTokenRotation` `TokenRotated`,
     `TestRouteConditionsTransition` event sequence) all green.

7. **`PolicyUUID` single-in, `PolicyUUIDs` multi-out.**
   - Files: `internal/cloudflare/access_applications.go` —
     `AccessApplicationInput.PolicyUUID string`,
     `AccessApplication.PolicyUUIDs []string`; `internal/cloudflare/real.go`
     ~lines 727+, 695, 711, 782–819 — drop `firstString`,
     `fallbackPolicyIDs`, `selfHosted{New,Update}PolicyIDs`;
     `internal/controller/cloudflareexposure_controller.go` ~line 679 —
     `accessApplicationPoliciesMatch` collapses to single-branch comparison
     (drift detection still uses the read-side slice to catch foreign
     additions).
   - Implements: review §1.5.
   - Tests: `TestAccessApplicationForeignPolicyAttachmentDriftsBack`;
     existing `TestExposureCreate` covers happy path.

8. **`accesspolicy_hash.go` canonicalisation collapse.**
   - Files: `internal/controller/accesspolicy_hash.go` — single
     `canonicalize(rules []cloudflare.AccessRule) []canonicalAccessRule`
     entry point (SDK boundary already speaks `cloudflare.AccessRule`);
     `internal/controller/cloudflareaccesspolicy_controller.go` ~line 322 —
     drop `canonicalizeCloudflareRules`; fold `translateRules` +
     `accessPolicyMatches` into `hashAccessRules` + `accessRulesEqual`.
   - Implements: review §1.12.
   - Tests: existing `TestAccessPolicyRulesHashCanonical`,
     `TestAccessPolicyRulesDrift` remain green with the single canonicaliser.

9. **`cmd/main.go` trim.**
   - Files: `cmd/main.go` — delete webhook server setup (`webhookCertPath`,
     `webhookCertName`, `webhookCertKey`, `tlsOpts`,
     `webhookServerOptions`, `webhook.NewServer(...)` lines ~60–125), delete
     `metricsCertPath/Name/Key` (chart bakes its own metrics service), drop
     now-unused imports (`crypto/tls`, `.../webhook`,
     `.../metrics/filters`), remove `// nolint:gocyclo` at line ~56; target
     ~80 lines.
   - Implements: review §1.10, §3.8.
   - Tests: `go build ./cmd/...` green; envtest green; manual
     `go run ./cmd/main.go -h` shows trimmed flag set.

10. **`policyName` always `-cfzt`-suffixed + image-regex port test.**
    - Files: `internal/controller/cloudflareaccesspolicy_controller.go`
      ~line 262 — always append `-cfzt` to `desiredPolicyName` regardless
      of whether `spec.policyName` is set; document the behaviour change in
      `api/v1alpha1/cloudflareaccesspolicy_types.go` `PolicyName` docstring;
      update `spec.md ## CRD model ### CloudflareAccessPolicy` `policyName`
      field description.
    - Implements: review §7.1 (decision: always append).
    - Tests: `TestAccessPolicyNameAlwaysSuffixed`,
      `TestCloudflareTunnelImageRegexPort` (verifies CRD accepts
      `registry.local:5000/cloudflared:2025.1.0`; widen
      `api/v1alpha1/cloudflaretunnel_types.go` image regex + regen CRDs if
      rejected, per review §7.2).

11. **`config/` tree + Makefile prune (aggressive).**
    - Files (delete): `config/network-policy/`, `config/prometheus/`,
      `config/samples/` (minimal samples → `examples/` if any remain cited
      from `spec.md`), `config/rbac/cloudflareexposure_admin_role.yaml` +
      `..._editor_role.yaml` + `..._viewer_role.yaml` (×6 kubebuilder
      helpers), `config/default/cert_metrics_manager_patch.yaml`,
      `config/default/manager_metrics_patch.yaml`, `config/manager/`;
      collapse `config/default/kustomization.yaml` to only what
      `make manifests` writes into `config/crd/bases/` and what
      `make helm-sync-crds` consumes (delete ~90% commented-out blocks);
      `Makefile` — delete `docker-buildx`, `build-installer`, `install`,
      `uninstall`, `deploy`, `undeploy` (~60 lines).
    - Implements: review §1.9, §2 dead-code table for config tree.
    - Tests: `make manifests generate && git diff --exit-code` clean;
      `make helm-sync-crds && git diff --exit-code` clean; `make test`,
      `helm lint charts/cfzt-operator`, `ci.yaml` all green.
    - Cross-check: update `AGENTS.md ## Commands` if any deleted target was
      referenced.

12. **Ownership package extraction.**
    - Files: new `internal/ownership/owner.go`
      (`type Owner struct{ uid types.UID }`; `func From(uid types.UID) Owner`;
      `.Comment() string`, `.CompactComment() string`, `.Tags() []string`,
      `.MatchesComment(s string) bool`, `.MatchesTags(t []string) bool`);
      new `internal/ownership/comment.go` (render/parse the
      `managed-by=cfzt-operator source-uid=<uid>` long form + compact form
      ≤34 chars); new `internal/ownership/accesstag.go` (chunked
      `source-uid=<chunk>` tags with `accessTagMaxLength = 35`, dead-direct
      branch already removed in subtask 1); delete `internal/naming/tags.go`;
      delete inline ownership helpers in
      `cloudflareexposure_controller.go:564–654`,
      `cloudflaretunnelroute_controller.go`, tunnel analogues; rewrite call
      sites to `ownership.From(uid).*`.
    - Implements: review §1.6, §1.17.
    - Tests: new `internal/ownership/owner_test.go` —
      `TestOwnerCommentRoundTrip`, `TestOwnerCompactCommentRoundTrip`,
      `TestOwnerAccessTagChunkRoundTrip`, `TestOwnerMatchesForeign`. Existing
      `TestExposureForeignResource`, `TestRouteForeignRefuses`,
      `TestExposureDNSForeignRecordConflict`, `TestAccessPolicyForeignRefuses`
      remain green.

13. **Shared reconciler `Base` + inline reconcile step pattern.**
    - Files: new `internal/controller/base.go`:
      ```go
      type Base struct {
          client.Client
          Scheme   *runtime.Scheme
          Recorder record.EventRecorder
          NewCloudflareClient func(ctx context.Context, ref CredentialsRef)
              (cloudflare.Client, error)
      }
      func (b *Base) SetReady(ctx context.Context, obj client.Object,
          conditions *[]metav1.Condition, generation int64, ready bool,
          reason, message string) error
      ```
      simple callback helper, no generics; new
      `internal/cloudflare/credentials.go` —
      `Load(ctx, c client.Reader, ref CredentialsRef) (accountID, token
      string, err error)`, `CredentialsRef` adapter handles the
      Tunnel-namespace-derived-from-`spec.cloudflared.namespace` vs
      AccessPolicy-explicit-`credentialsSecretRef.namespace` divergence; the
      four reconcilers embed `Base`, per-CR `cloudflareClient(...)` helpers
      deleted (~250–350 LoC removed); Exposure `Reconcile` body refactored so
      each step mutates in-scope `status` and short-circuit steps `return`
      directly — drop the `(status, done, result, err)` tuple at
      `cloudflareexposure_controller.go:122–139`.
    - Implements: review §1.1, §1.8.
    - Tests: existing envtests for the four reconcilers remain green without
      modification (contract = conditions + events + CF state, not helper
      shape). New: `TestBaseSetReadyIdempotent`,
      `TestCredentialsLoaderTunnelNamespace`,
      `TestCredentialsLoaderExplicitNamespace`.

14. **Requeue/error policy standardisation.**
    - Files: across the four reconcilers — `RequeueAfter: 30*time.Second`
      for `CredentialsMissing`, `TunnelNotReady`, `PolicyNotReady`,
      `HostnameConflict`, `ForeignResource`, `BlockedByExposures`,
      `BlockedByRoutes`; `fmt.Errorf("...: %w", err)` (exponential backoff)
      for `*Failed` / API-call / write-failure reasons; document policy in a
      new `## Requeue policy` subsection of `AGENTS.md ## Reconciliation
      Rules`.
    - Implements: review §1.15 (decision: split by failure class).
    - Tests: `TestExposureHostnameConflictRequeuesAt30s`,
      `TestExposureCloudflareWriteFailureBacksOff`.

15. **`internal/naming` shrink + final package reshape.**
    - Files: `internal/naming/` retains only `TokenSecretName`,
      `DaemonSetName`, `AccessAppName`, `Finalizer`; `tags.go` already
      deleted by subtask 12; structural endpoint of subtasks 12+13. Verify
      against review §4 proposed layout.
      `internal/controller/conditions.go` move to `api/v1alpha1/` (review
      §3.6) — out of scope here, flag for post-MVP.
    - Implements: review §4.
    - Tests: `go build ./...`, `go vet ./...`, `go test ./...` green.

16. **Test reshape + linter re-enable.**
    - Files: replace the four `internal/controller/*_validation_test.go`
      files with one table-driven
      `internal/controller/crd_admission_test.go` that builds a manifest per
      table row, applies via envtest, asserts admission outcome (~half the
      LoC, review §6); extract `test/live/cloudflare_smoke_test.go` (1,175
      lines) harness into `test/live/harness.go`, leaving `*_test.go` thin
      with just lifecycle/preflight cases (review §6); drop the `dupl`
      suppression on `internal/*` in `.golangci.yml` lines ~42–46 (review
      §3.7); add head-of-file comment on
      `internal/controller/httproute_discovery.go` documenting the
      deliberate `unstructured` choice and discouraging a "helpful"
      `gateway-api/v1` upgrade (review §1.16); add a block comment on
      `charts/cfzt-operator/templates/clusterrole.yaml` explaining that the
      `gateway.networking.k8s.io/httproutes` grant is unconditional and
      harmless when the CRD is absent (review §8).
    - Implements: review §3.6 deferred, §3.7, §6, §8, §1.16.
    - Tests: `make test` green with the consolidated `crd_admission_test.go`
      (table sweep must cover every CEL rule the four deleted files
      covered); live smoke unaffected by harness split (no behaviour change,
      only file relocation).

17. **Live smoke audit + alignment.**
    - Run after subtasks 1–16 have landed. Re-read
      `test/live/cloudflare_smoke_test.go` + the freshly extracted
      `test/live/harness.go` (subtask 16) against every behaviour-visible
      change introduced earlier in the slice. Audit for:
      - subtask 4 — `IngressDocHash` skip-on-equal: verify any timing-based
        assertion tolerates the one-PUT-then-skip steady state (live smoke
        hits real CF, so observable only via reconcile cadence).
      - subtask 5 — `Tunnels.List(ctx, accountID, name string)` string
        signature, `Configurations.Get` removed, tag-ensure pushed inside
        `AccessApplications.Create/Update`: live-smoke direct calls into
        `h.cf.*` must compile against the new signatures and stop calling
        `AccessTags.Ensure` explicitly.
      - subtask 7 — `AccessApplication.PolicyUUIDs []string` read shape:
        any live-smoke assertion reading back the bound policy from a CF
        Access application must consume the slice (`PolicyUUIDs[0]`) rather
        than the old `PolicyUUID` scalar.
      - subtask 10 — `policyName` always `-cfzt`: any live-smoke
        `CloudflareAccessPolicy` fixture that sets `spec.policyName` and
        then asserts the CF-side name must expect the `-cfzt` suffix.
      - subtask 12 — ownership package rename: verify live smoke uses no
        stale naming ownership helpers.
      - subtask 14 — requeue policy split: re-read `waitTunnelReady`,
        `waitExposureConflictReason`, `waitTunnelRouteReady`,
        `waitTunnelRouteForeignReason`, `waitAccessPolicyReady` timeouts and
        adjust for the new 30s-floor / exponential-backoff cadence as
        applicable.
      - subtask 15 / 16 — package + harness split: confirm
        `test/live/harness.go` imports only public surface (no
        `internal/ownership`, no `internal/controller` types).
    - Files: `test/live/cloudflare_smoke_test.go`, `test/live/harness.go`,
      `hack/live-cloudflare-local.sh`, `.env.live.example` (only if env-var
      surface changed). Make targeted edits as the audit surfaces them; do
      not pre-bake speculative changes. Annotate each audit-driven edit
      inline with a `// Slice 6 adjustment: ...` comment for provenance.
    - Implements: cross-slice integration hygiene — live smoke remains the
      MVP gate per `spec.md ## CI / CD`.
    - Tests: `bash hack/live-cloudflare-local.sh preflight` clean;
      `bash hack/live-cloudflare-local.sh lifecycle` green end-to-end across
      all five existing phases (Slices 1–5). Any regression must be
      root-caused to the offending earlier subtask and fixed in the same
      PR — never silence the live smoke.

**Definition of done** (from `claude-review.md ## 9. Estimated impact` +
preserved invariants):

- Net Go LoC delta ≥ −400 (target: ~−435 per reviewer's table). Adds in
  `internal/ownership/` (+~120) and `internal/controller/base.go` (+~80)
  compensated by removals across the four reconcilers and `config/`.
- `make manifests generate && git diff --exit-code` clean.
- `make helm-sync-crds && git diff --exit-code` clean.
- `make test` green. `go test ./...` green.
- `helm lint charts/cfzt-operator` clean.
- `golangci-lint run` clean with `dupl` re-enabled on `internal/*`.
- `ci.yaml` green.
- Live-smoke `hack/live-cloudflare-local.sh lifecycle` green across all five
  existing phases.
- Behaviour-visible changes confined to: managed Access policy CF name now
  always ends in `-cfzt` (subtask 10); one fewer Cloudflare API call per
  Tunnel reconcile when ingress doc unchanged (subtask 4); reconcile
  failure cadence now backoff-vs-30s per class (subtask 14, documented).
- No spec.md decision (D1–D25) altered. CR shapes unchanged except: new
  optional `CloudflareTunnelStatus.IngressDocHash` field (subtask 4);
  `ObservedTunnelUid` removed (subtask 1, unused). CRD storage version
  unchanged (`v1alpha1`).

Subtask-derived additions: `TestEnqueueNamedExtracts`,
`TestRealClientZoneCacheServesAcrossInstances`, `TestRealClientReuse`,
`TestTunnelConfigUpdateSkippedWhenUnchanged`,
`TestTunnelConfigUpdateOnDrift`, `TestMapAPIErrorNotFound`,
`TestFakeAccessApplicationsEnsuresTagsImplicitly`, `TestTunnelsListString`,
`TestAccessApplicationForeignPolicyAttachmentDriftsBack`,
`TestAccessPolicyNameAlwaysSuffixed`, `TestCloudflareTunnelImageRegexPort`,
`TestOwnerCommentRoundTrip`, `TestOwnerCompactCommentRoundTrip`,
`TestOwnerAccessTagChunkRoundTrip`, `TestOwnerMatchesForeign`,
`TestBaseSetReadyIdempotent`, `TestCredentialsLoaderTunnelNamespace`,
`TestCredentialsLoaderExplicitNamespace`,
`TestExposureHostnameConflictRequeuesAt30s`,
`TestExposureCloudflareWriteFailureBacksOff` all pass.

**Risks**

- Subtask 13 (`Base` + step-pattern refactor) has the biggest blast radius.
  Land subtasks 1–12 first so smaller cleanups don't fight rebases inside
  the four reconcilers. Reviewer's "ship last" guidance (§10 item 7)
  preserved by the ordering above.
- `policyName` always-`-cfzt` (subtask 10) is a one-time behaviour change
  for any pre-MVP user who set `spec.policyName` without expecting the
  suffix. Pre-MVP, so deemed acceptable; subtask 10 also patches `spec.md`
  and the CFAP `PolicyName` docstring.
- Subtask 11 deletes `config/manager/` and `make deploy`/`install`/`uninstall`
  targets. Chart is the contract (D14/D17) and `release.yaml` publishes the
  chart, but developer muscle memory will break. Document in the same PR
  commit body and remove any `AGENTS.md` command-table references.
- Subtask 4 (`IngressDocHash`) is a status-field add; on operator upgrade
  the hash is empty so the first reconcile after upgrade always re-PUTs the
  config, then converges to the skip-on-equal steady state. No data loss.
- Subtask 12 ownership-tag refactor must round-trip every existing CF
  resource the operator owns: long-form DNS comments, compact route
  comments, chunked Access-app tag lists. Pre-merge
  `hack/live-cloudflare-local.sh lifecycle` is mandatory and subtask 17
  re-audits.
- Subtask 6 event-recorder switch: signature changes from
  `Eventf(obj, nil, type, reason, reason, fmt, args...)` to
  `Eventf(obj, type, reason, fmt, args...)`. Compile errors surface missed
  call sites at build time.
- Subtask 15/16 package + harness split must not leak internal types into
  `test/live/`. Keep `test/live/cloudflare_smoke_test.go` import paths
  stable; `internal/ownership` is reachable only via `h.cf.*` wrapper.

## 4. Bootstrap subtasks (scaffold absent)

Run before Slice 1 subtask 1. Per `AGENTS.md ## Bootstrap`.

1. `kubebuilder init --domain reid.ee --repo github.com/andrewreid/cfzt-operator`. Commit scaffold.
2. `kubebuilder create api --group cfzt --version v1alpha1 --kind CloudflareTunnel --resource --controller`. Commit.
3. `kubebuilder create api --group cfzt --version v1alpha1 --kind CloudflareExposure --resource --controller`. Commit.
4. Edit generated `cmd/main.go`: enable leader election (D12), register both controllers, set `MaxConcurrentReconciles=1` on both (D19), wire `--zap-log-level`. Add HTTPRoute discovery placeholder (used in Slice 3) — for MVP scaffold leave the discovery branch as a TODO behind a build-time `false`.
5. Hand-write `charts/cfzt-operator/` per `spec.md ## Helm chart layout` (Chart.yaml, values.yaml, crds/ placeholder, templates per layout, NOTES.txt with D23 GitOps caveat).
6. Hand-write `.github/workflows/ci.yaml` (`go vet`, `golangci-lint`, `make manifests generate`, `make helm-sync-crds`, `git diff --exit-code`, `make test`, `helm lint`) and `release.yaml` (image to `ghcr.io/andrewreid/cfzt-operator:<tag>`, chart to `oci://ghcr.io/andrewreid/charts/cfzt-operator`). Per `spec.md ## CI / CD`.
7. Add envtest bootstrap to `Makefile` `test` target per `AGENTS.md ## envtest setup`. Confirm `make test` works against the empty scaffold.
8. Commit. Open Slice 1 with a working green CI baseline.

## 5. Out-of-plan deferrals

None blocking.

## 6. Verification

Per slice:

- `make manifests generate && git diff --exit-code` — fail on uncommitted generated drift (CI mirrors this).
- `make test` — unit + envtest. Each new test name listed in the slice subtasks must appear in the run.
- `go test ./...` after any reconciliation-semantics change (per `AGENTS.md ## Verification`).

Slice 1 smoke:

- `helm lint charts/cfzt-operator` clean.
- `helm install cfzt-operator charts/cfzt-operator -n cfzt-system --create-namespace` against a fresh kind cluster. Apply credentials Secret. Apply `CloudflareTunnel`. Confirm `status.tunnelId`, token Secret, DaemonSet, `Ready=True`.

Slice 2 end-to-end:

- Slice 1 smoke remains green.
- Apply a `CloudflareExposure` per `spec.md ## Primary user experience` minimal example. Wait for `Ready=True`. From outside the cluster: `curl -v https://<hostname>` — expect Cloudflare Access challenge page. Authenticate, confirm origin response.
- External-origin variant (D16): apply Home Assistant example, run same curl, confirm reachability.
- `kubectl delete cloudflareexposure <name>` — confirm DNS record, Access app, ingress rule all gone.
- `kubectl delete cloudflaretunnel <name>` while another Exposure exists — confirm `Ready=False, Reason=BlockedByExposures` and the tunnel is not deleted.

Slice 3 end-to-end:

- Apply Service + Exposure with `sourceRef` and no `origin`. Confirm reconcile uses derived `<svc>.<ns>.svc.cluster.local:<port>`.
- `kubectl delete service <name>` — confirm cascading Exposure deletion and CF cleanup.
- Restart operator on a cluster without `gateway.networking.k8s.io` CRD — confirm log line `HTTPRoute CRD not found, controller disabled` and no crash.

Slice 4 end-to-end:

- Slice 1–3 smoke remain green.
- Apply a `CloudflareAccessPolicy` per `spec.md ## CRD model ### CloudflareAccessPolicy` (decision `allow`, include rule `emailDomain: <your-domain>`). Wait for `Ready=True`; confirm `status.policyId` set and the policy appears in the Cloudflare dashboard with `managed-by=cfzt-operator` tag (or, if the SDK has no tag field, confirm the ID matches `status.policyId`).
- Apply a `CloudflareExposure` with `spec.access.policyRef.name: <policy-cr-name>`. Confirm `Ready=True` and the Access app binds the resolved UUID. `curl -v https://<hostname>` → Access challenge; authenticate, confirm origin response.
- `kubectl edit cloudflareaccesspolicy <name>` — change an include rule. Within one reconcile, dashboard shows updated rule; all referencing Exposures re-bind cleanly.
- `kubectl delete cloudflareaccesspolicy <name>` while an Exposure references it — confirm `Ready=False, Reason=BlockedByExposures`, policy CR not deleted, CF policy still present. Remove the referencing Exposure; confirm policy CR + CF policy are then deleted.
- Negative: create a CF Access policy by hand in the dashboard named `<policy-cr-name>-cfzt`, then apply the matching `CloudflareAccessPolicy` CR — confirm `Ready=False, Reason=ForeignPolicy`, dashboard policy untouched.

Slice 5 end-to-end:

- Slices 1–4 smoke remain green.
- Apply a `CloudflareTunnelRoute` referencing a tunnel with `status.tunnelId` and `spec.network: 172.16.0.0/24` (or an IPv6 CIDR). Confirm `Ready=True`; dashboard Networks → Routes shows the route bound to the tunnel with compact `managed-by=cfzt` source-uid in the comment.
- `kubectl edit cloudflaretunnelroute <name>` — change `spec.network` to a new CIDR; confirm the controller preflights for foreign target conflicts and then updates the CF route within one reconcile.
- `kubectl delete cloudflaretunnelroute <name>` — confirm CF route is removed.
- Negative: pre-create a CF route by hand for the same CIDR+VNet, then apply the matching `CloudflareTunnelRoute` CR — confirm `Ready=False, Reason=ForeignRoute`, dashboard route untouched.
- `kubectl delete cloudflaretunnel <name>` while a route references it — confirm `Ready=False, Reason=BlockedByRoutes`, tunnel CR remains. Remove the route; confirm tunnel CR + CF tunnel are then deleted (subject to existing Exposure-reference rules).
- `bash hack/live-cloudflare-local.sh lifecycle` (or `release.yaml` live job) — `TestCloudflareLifecycle` covers route create / idempotent reconcile across operator restart / foreign-route conflict refusal / cleanup, against real Cloudflare. Actual packet routing through the route is not asserted (no WARP client in kind); only the lifecycle on the Cloudflare side is verified.

CI gate (D18, `.github/workflows/ci.yaml`):

- All slices: PR must show green `ci.yaml`. No skipped tests. Generated-file drift gate must pass.
- Release: tag `vX.Y.Z` triggers `release.yaml` → image at `ghcr.io/andrewreid/cfzt-operator:<tag>`, chart at `oci://ghcr.io/andrewreid/charts/cfzt-operator`.
