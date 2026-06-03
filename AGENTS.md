# AGENTS.md

Operator-maintainer handbook for agent sessions in this repo (Codex, Claude Code, others).

`spec.md` = product direction + binding decisions (D1–D26). Read it before non-trivial work. This file = how to operate; no architecture restated here. Current state + next subtask live in `docs/plan.md`.

---

## Resume

Scaffold + Slices 1–5 are in. To pick up work, read `docs/plan.md ## 2. Current state` for the active slice and outstanding subtasks, then the matching `### Slice N` block. Work one subtask per session; commit per subtask.

---

## Always-On

- Token economy = repo policy. Terse caveman default. Drop only when clarity or safety needs it.
- `rg` over broad reads. Targeted `Read` over file dumps.
- Consider delegation **before** coding (see `## Delegation`). Cheapest sufficient model wins.
- `spec.md` is intent, not proof — verify code exists before assuming. Decisions D1–D26 are binding; do not relitigate without the user.
- **Agent-executed shell commands prefix with `rtk`** (even inside `&&` chains). See `## RTK`.
- **User-facing docs must not include `rtk`** — humans see `make test`, `go test ./...`, `kubectl get pods`.

---

## Delegation

- Tiny / surgical (≤2 files, mechanical) → do locally.
- Bounded implementation (single controller/package) → scoped coder.
- Broad refactor or reconciliation-semantics work → orchestrator (architecture model) plans, scoped coders implement, reviewer audits diffs before final.
- Independent investigations → parallelise. Orchestrator owns final correctness — subagent output is input, not truth.
- Model tiers (`cavecrew`): scout/retrieval `haiku-4.5` / `gpt-5.4-mini medium`; architecture/SME `opus-4.7 high` / `gpt-5.5 high`; scoped coding `sonnet-4.6 high` / `gpt-5.4-mini high`.

---

## RTK

`rtk` = Rust Token Killer, a token-optimised CLI proxy for agent sessions. Filters command output when a filter exists, else passes through. Always safe to prefix.

```
rtk git status / log / diff / add / commit / push
rtk go test ./...            # failures-only
rtk gh pr view / checks / run list
rtk ls / read / grep / find
rtk kubectl get / logs   •   rtk docker logs / ps   •   rtk curl
rtk gain [--history]        # savings stats   •   rtk proxy <cmd>  # bypass filter
```

Prefix even inside chains: `rtk git add . && rtk git commit -m "x" && rtk git push`. If `rtk` is not on PATH (e.g. CI sandbox), fall back to the bare command and warn the user. Never copy `rtk` into README/docs/examples for human operators.

---

## Repo Map

Kubebuilder + controller-runtime, Go, `cloudflare-go/v4` (D13). Module: `github.com/andrewreid/cfzt-operator`.

```
api/v1alpha1/                CRD types (Tunnel, Exposure, AccessPolicy, TunnelRoute)
internal/controller/         tunnel, exposure, access-policy, tunnel-route reconcilers + Base
internal/tunnelconfig/       single-writer builder for tunnel ingress doc (D11)
internal/cloudflare/         SDK wrapper + fake client + zone cache
internal/origin/             Service + HTTPRoute origin derivation (Slice 3)
internal/naming/             resource naming helpers
internal/ownership/          source-uid comments + chunked Access tag ownership
internal/workload/           cloudflared DaemonSet + token Secret
cmd/                         manager entrypoint
config/                      kubebuilder-generated kustomize
charts/cfzt-operator/        hand-written Helm chart, OCI to GHCR (D14, D17)
.github/workflows/           ci.yaml + live-smoke.yaml + release.yaml (D18)
docs/plan.md                 implementation plan
```

No `internal/annotations/` — annotation UX is post-MVP (D5); the CR is the interface. Gateway API (`HTTPRoute`) support is conditional — enabled only when the CRD is discovered at startup.

---

## Commands

Agent execution uses `rtk`; human-facing docs omit it.

```
rtk make manifests          # CRDs + RBAC from kubebuilder markers
rtk make generate           # deepcopy etc.
rtk make test               # unit + envtest (auto-installs setup-envtest)
rtk make lint               # custom golangci-lint with module plugins
rtk make docker-build       # operator image
rtk make run                # run controller against current kubeconfig
rtk make helm-package       # package OCI Helm chart (D14)
rtk helm lint charts/cfzt-operator
```

Regenerate manifests + deepcopy after any `api/v1alpha1` change and commit the generated output — CI fails on uncommitted drift. `make test` handles envtest setup automatically; if it ever doesn't, fix the target.

