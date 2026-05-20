# cfzt-operator implementation plan

## 1. Context

Operational plan for shipping the cfzt-operator MVP in three slices (Tunnel/connector → Exposure → sourceRef derivation). Source of truth for architecture, decisions D1–D23, CRD shapes, RBAC, and DoD lists is `spec.md`. Operating handbook (bootstrap, commands, RTK, delegation, code rules) is `AGENTS.md`. This plan turns those into ordered, single-session subtasks with cited spec sections and test names.

Slice 4 (`CloudflareAccessPolicy` CRD, D24) added under `## 3. Slice plan` after Slice 3 ships — managed Access policies are now in scope per `spec.md ## Decisions` D24.

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
`helm-sync-crds` targets added.

Completed on 2026-05-19:
- Subtask 1: `CloudflareTunnel` types + CRD validation markers. API amended
  so credentials Secret namespace is derived from `spec.cloudflared.namespace`
  instead of duplicated under `credentialsSecretRef`; image `:latest`
  validation uses bounded `endsWith` CEL. The CRD post-processing helper was
  removed. `rtk make test` is green.
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
  `rtk make test` is green.

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
  client-side name filtering, and update idempotence. `rtk make test`
  green.
- Slice 4 subtask 2: `CloudflareExposure.spec.access.policyRef.name` added
  (RFC 1123 subdomain pattern, maxLength 253) and the access CEL rule
  rewritten to require exactly one of `policyRef.uuid` or `policyRef.name`
  when `access.enabled: true`. `TestExposurePolicyRefOneOfValidation`
  covers uuid-alone, name-alone, both-set reject, neither-set-when-enabled
  reject. Existing "requires policy UUID" test message string updated to
  match the new CEL message. `rtk make test` green.
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
  `credentialsSecretRef.namespace`, missing `decision`. `rtk make test`
  green.

Next: manual live-cluster smoke. Live-cluster Slice 1, Slice 2, and Slice 3
smoke (`helm install`, real `CloudflareTunnel`, real `CloudflareExposure`,
Service `sourceRef`, Access challenge curl, external-origin curl, and source
deletion cascade) remain manual because they require a Kubernetes cluster and
Cloudflare credentials.

## 3. Slice plan

### Slice 1 — Tunnel and connector

Per `spec.md ## Implementation slices` → Slice 1. Outcome: `CloudflareTunnel` CR adopts/creates the Cloudflare tunnel, persists the token, runs the cloudflared DaemonSet.

**Subtasks**

1. **Define `CloudflareTunnel` types + validation markers.**
   - Files: `api/v1alpha1/cloudflaretunnel_types.go`, `api/v1alpha1/groupversion_info.go`.
   - Implements: `spec.md ## CRD model` (CloudflareTunnel), `## CRD validation` (CloudflareTunnel rules including single namespace source via `spec.cloudflared.namespace`, image `:latest` reject), kubebuilder markers from spec.
   - Tests: `api/v1alpha1` round-trip deepcopy test; `TestCloudflareTunnelCRDValidation` (envtest applies bad manifests, expects rejection — covers `:latest` image case and defaulting).
   - Run `rtk make manifests generate`; commit generated CRD + deepcopy.

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
   - Files: existing `charts/cfzt-operator/Chart.yaml`, `values.yaml`, `crds/{cloudflaretunnel,cloudflareexposure}.yaml` (copied from `config/crd` by `rtk make helm-sync-crds`), `templates/{deployment,serviceaccount,clusterrole,clusterrolebinding,role-leader-election,rolebinding-leader-election,NOTES.txt}`. `.github/workflows/ci.yaml` exercise.
   - Implements: D17 chart layout, `spec.md ## Helm chart layout`, `## RBAC` table (minus Service / HTTPRoute rows — those land in Slice 2/3), D23 NOTES.txt GitOps caveat.
   - Tests: `rtk helm lint charts/cfzt-operator`; `rtk make manifests generate && rtk git diff --exit-code` clean in CI; `rtk make test` green.

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
   - Run `rtk make manifests generate`; commit generated CRD + deepcopy.

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
    - Files: kubebuilder RBAC markers for `cloudflareaccesspolicies{,/status,/finalizers}`; regenerate `charts/cfzt-operator/templates/clusterrole.yaml`; `rtk make helm-sync-crds` copies new CRD to `charts/cfzt-operator/crds/`.
    - Implements: `spec.md ## RBAC` Slice 4 rows.
    - Tests: `rtk make manifests generate && rtk git diff --exit-code` clean; `rtk helm lint charts/cfzt-operator` clean.

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

## 4. Bootstrap subtasks (scaffold absent)

Run before Slice 1 subtask 1. Per `AGENTS.md ## Bootstrap`.

