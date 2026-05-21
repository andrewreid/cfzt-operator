# AGENTS.md

Operator-maintainer handbook for future Codex / Claude Code / other agent sessions in this repo.

`spec.md` = product direction + binding decisions (D1–D24). Read it before non-trivial work. This file = how to operate. No architecture restated here.

---

## Bootstrap

Repo state varies. Check before assuming:

```
rtk ls
```

- **Only `spec.md` + `AGENTS.md` + `CLAUDE.md` present** → no scaffold yet. First implementation session must:
  1. `rtk kubebuilder init --domain reid.ee --repo github.com/andrewreid/cfzt-operator`
  2. `rtk kubebuilder create api --group cfzt --version v1alpha1 --kind CloudflareTunnel --resource --controller`
  3. `rtk kubebuilder create api --group cfzt --version v1alpha1 --kind CloudflareExposure --resource --controller`
  4. Edit generated `cmd/main.go` for leader-election ON, multi-controller registration.
  5. Hand-write `charts/cfzt-operator/` per `spec.md ## Helm chart layout`.
  6. Hand-write `.github/workflows/ci.yaml` + `release.yaml` per `spec.md ## CI / CD`.
  7. Commit slice-by-slice.

- **Scaffold present but no slice complete** → resume from `docs/plan.md`.

- **Slices in flight** → check `docs/plan.md` for current slice + outstanding subtasks.

Plan lives at `docs/plan.md` (in-repo, committed). Updated each slice.

---

## Always-On

- Token economy = repo policy. Terse caveman default. Drop only when clarity or safety needs it.
- Consider delegation **before** coding. Cheapest sufficient model wins.
- `rg` over broad reads. Targeted `Read` over file dumps.
- `spec.md` describes intent. Decisions table (D1–D24) is binding — do not relitigate without user.
- Verify implementation before assuming. spec ≠ proof code exists.
- **Agent-executed shell commands prefix with `rtk`.** Even inside `&&` chains. See `## RTK` below.
- **User-facing documentation must not include `rtk` prefixes.** Humans should see normal commands such as `make test`, `go test ./...`, and `kubectl get pods`.

Orchestration:

- User wants token-efficient orchestration. Orchestrator asks + reviews, does not bulk-code, when task framed that way.
- Use `cavecrew` delegation:
  - **Codex**: `gpt-5.4-mini medium` scout/retrieval • `gpt-5.5 high` architecture/SME planning • `gpt-5.4-mini high` scoped coding.
  - **Claude Code**: `haiku-4.5 medium` scout/retrieval • `opus-4.7 high` architecture/SME planning • `sonnet-4.6 high` scoped coding.
- Reviewer loop before final output.

---

## Delegation Policy

- Tiny / surgical (≤2 files, mechanical) → do locally.
- Bounded implementation (single controller, single package) → delegate to scoped coder.
- Broad refactor or reconciliation-semantics work → orchestrator (architecture model) plans, scoped coders implement.
- Independent investigations → parallelise.
- Reviewer audits diffs before final.
- Orchestrator owns final correctness. Subagent output is input, not truth.

---

## RTK

`rtk` = Rust Token Killer. Token-optimised CLI proxy for agent sessions only. Installed in this repo via the RTK skill. If filter exists for a command, RTK uses it; otherwise passes through unchanged. Always safe for agents to prefix local shell execution.

Do not copy `rtk` into README/docs/examples intended for human operators. When editing docs, show the underlying command without the agent-only wrapper.

Critical commands:

```
rtk git status / log / diff / add / commit / push        # 59-80% savings
rtk go test ./...                                        # failures-only (90%)
rtk gh pr view / pr checks / run list                    # 26-87%
rtk ls / read / grep / find                              # 60-75%
rtk kubectl get / logs                                   # 85%
rtk docker logs / ps                                     # 85%
rtk curl                                                 # 70%
```

Meta:

```
rtk gain              # cumulative savings stats
rtk gain --history    # per-command history
rtk discover          # scan transcripts for missed savings
rtk proxy <cmd>       # bypass filter (debug only)
```

Even inside chains when the agent is executing commands locally:

```
# wrong
git add . && git commit -m "x" && git push

# right
rtk git add . && rtk git commit -m "x" && rtk git push
```

If `rtk` is not on PATH (e.g. CI sandbox), fall back to bare command and warn the user.

---

## Repo Map

Kubebuilder + controller-runtime, Go, `cloudflare-go/v4` (D13). Module: `github.com/andrewreid/cfzt-operator`.

```
api/v1alpha1/                CRD types (CloudflareTunnel cluster-scoped, CloudflareExposure namespaced)
internal/controller/         tunnel + exposure reconcilers
internal/tunnelconfig/       single-writer builder for tunnel ingress doc (D11)
internal/cloudflare/         SDK wrapper + fake client + zone cache
internal/origin/             Service + HTTPRoute origin derivation (Slice 3)
internal/naming/             names + source-uid tag formatting
internal/workload/           cloudflared DaemonSet + token Secret
cmd/                         manager entrypoint
config/                      kubebuilder-generated kustomize
charts/cfzt-operator/        hand-written Helm chart, OCI to GHCR (D14, D17)
.github/workflows/           ci.yaml + release.yaml (D18)
docs/plan.md                 implementation plan
```

