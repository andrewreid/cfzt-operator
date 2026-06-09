# Testing

This repo uses three testing layers:

- fast unit tests for pure package behavior;
- controller/envtest tests for Kubernetes reconciliation semantics; and
- opt-in live smoke tests for the Cloudflare API, Helm chart, image, DNS, tunnel, Access, and cleanup paths.

Use `make test` as the default local gate after meaningful changes. It regenerates manifests and deepcopy code, formats, vets, installs envtest assets if needed, and runs the normal Go test set.

```sh
make test
```

CI runs the same broad gate, plus linting, generated-drift detection, and Helm linting.

## Normal Tests

The normal test suite is everything that runs without special build tags:

```sh
go test ./...
```

The important package-level split is:

- `internal/cloudflare`: real-client mapping and fake-client behavior. Unit tests must not call the Cloudflare API.
- `internal/tunnelconfig`, `internal/naming`, `internal/ownership`, `internal/workload`, and `api/v1alpha1`: deterministic builders, names, ownership markers, workload rendering, and API helpers.
- `internal/controller`: controller and CRD validation tests backed by envtest.

Controller tests use Ginkgo and controller-runtime envtest. The suite in `internal/controller/suite_test.go` starts a local API server and etcd, installs CRDs from `config/crd/bases`, registers the cfzt API scheme, and exposes a controller-runtime client to the tests. Individual controller tests use `internal/cloudflare.NewFake()` for Cloudflare state, then create Kubernetes resources and assert both Kubernetes status and fake Cloudflare side effects.

Reconciliation semantics need envtest coverage. In particular, finalizers, deletion paths, ownership checks, hostname conflicts, foreign resources, tunnel blocking, route conflicts, drift correction, and condition transitions should be asserted through the controller tests rather than only through package-level unit tests.

After changing `api/v1alpha1`, regenerate and commit the generated files with the API change:

```sh
make manifests
make generate
make helm-sync-crds
make test
```

## E2E Tests

Kubebuilder scaffolded e2e tests live under `test/e2e` and are behind the `e2e` build tag. They are not part of the default test target. They build the manager image, load it into kind, install the CRDs, deploy the controller, and check that the manager and metrics endpoint come up.

```sh
make test-e2e
```

These tests are mostly deployment smoke coverage. They do not exercise real Cloudflare reconciliation.

## Live Cloudflare Smoke Tests

The live smoke test entrypoints live in `test/live/cloudflare_smoke_test.go` and `test/live/failover_smoke_test.go`; shared Kubernetes and Cloudflare helpers live in `test/live/harness.go`. The package is behind the `live` build tag. It is intentionally excluded from normal test runs because it talks to a real Cloudflare account, creates real DNS records, creates real Zero Trust resources, installs the chart into kind, and depends on public DNS/Cloudflare propagation.

There are three tests:

- `TestCloudflarePreflight`: verifies the supplied Cloudflare credentials can list DNS records, Access policies, Access applications, and tunnels. It does not require a Kubernetes cluster.
- `TestCloudflareLifecycle`: installs the operator Helm chart into Kubernetes, creates live cfzt resources, verifies public and Access-protected hostnames, checks idempotency after no-op updates and an operator restart, verifies foreign-resource safety, then deletes the resources and waits for Cloudflare cleanup.
- `TestFailoverLifecycle`: installs the chart, creates a failover Exposure, verifies lease/CNAME behavior through initial promotion, peer takeover, automatic re-promotion, Manual `AwaitingPromotion`, force-promotion, and cleanup.

### What Lifecycle Covers

`TestCloudflareLifecycle` performs this flow:

