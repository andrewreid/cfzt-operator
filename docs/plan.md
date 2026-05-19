# cfzt-operator implementation plan

## 1. Context

Operational plan for shipping the cfzt-operator MVP in three slices (Tunnel/connector → Exposure → sourceRef derivation). Source of truth for architecture, decisions D1–D23, CRD shapes, RBAC, and DoD lists is `spec.md`. Operating handbook (bootstrap, commands, RTK, delegation, code rules) is `AGENTS.md`. This plan turns those into ordered, single-session subtasks with cited spec sections and test names.

Not covered: post-MVP work (annotation UX, AccessPolicy CRD, Ingress source, WARP, Gateway, OLM, multi-cluster). Decisions are not re-derived here — see `spec.md ## Decisions`.

## 2. Current state

No kubebuilder scaffold present. Only `spec.md`, `AGENTS.md`, `CLAUDE.md`, `.rtk/`. Git log: single commit `b0b25ce Add initial architecture specification`. Bootstrap subtasks (section 4) must run before Slice 1.

## 3. Slice plan

### Slice 1 — Tunnel and connector

Per `spec.md ## Implementation slices` → Slice 1. Outcome: `CloudflareTunnel` CR adopts/creates the Cloudflare tunnel, persists the token, runs the cloudflared DaemonSet.

**Subtasks**

1. **Define `CloudflareTunnel` types + validation markers.**
   - Files: `api/v1alpha1/cloudflaretunnel_types.go`, `api/v1alpha1/groupversion_info.go`.
   - Implements: `spec.md ## CRD model` (CloudflareTunnel), `## CRD validation` (CloudflareTunnel rules including CEL `credentialsSecretRef.namespace == cloudflared.namespace`, image `:latest` reject), kubebuilder markers from spec.
   - Tests: `api/v1alpha1` round-trip deepcopy test; `TestCloudflareTunnelCRDValidation` (envtest applies bad manifests, expects rejection — covers namespace-mismatch and `:latest` image cases).
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
   - Implements: `spec.md ## CRD model` (CloudflareTunnel responsibilities 1–5, 8), `## Ownership and deletion semantics` (tunnel comment tag + foreign-tunnel guard → `Reason=ForeignTunnel`), `AGENTS.md ## Reconciliation Rules`. D19 `MaxConcurrentReconciles=1`.
   - Tests: `TestTunnelCreate`, `TestTunnelAdopt`, `TestTunnelForeignTunnelRefuses` (extra coverage for adoption-with-foreign-tag path).

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
   - Implements: `spec.md ## Status and conditions` (D8 — `Ready` + `Progressing` only; reasons `CredentialsMissing`, `TunnelCreating`, `TunnelAdopted`, `TokenFetchFailed`, `WorkloadNotReady`, `Reconciled`), `## Observability` (events `AdoptedTunnel`, `TokenRotated`).
   - Tests: assertions woven into existing tunnel envtests; one focused `TestTunnelConditionsTransition`.

9. **Helm chart skeleton + CI green.**
   - Files: `charts/cfzt-operator/Chart.yaml`, `values.yaml`, `crds/cloudflaretunnel.yaml` (script-copied from `config/crd`), `templates/{deployment,serviceaccount,clusterrole,clusterrolebinding,role-leader-election,rolebinding-leader-election,NOTES.txt}`. `.github/workflows/ci.yaml` exercise.
   - Implements: D17 chart layout, `spec.md ## Helm chart layout`, `## RBAC` table (minus Service / HTTPRoute rows — those land in Slice 2/3), D23 NOTES.txt GitOps caveat.
   - Tests: `helm lint charts/cfzt-operator`; `make manifests generate && git diff --exit-code` clean in CI; `rtk make test` green.

**Definition of done** (from `spec.md ## Implementation slices ### Slice 1`):

- `kubectl apply` of a `CloudflareTunnel` against an empty Cloudflare account creates the tunnel, populates `status.tunnelId`, creates Secret `<name>-token`, creates DaemonSet `cloudflared-<name>`.
- `Ready=True` once DaemonSet has ≥1 ready pod.
- Reapply is a no-op.
- envtest tests pass: `TestTunnelCreate`, `TestTunnelAdopt`, `TestTunnelTokenRotation`, `TestTunnelFinalizerNoop`.
- `ci.yaml` green.
- `helm install` against a fresh cluster works.

Subtask-derived additions: `TestCloudflareTunnelCRDValidation`, `TestTunnelForeignTunnelRefuses`, `TestTunnelConditionsTransition` also pass; `helm lint` clean.

**Risks**

- SDK-shape uncertainty (G1 / D13). `cloudflare-go/v4` Tunnels.Token surface may differ from `spec.md ## Cloudflare SDK method mapping`. Use Cloudflare MCP server before writing real client; isolate inside `internal/cloudflare`.
- DaemonSet readiness racing initial token write — if DS rolls out before Secret exists, pods crash-loop. Order writes: Secret first, DS second; treat DS ready-replica count as the gate for `Ready=True` per spec Tunnel responsibility 8.
- Foreign-tunnel detection ambiguity: account already has a tunnel of the same name. Spec mandates `Reason=ForeignTunnel`, no mutation (`## Ownership and deletion semantics`). Test must cover the bare untagged-and-named-collision case.

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

CI gate (D18, `.github/workflows/ci.yaml`):

- All slices: PR must show green `ci.yaml`. No skipped tests. Generated-file drift gate must pass.
- Release: tag `vX.Y.Z` triggers `release.yaml` → image at `ghcr.io/andrewreid/cfzt-operator:<tag>`, chart at `oci://ghcr.io/andrewreid/charts/cfzt-operator`.