No `internal/annotations/`. Annotation UX is post-MVP (D5). CR is the interface.

Gateway API (`HTTPRoute`) support is conditional — controller only enables when CRD discovered at startup.

---

## Commands

Agent execution uses `rtk`; human-facing docs should omit it. Targets assume kubebuilder scaffold complete (see `## Bootstrap`).

```
rtk make manifests          # CRDs + RBAC from kubebuilder markers
rtk make generate           # deepcopy etc.
rtk make test               # unit + envtest (auto-installs setup-envtest binary)
rtk go test ./...           # raw test pass
rtk make docker-build       # operator image
rtk make run                # run controller against current kubeconfig
rtk make helm-package       # package OCI Helm chart (D14)
rtk helm lint charts/cfzt-operator
```

Regenerate manifests + deepcopy after any `api/v1alpha1` change. Commit generated output. CI fails on uncommitted generated drift.

### envtest setup

First run on a fresh worktree:

```
rtk go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
rtk setup-envtest use --bin-dir bin/envtest 1.30.x
export KUBEBUILDER_ASSETS=$(setup-envtest use --bin-dir bin/envtest -p path 1.30.x)
```

Makefile target `make test` handles this automatically. If it doesn't, add it.

---

## Commits & PRs

- Use `caveman-commit` skill / `/caveman-commit` for messages. Conventional Commits. Subject ≤50 chars. Body only when "why" isn't obvious.
- One logical change per commit. Prefer one commit per slice subtask.
- Generated files (`zz_generated_*.go`, CRD YAML, deepcopy) committed in the same commit as the `api/` change that produced them.
- Do not add "Codex" or "Claude" co-author notes or other additional data to the commit messages, branch names etc.

### Branching (pre-MVP)

Out of scope until MVP ships. Work on `main` directly is acceptable until then. Post-MVP: feature branches required, PR review required, branch protection on.

---

## Update policy (spec / AGENTS / code)

- **Decisions D1–D24 in `spec.md`** are not changed without user sign-off. If a decision turns out wrong mid-implementation, stop, ask, do not silently work around.
- **Other `spec.md` sections** (CRD examples, package layout, RBAC table, etc.) may be corrected in the same PR as the discovering change. Note the correction in the commit message.
- **`AGENTS.md`** updated when operational reality drifts from documented practice (new tooling, command, convention).
- **`docs/plan.md`** updated every slice — current state, next subtask, blockers.

---

## Code Rules