1. Load configuration from environment variables.
2. Create or reuse the operator and smoke namespaces.
3. Create the `cloudflare-credentials` Secret for the operator.
4. Install the Helm chart with `helm upgrade --install`.
5. Deploy an `agnhost netexec` echo Service in the smoke namespace.
6. Create a managed `CloudflareAccessPolicy` and verify the policy ID and observed rules hash.
7. Create a `CloudflareTunnel`, verify the tunnel ID and token Secret reference, and wait for the cloudflared DaemonSet.
8. Create a `CloudflareTunnelRoute` and verify the real Cloudflare route and ownership comment.
9. Create one public `CloudflareExposure` and one Access-enabled `CloudflareExposure` with a root app plus a path override.
10. Wait for both Exposures to become `Ready=True`.
11. Call the public hostname and require HTTP 200 from the echo workload.
12. Call the Access hostname and the path-scoped Access URL without credentials and require an Access challenge or denial, not HTTP 200.
13. Reapply no-op spec updates, restart the operator Deployment, and verify the Cloudflare object IDs, route hashes, Access application names/ordered domains/policy bindings, and Tunnel ingress document hash do not change. This includes round-tripping the root app and path app domains in Cloudflare's returned order.
14. Create a foreign DNS CNAME for a conflict hostname, create a conflicting Exposure, and verify the operator reports `HostnameConflict` or `ForeignResource` without changing the foreign record.
15. Create a foreign Cloudflare tunnel route, create a conflicting `CloudflareTunnelRoute`, and verify the operator reports `ForeignRoute` without changing the foreign route.
16. Delete the Kubernetes resources, wait for finalizers, and verify managed DNS records, Access applications, Access policy, tunnel route, and tunnel are gone.

The harness uses a custom HTTP client that resolves through `1.1.1.1`, does not follow redirects, and disables TLS certificate verification. That keeps Access redirects visible and avoids local resolver cache surprises while Cloudflare DNS is settling.

### What Failover Covers

`TestFailoverLifecycle` exercises the D26 failover state machine against real Cloudflare DNS:

1. Install the operator Helm chart with a non-default `site.id`.
2. Create a tunnel and a failover Exposure with managed DNS.
3. Wait for local promotion to `Primary`, verify the TXT lease names the local site, then wait for the public CNAME to exist and point at the local tunnel.
4. Simulate peer takeover by writing a peer-held TXT lease and verify the local Exposure demotes to `Standby` without stealing the lease.
5. Let the peer lease expire under `promotionPolicy: Automatic`, verify the local site re-promotes, and wait for the CNAME to return to the local tunnel.
6. Switch to `promotionPolicy: Manual`, simulate another expired peer lease, and verify `Ready=False, Reason=AwaitingPromotion` with no lease steal.
7. Add a new `cfzt.reid.ee/force-promote` token, verify promotion, lease ownership, and CNAME target.
8. Delete the Kubernetes resources and verify the failover CNAME, TXT lease, and Cloudflare tunnel are absent.

The smoke waits for the actual public CNAME target after each promotion. `status.failover.role=Primary` proves lease/status state, but the public-serving path is only complete once Cloudflare DNS has the expected CNAME.

### Required Cloudflare Setup

Copy `.env.live.example` to `.env.live` for local runs:

```sh
cp .env.live.example .env.live
```

Fill in these required values:

```sh
export CF_ACCOUNT_ID="..."
export CF_API_TOKEN="..."
export CF_TEST_ZONE="example.com"
```

Set `CF_ZONE_ID` when the token can manage DNS records but cannot list zones:

```sh
export CF_ZONE_ID="..."
```

The token must be able to perform the operations tested by the harness: list/create/delete Cloudflare tunnels, read tunnel tokens, list/create/update/delete Access policies and self-hosted Access applications, list/create/delete DNS CNAME records in the test zone, list/create/update/delete DNS TXT records for failover leases, and list/create/delete tunnel private network routes. If `CF_ZONE_ID` is not set, it must also be able to list/resolve zones.

Use a disposable test zone or a delegated subdomain. The harness creates hostnames like:

```text
public-<run-id>-<attempt>.<CF_TEST_ZONE>
access-<run-id>-<attempt>.<CF_TEST_ZONE>
conflict-<run-id>-<attempt>.<CF_TEST_ZONE>
failover-<run-id>-<attempt>.<CF_TEST_ZONE>
_cfzt-lease.<hash8(group)>.<CF_TEST_ZONE>
```