1. `rtk kubebuilder init --domain reid.ee --repo github.com/andrewreid/cfzt-operator`. Commit scaffold.
2. `rtk kubebuilder create api --group cfzt --version v1alpha1 --kind CloudflareTunnel --resource --controller`. Commit.
3. `rtk kubebuilder create api --group cfzt --version v1alpha1 --kind CloudflareExposure --resource --controller`. Commit.
4. Edit generated `cmd/main.go`: enable leader election (D12), register both controllers, set `MaxConcurrentReconciles=1` on both (D19), wire `--zap-log-level`. Add HTTPRoute discovery placeholder (used in Slice 3) — for MVP scaffold leave the discovery branch as a TODO behind a build-time `false`.
5. Hand-write `charts/cfzt-operator/` per `spec.md ## Helm chart layout` (Chart.yaml, values.yaml, crds/ placeholder, templates per layout, NOTES.txt with D23 GitOps caveat).
6. Hand-write `.github/workflows/ci.yaml` (`go vet`, `golangci-lint`, `make manifests generate && git diff --exit-code`, `make test`, `helm lint`) and `release.yaml` (image to `ghcr.io/andrewreid/cfzt-operator:<tag>`, chart to `oci://ghcr.io/andrewreid/charts/cfzt-operator`). Per `spec.md ## CI / CD`.
7. Add envtest bootstrap to `Makefile` `test` target per `AGENTS.md ## envtest setup`. Confirm `rtk make test` works against the empty scaffold.
8. Commit. Open Slice 1 with a working green CI baseline.

## 5. Out-of-plan deferrals

None blocking. Two soft observations flagged for awareness, neither requires a decision before code:

- `spec.md ## Decisions` references "D1–D21" in the design-warning section and "D1–D23" in the table itself. Mismatch is in spec, not in this plan; not a contradiction in intent. Will not be edited (`AGENTS.md ## Update policy` — D1–D23 stand; the prose reference will be corrected the next time spec is touched for a discovering change).
- `AGENTS.md ## Bootstrap` numbers decisions D1–D21 in a parenthetical; same minor drift. Same handling.

If the user wants either prose reference fixed proactively, flag separately.

## 6. Verification

Per slice:

- `rtk make manifests generate && rtk git diff --exit-code` — fail on uncommitted generated drift (CI mirrors this).
- `rtk make test` — unit + envtest. Each new test name listed in the slice subtasks must appear in the run.
- `rtk go test ./...` after any reconciliation-semantics change (per `AGENTS.md ## Verification`).

Slice 1 smoke:

- `rtk helm lint charts/cfzt-operator` clean.
- `rtk helm install cfzt-operator charts/cfzt-operator -n cfzt-system --create-namespace` against a fresh kind cluster. Apply credentials Secret. Apply `CloudflareTunnel`. Confirm `status.tunnelId`, token Secret, DaemonSet, `Ready=True`.

Slice 2 end-to-end:

- Slice 1 smoke remains green.
- Apply a `CloudflareExposure` per `spec.md ## Primary user experience` minimal example. Wait for `Ready=True`. From outside the cluster: `rtk curl -v https://<hostname>` — expect Cloudflare Access challenge page. Authenticate, confirm origin response.
- External-origin variant (D16): apply Home Assistant example, run same curl, confirm reachability.
- `rtk kubectl delete cloudflareexposure <name>` — confirm DNS record, Access app, ingress rule all gone.
- `rtk kubectl delete cloudflaretunnel <name>` while another Exposure exists — confirm `Ready=False, Reason=BlockedByExposures` and the tunnel is not deleted.

Slice 3 end-to-end:

- Apply Service + Exposure with `sourceRef` and no `origin`. Confirm reconcile uses derived `<svc>.<ns>.svc.cluster.local:<port>`.
- `rtk kubectl delete service <name>` — confirm cascading Exposure deletion and CF cleanup.
- Restart operator on a cluster without `gateway.networking.k8s.io` CRD — confirm log line `HTTPRoute CRD not found, controller disabled` and no crash.

Slice 4 end-to-end:

- Slice 1–3 smoke remain green.
- Apply a `CloudflareAccessPolicy` per `spec.md ## CRD model ### CloudflareAccessPolicy` (decision `allow`, include rule `emailDomain: <your-domain>`). Wait for `Ready=True`; confirm `status.policyId` set and the policy appears in the Cloudflare dashboard with `managed-by=cfzt-operator` tag (or, if the SDK has no tag field, confirm the ID matches `status.policyId`).
- Apply a `CloudflareExposure` with `spec.access.policyRef.name: <policy-cr-name>`. Confirm `Ready=True` and the Access app binds the resolved UUID. `rtk curl -v https://<hostname>` → Access challenge; authenticate, confirm origin response.
- `rtk kubectl edit cloudflareaccesspolicy <name>` — change an include rule. Within one reconcile, dashboard shows updated rule; all referencing Exposures re-bind cleanly.
- `rtk kubectl delete cloudflareaccesspolicy <name>` while an Exposure references it — confirm `Ready=False, Reason=BlockedByExposures`, policy CR not deleted, CF policy still present. Remove the referencing Exposure; confirm policy CR + CF policy are then deleted.
- Negative: create a CF Access policy by hand in the dashboard named `<policy-cr-name>-cfzt`, then apply the matching `CloudflareAccessPolicy` CR — confirm `Ready=False, Reason=ForeignPolicy`, dashboard policy untouched.

CI gate (D18, `.github/workflows/ci.yaml`):

- All slices: PR must show green `ci.yaml`. No skipped tests. Generated-file drift gate must pass.
- Release: tag `vX.Y.Z` triggers `release.yaml` → image at `ghcr.io/andrewreid/cfzt-operator:<tag>`, chart at `oci://ghcr.io/andrewreid/charts/cfzt-operator`.