- `CloudflareExposure` is the workload interface (D5). `CloudflareTunnel` owns tunnel + connector lifecycle.
- Exposure controller MUST NOT call the tunnel-configurations endpoint (D11). Enqueues the owning tunnel via D20 watch.
- Tunnel controller is sole writer of the tunnel-config doc. Reads all referencing Exposures, computes full ingress doc, PUTs it. **Last-write-wins** — no etag.
- Tunnel controller `MaxConcurrentReconciles=1` (D19). Exposure controller `=1` in MVP.
- Cloudflare API access goes through `internal/cloudflare` interface. Controllers never import `cloudflare-go/v4` directly. Fake client lives next to the real one for tests.
- Use the [Cloudflare MCP server](https://github.com/cloudflare/mcp-server-cloudflare) for SDK method lookups — saves tokens vs reading the SDK source.
- Small interfaces. Explicit code over generic abstractions. No premature frameworking.
- Composition over inheritance-style wrappers.
- No hidden magic. No global state beyond the cloudflare-client token bucket.
- Status conditions: `Ready` + `Progressing` only (D8). Detail in `Reason`/`Message`.
- Finalizer string: `cfzt.reid.ee/finalizer` on all owning CRDs (D21).
- Service and HTTPRoute paths converge through `CloudflareExposure`. Source-kind-specific code is confined to `internal/origin/`.
- Pin cloudflared image as a Go constant in `internal/workload/`. Never `:latest`. Validation marker rejects `:latest` in user spec.
- Leader election ON (D12) — assume single writer per process. Do not add per-process locks.
- Determinism: tunnel ingress rules sorted by hostname; catch-all `http_status:404` always last.
- External origins (D16) are first-class — never gate `origin.host` on being a Service DNS suffix.

---

## Reconciliation Rules

**Hard part of this repo is reconciliation semantics, not Cloudflare API calls.** Optimise for correctness, feature count second.

- Every reconcile is idempotent. Pattern in `spec.md ## Reconcile idempotency`.
- Ownership tracked via per-resource `source-uid` tag on persistent CF objects (Tunnel comment, Access app tags, DNS record comment). **Ingress rules inside the tunnel-config doc are NOT tagged** (D11) — doc is computed-from-K8s every reconcile.
- **Verify before mutate.** Tagged resource without matching local UID → `Ready=False, Reason=ForeignResource`, no write, no delete.
- Hostname conflict (existing Access app or DNS record for hostname has different `source-uid`) → `Ready=False, Reason=HostnameConflict`. Do not touch. Requeue with backoff. Ingress-rule conflict surfaces at builder time (two Exposures, same hostname → both go `HostnameConflict`).
- Deletion only inside finalizers, only on resources the local CR demonstrably owns.
- Tunnel finalizer blocks while any `CloudflareExposure` references the tunnel (`Reason=BlockedByExposures`).
- Same-namespace `sourceRef` → add ownerReference Exposure→source. K8s GC cascades.
- Partial failure: write what succeeded, surface what failed via `Ready=False, Reason=<specific>`, requeue. Never roll back successful CF writes "to be tidy".
- Drift on tagged CF resource → reconcile rewrites to desired. Untagged or foreign-tagged for the same hostname → leave alone, surface conflict.
- Token rotation rolls cloudflared via pod-template checksum annotation. Never delete + recreate the DaemonSet.
- Repeated reconciles MUST be safe. If you cannot prove a code path is idempotent, it is wrong.

---

## Cloudflare Rules

- Published hostname route + Access application + DNS CNAME = three separate Cloudflare resources reconciled from one `CloudflareExposure`.
- Remotely-managed tunnels only (D1). Never generate `config.yml` for cloudflared. Never call configurations endpoint from Exposure controller.
- Per-tunnel token (D4). Fetched via SDK. Stored in operator-managed Secret `<tunnel-cr-name>-token` (key `token`) in cloudflared namespace.
- DNS managed by default (D2). Proxied CNAME → `<tunnelId>.cfargotunnel.com`. `dns.manage: false` → operator creates **zero** DNS records. No external-dns annotations emitted, ever.
- Zone resolution: longest zone-suffix match against cached zone list. No PSL parsing.
- Access policy: `CloudflareExposure.spec.access.policyRef` references either an existing CF policy by UUID or a managed `CloudflareAccessPolicy` by name (D24). Exactly one is required when Access is enabled.
- Access app naming: `<displayName | metadata.name>-cfzt` (suffix to avoid colliding with hand-created apps).
- API token: minimum scopes per `spec.md ## Credentials`. Add `Zone:DNS:Edit` only when DNS managed.
- Rate limiting: per-token bucket inside `internal/cloudflare/client.go`. Backoff on 429 / 5xx.
- Tunnel adoption: exact match on `spec.tunnelName` within account. Found + untagged → claim by writing operator marker. Found + foreign tag → `Reason=ForeignTunnel`, no mutation.
- Avoid Zero Trust scope creep. WARP, Gateway policies, posture, device management = out.

---

## Verification

- After meaningful change: `rtk make test` for the touched package; `rtk go test ./...` if reconciliation logic touched.
- Reconciliation-semantics changes (ownership, finalizers, tunnel-config builder) require envtest coverage. Not optional.
- Unit tests use the fake Cloudflare client (`internal/cloudflare/fake.go`). Real SDK never reached in unit tests.
- envtest for controller behaviour: create CR → assert CF fake state + CR status conditions.
- Finalizer + deletion paths must have explicit tests. Untested deletion = production incident.
- Hostname conflict, ForeignResource, BlockedByExposures: each must have a dedicated test.
- Regenerate manifests + deepcopy before running tests if `api/` touched.
- No skipping tests to "fix later". If a test is broken by a change, fix the change or fix the test in the same PR.
- CI (`.github/workflows/ci.yaml`) MUST be green before merge.

---

## Scope Boundaries

**This repo is not a general Cloudflare operator. Not a Zero Trust management platform.**

MVP scope (`spec.md ## MVP scope`):

- `CloudflareTunnel` CR — adopt/create tunnel, manage token Secret, run cloudflared DaemonSet.
- `CloudflareExposure` CR — published hostname route, proxied DNS CNAME, Access app, policy binding by UUID.
- `CloudflareAccessPolicy` CR — managed reusable account-level Access policy with structured rule subset (D24).
- External (non-K8s) origins (D16).
- `Ready` + `Progressing` conditions, finalizers, ownership tagging.
- Helm OCI distribution + CI/release pipeline.

Deferred (do not implement without user sign-off):

- Annotation-driven UX (post-MVP convenience layer).
- Ingress source.
- WARP routing.
- Cloudflare Gateway management.
- Device posture.
- Broad DNS management beyond hostname CNAMEs.
- Multi-account / multi-cluster abstractions.
- Validating / conversion webhooks.
- Generic policy DSLs.
- OLM packaging, Crossplane, Helm-operator, Ansible-operator dependencies.

When in doubt: smaller surface, fewer features, sharper invariants. Ship correctness, not coverage.