The default private-route smoke CIDRs are:

```sh
export CF_SMOKE_ROUTE_CIDR="100.64.207.0/24"
export CF_SMOKE_ROUTE_CONFLICT_CIDR="100.64.208.0/24"
```

Override them in `.env.live` if those networks already exist in the Cloudflare account.

### Local Execution

The helper script is the preferred local entrypoint. It sources `.env.live`, can start Colima when configured to do so, creates or reuses a kind cluster, builds the operator image locally, loads it into kind, and runs the same Go live tests used by CI. When `CHART_REF` points at a local chart, the Go harness applies CRDs from that chart before `helm upgrade --install` so reused kind clusters do not keep stale Helm-managed CRDs.

Run preflight first:

```sh
bash hack/live-cloudflare-local.sh preflight
```

Run the full lifecycle smoke:

```sh
bash hack/live-cloudflare-local.sh lifecycle
```

Run the DR failover smoke after failover, DNS lease, or promotion-policy changes:

```sh
bash hack/live-cloudflare-local.sh failover
```

Clean up the local kind cluster and Colima runtime when finished:

```sh
bash hack/live-cloudflare-local.sh down
```

Useful `.env.live` local overrides:

```sh
export OPERATOR_NAMESPACE="cfzt-system"
export SMOKE_NAMESPACE="cfzt-smoke"
export KIND_CLUSTER="cfzt-live-local"
export IMAGE_REPOSITORY="cfzt-operator"
export IMAGE_TAG="live-local"
export CHART_REF="charts/cfzt-operator"
export LOCAL_DOCKER_RUNTIME="auto"
export COLIMA_PROFILE="default"
export COLIMA_START_ARGS="--cpu 4 --memory 8"
```

### Direct Go Execution

Direct `go test` execution is useful in CI-like environments or when you already have the Kubernetes cluster, image, chart, and environment prepared.

Preflight:

```sh
set -a
source .env.live
set +a
go test -tags=live ./test/live -run '^TestCloudflarePreflight$' -count=1 -timeout=5m -v
```

Lifecycle:

```sh
set -a
source .env.live
set +a
go test -tags=live ./test/live -run '^TestCloudflareLifecycle$' -count=1 -timeout=15m -v
```

Failover:

```sh
set -a
source .env.live
set +a
go test -tags=live ./test/live -run '^TestFailoverLifecycle$' -count=1 -timeout=15m -v
```

For direct lifecycle runs, the current kubeconfig must point at the target cluster, `helm` must be installed, and the chart/image settings must be valid for that cluster. With the default local values, the image must already be loaded into kind because the chart is installed with `image.pullPolicy=Never`.

### Release Workflow

`.github/workflows/release.yaml` is manually triggered with a version input such as `0.2.3`. It does not publish from tag pushes. The workflow validates the requested version, then runs all release gates before creating the git tag, pushing GHCR artifacts, or creating the GitHub Release:

- `go vet`, `make lint` (including the custom golangci-lint plugins and cache clean), generated drift check, `make test`, and `helm lint`.
- Package the candidate Helm chart with the requested version.
- Build a local candidate image and install the packaged candidate chart into kind.
- Run `TestCloudflarePreflight`.
- Create a fresh kind cluster, load the local candidate image, and run `TestCloudflareLifecycle` against the packaged candidate chart and real Cloudflare.
- Only after those gates pass, create the `v<version>` git tag, build the final multi-arch image on native `linux/amd64` and `linux/arm64` GitHub runners, merge the pushed digests into the release image tags, push the Helm chart, verify both published artifacts, and create the GitHub Release.

The release workflow and the ad-hoc `.github/workflows/live-smoke.yaml` workflow read Cloudflare values from repository secrets and variables:

```text
secrets.CF_ACCOUNT_ID
secrets.CF_API_TOKEN
secrets.CF_TEST_ZONE or vars.CF_TEST_ZONE
secrets.CF_ZONE_ID or vars.CF_ZONE_ID
vars.CF_SMOKE_ROUTE_CIDR
vars.CF_SMOKE_ROUTE_CONFLICT_CIDR
```

`CF_ZONE_ID` is optional only when the token can list zones. The route CIDR values are optional and default to the CGNAT ranges documented above. If preflight fails while resolving the zone or checking route collisions, fix the repository secret or variable before changing the harness.

The release workflow tests these candidate settings before publication:

```text
CHART_REF=./dist/cfzt-operator-<version>.tgz
IMAGE_REPOSITORY=cfzt-operator
IMAGE_TAG=release-candidate
```

Use the separate `live smoke` workflow when you want to exercise the main live lifecycle harness without creating a release. It builds and loads a local image, installs `charts/cfzt-operator`, and never pushes tags, packages, images, or GitHub Releases. The failover smoke is currently run with the local helper or direct `go test` command above.

### Development Snapshots

Use `.github/workflows/dev-artifacts.yaml` when Flux needs registry artifacts for cluster testing but the change is not ready for a semver release. The workflow publishes immutable development artifacts from GitHub Actions, building the image on native `linux/amd64` and `linux/arm64` runners and then merging the pushed digests into one manifest:

```text
image: ghcr.io/andrewreid/cfzt-operator:0.0.0-dev.sha-<short-sha>
chart: oci://ghcr.io/andrewreid/charts/cfzt-operator
chart version: 0.0.0-dev.sha-<short-sha>
appVersion: 0.0.0-dev.sha-<short-sha>
```

It runs automatically for pushes to `main`. It can also be run manually with `workflow_dispatch` for a selected branch or commit once the workflow exists on the default branch. For one-off branch publication before that, include `[publish-dev]` in the push commit message. The workflow refuses to overwrite an existing image or chart for the same commit-derived version.

Flux can pin a snapshot chart directly:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata:
  name: cfzt-operator-dev
  namespace: flux-system
spec:
  type: oci
  url: oci://ghcr.io/andrewreid/charts
  interval: 10m
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: cfzt-operator
  namespace: cfzt-system
spec:
  chart:
    spec:
      chart: cfzt-operator
      version: 0.0.0-dev.sha-<short-sha>
      sourceRef:
        kind: HelmRepository
        name: cfzt-operator-dev
        namespace: flux-system
  install:
    crds: Create
  upgrade:
    crds: CreateReplace
  values:
    image:
      tag: 0.0.0-dev.sha-<short-sha>
    site:
      id: <cluster-site-id>
```

The snapshot channel has the same CRD caveats as normal releases: breaking `v1alpha1` changes may require CR migration, and `upgrade.crds: CreateReplace` must be used deliberately.

### Cleanup and Failure Notes

The lifecycle and failover tests register cleanup with `t.Cleanup`, so it runs after test failure as long as the process is still alive. Cleanup deletes the cfzt Kubernetes resources first so their finalizers can remove owned Cloudflare resources, then it removes intentionally created foreign or peer resources directly. The failover cleanup also checks that the public CNAME, TXT lease, and Cloudflare tunnel are absent.

If a run is interrupted hard, inspect both Kubernetes and Cloudflare for leftover resources whose names include the run suffix. Local runs default the suffix from `GITHUB_RUN_ID`, `GITHUB_RUN_ATTEMPT`, or a `local-<unix timestamp>` fallback. GitHub Actions runs use the real run ID and attempt.

Common failure causes:

- missing or insufficient Cloudflare token scopes;
- missing `CF_ZONE_ID` when the token cannot list zones;
- private-route CIDR collision with existing Cloudflare routes;
- candidate image or chart not available to the test cluster;
- DNS or Access propagation taking longer than the harness timeout; and
- Cloudflare API throttling from repeated local or CI smoke attempts; and
- operator finalizer failures that leave Kubernetes resources deleting.