---

## Commits & PRs

- Use `caveman-commit` / `/caveman-commit`. Conventional Commits, subject ≤50 chars, body only when "why" isn't obvious.
- One logical change per commit; prefer one commit per slice subtask. Generated files (`zz_generated*.go`, CRD YAML, deepcopy) commit alongside the `api/` change that produced them.
- `main` is protected. All work goes through a feature branch → PR → required validation green before merge.
- Branch names are human, work-descriptive: `fix/orphan-recovery`, `feat/access-policy-recovery`. No agent/tool prefixes.
- **No agent attribution** anywhere (commits, branches, PRs, docs, comments, trailers, "generated by") unless the user explicitly asks.

---

## Update policy

- **Decisions D1–D26 in `spec.md`** change only with user sign-off. If one looks wrong mid-implementation, stop and ask — do not silently work around.
- Other `spec.md` sections (CRD examples, package layout, RBAC table) may be corrected in the same PR as the discovering change; note it in the commit message.
- Update **`AGENTS.md`** when operational reality drifts (new tooling, command, convention). Update **`docs/plan.md`** every slice — current state, next subtask, blockers.

---

## Code Rules

- `CloudflareExposure` is the workload interface (D5). `CloudflareTunnel` owns tunnel + connector lifecycle.
- Exposure controller MUST NOT call the tunnel-configurations endpoint (D11); it enqueues the owning tunnel via the D20 watch. Tunnel controller is the sole writer of the tunnel-config doc — reads all referencing Exposures, computes the full ingress doc, PUTs it. **Last-write-wins**, no etag.
- All controllers `MaxConcurrentReconciles=1` (D19). Leader election ON (D12) — single writer per process; add no per-process locks.
- Cloudflare API access goes through the `internal/cloudflare` interface. Controllers never import `cloudflare-go/v4` directly. Fake client lives beside the real one. Use the [Cloudflare MCP server](https://github.com/cloudflare/mcp-server-cloudflare) for SDK lookups.
- Small interfaces, explicit code, composition over wrappers. No premature frameworking, no hidden magic, no global state beyond the cloudflare-client token bucket.
- Status conditions: `Ready` + `Progressing` only (D8); detail in `Reason`/`Message`. Finalizer string `cfzt.reid.ee/finalizer` on all owning CRDs (D21).
- Service and HTTPRoute paths converge through `CloudflareExposure`; source-kind-specific code stays in `internal/origin/`.
- Pin the cloudflared image as a Go constant in `internal/workload/`. Never `:latest` (validation marker rejects it in user spec).
- Determinism: tunnel ingress rules sorted by hostname; catch-all `http_status:404` always last.
- External origins (D16) are first-class — never gate `origin.host` on being a Service DNS suffix.

---

## Reconciliation Rules

**The hard part of this repo is reconciliation semantics, not Cloudflare API calls.** Optimise for correctness; feature count second.

- Every reconcile is idempotent (`spec.md ## Reconcile idempotency`). If you cannot prove a path idempotent, it is wrong.
- **Ownership.** Persistent CF objects carry a `source-uid`: Access app tags (chunked) and DNS record comment. **Tunnels** carry no CF-side tag — ownership is the generated name `<tunnelName>-cfzt-<hash8(uid)>` plus `status.tunnelId` (D9, superseded by D26 for multi-cluster). **Ingress rules are NOT tagged** (D11) — the doc is computed-from-K8s every reconcile.
- **Verify before mutate.** Tagged resource without a matching local owner → `Ready=False, Reason=ForeignResource`/`ForeignTunnel`/`ForeignPolicy`/`ForeignRoute`, no write, no delete.
- Hostname conflict (existing Access app / DNS record for the hostname has a different `source-uid`) → `Ready=False, Reason=HostnameConflict`, do not touch, requeue 30s. Ingress-rule conflict surfaces at builder time (two Exposures, same hostname → both `HostnameConflict`).
- Deletion only inside finalizers, only on resources the local CR demonstrably owns. Tunnel finalizer blocks while any Exposure (`BlockedByExposures`) or Route (`BlockedByRoutes`) references it.
- Same-namespace `sourceRef` → ownerReference Exposure→source; K8s GC cascades.
- Partial failure: persist what succeeded, surface what failed via `Ready=False, Reason=<specific>`, requeue. Never roll back successful CF writes "to be tidy".
- Drift on a tagged CF resource → reconcile rewrites to desired. Untagged / foreign-tagged for the same hostname → leave alone, surface conflict.
- Token rotation rolls cloudflared via the pod-template checksum annotation — never delete + recreate the DaemonSet.

### Requeue policy

- Waiting states requeue after 30s with no returned error: `CredentialsMissing`, `TunnelNotReady`, `PolicyNotReady`, `HostnameConflict`, `ForeignResource`, `BlockedByExposures`, `BlockedByRoutes`.
- Transient Cloudflare API / write failures return an error after status is updated, so controller-runtime exponential backoff applies (includes `*Failed` reasons).

---

## Cloudflare Rules

- One `CloudflareExposure` → three separate CF resources: published hostname route + Access application + DNS CNAME.
- Remotely-managed tunnels only (D1). Never generate `config.yml`; never call the configurations endpoint from the Exposure controller.
- Per-tunnel token (D4), fetched via SDK, stored in Secret `<tunnel-cr-name>-token` (key `token`) in the cloudflared namespace.
- DNS managed by default (D2): proxied CNAME → `<tunnelId>.cfargotunnel.com`. `dns.manage: false` → zero DNS records, no external-dns annotations ever.
- Zone resolution: longest zone-suffix match against the cached zone list. No PSL parsing.
- Access policy: `spec.access.policyRef` references an existing CF policy by `uuid` or a managed `CloudflareAccessPolicy` by `name` (D24) — exactly one when Access is enabled.
- Naming: Access app `<displayName | metadata.name>-cfzt`; Access policy `<policyName | metadata.name>-cfzt` (suffix always appended). Tunnel `<tunnelName>-cfzt-<hash8(uid)>`.
- Tunnel ownership: generated-name + `status.tunnelId` (D9). If `status.tunnelId` is unset, recover only an exact generated-name match; a legacy unsuffixed match → `Reason=ForeignTunnel`, no mutation.
- API token: minimum scopes per `spec.md ## Credentials`. `Zone:DNS:Edit` only when DNS is managed.
- Rate limiting: per-token bucket in `internal/cloudflare/client.go`; backoff on 429 / 5xx.
- Avoid Zero Trust scope creep. WARP, Gateway policies, posture, device management = out.

---

## Verification

- After a meaningful change: `rtk make test` for the touched package; `rtk go test ./...` if reconciliation logic was touched. Regenerate manifests + deepcopy first if `api/` changed.
- Any code change should pass live smoke before commit: `rtk bash hack/live-cloudflare-local.sh lifecycle`. Docs-only and other tiny non-substantive commits do not need local live smoke, but `.github/workflows/live-smoke.yaml` runs against real Cloudflare in CI before merge to `main`.
- Reconciliation-semantics changes (ownership, finalizers, tunnel-config builder) **require** envtest coverage — create CR → assert CF-fake state + CR status conditions. Not optional.
- Unit tests use the fake Cloudflare client (`internal/cloudflare/fake.go`); the real SDK is never reached in unit tests.
- Finalizer + deletion paths need explicit tests (untested deletion = production incident). `HostnameConflict`, `ForeignResource`, `BlockedByExposures` each need a dedicated test.
- No skipping tests to "fix later" — if a change breaks a test, fix the change or the test in the same PR. CI must be green before merge.

---

## Scope Boundaries

**This repo is not a general Cloudflare operator, nor a Zero Trust management platform.**

In scope (`spec.md ## MVP scope`):

- `CloudflareTunnel` — adopt/create tunnel, manage token Secret, run cloudflared DaemonSet.
- `CloudflareExposure` — published hostname route, proxied DNS CNAME, Access app, policy binding.
- `CloudflareAccessPolicy` — managed account-level Access policy, structured rule subset (D24).
- `CloudflareTunnelRoute` — private network CIDR-to-tunnel route (D25).
- Active-passive multi-cluster DR — per-Exposure `spec.failover` + DNS TXT lease, `--site-id` per process (D26, supersedes D9).
- External (non-K8s) origins (D16); `Ready`+`Progressing` conditions, finalizers, ownership tagging; Helm OCI distribution + CI/release.

Deferred (do not implement without user sign-off): annotation UX; Ingress source; WARP routing; Cloudflare Gateway; device posture; broad DNS beyond hostname CNAMEs; multi-account; multi-cluster active-active / federation; validating / conversion webhooks; generic policy DSLs; OLM / Crossplane / Helm-operator / Ansible-operator.

When in doubt: smaller surface, fewer features, sharper invariants. Ship correctness, not coverage.
