# cfzt-operator architecture specification

## Purpose

`cfzt-operator` is a Kubernetes operator for publishing Kubernetes workloads through Cloudflare Zero Trust, Cloudflare Tunnel, and Cloudflare Access.

The primary goal is to let a cluster user expose a workload by adding a small set of annotations to a Kubernetes resource, in the same broad spirit as `external-dns`.

The operator should manage the Cloudflare-side lifecycle needed to make that exposure work:

- Cloudflare Tunnel lifecycle, where configured to do so.
- `cloudflared` connector workload lifecycle inside Kubernetes.
- Tunnel public hostname / published application route configuration.
- Cloudflare Access application creation and update.
- Cloudflare Access policy binding.
- Status reporting back into Kubernetes.
- Safe cleanup of Cloudflare resources owned by the operator.

The intended first implementation should be deliberately small and focused. It should not attempt to manage every Cloudflare Zero Trust feature. The first useful product is an annotation-driven workload exposure controller backed by a small number of explicit custom resources.

## Intended implementation stack

This project should be built from scratch using:

- Go.
- Kubebuilder.
- `controller-runtime`.
- Kubernetes CRDs.
- Cloudflare Go SDK where practical.

Existing open-source Cloudflare operator projects may be used as references for API calls, reconciliation patterns, CRD shapes, status handling, finalizers, and cloudflared deployment mechanics. The project should not begin by forking another operator unless a later design decision explicitly changes that.

## Design philosophy

The operator should be lightweight, explicit, and easy to reason about.

The key design choice is:

> Annotations are the user-facing convenience layer, but CRDs are the durable reconciliation layer.

An annotated `HTTPRoute` or `Service` should not directly perform all Cloudflare API work in that resource's controller. Instead, the annotation controller should translate the desired exposure into a `CloudflareExposure` custom resource. The `CloudflareExposure` controller should then reconcile Cloudflare resources.

This produces a clearer separation:

```text
HTTPRoute or Service annotations
  -> CloudflareExposure CR
  -> Cloudflare published route + Access application + Access policy binding
```

This keeps the annotation layer thin and allows future non-annotation workflows to create `CloudflareExposure` resources directly.

## MVP scope

The MVP should support:

- Kubebuilder project scaffold.
- One operator deployment.
- One API group, tentatively `cfzt.reid.ee`.
- `CloudflareTunnel` CRD.
- `CloudflareExposure` CRD.
- Optional `CloudflareAccessPolicy` CRD if this remains small enough; otherwise use references to existing Cloudflare policy IDs or names initially.
- Annotation parsing for `Service`.
- Annotation parsing for Gateway API `HTTPRoute`.
- Managed `cloudflared` connector workload for a tunnel.
- Creation or update of Cloudflare Tunnel public hostname routes.
- Creation or update of Cloudflare Access applications.
- Binding of Access policies to Access applications.
- Status conditions on CRDs.
- Finalizers for owned Cloudflare resources.

The MVP should defer:

- Ingress support.
- Private network CIDR routes.
- WARP routing.
- Device posture management.
- Gateway policy management.
- Full Cloudflare Gateway management.
- Multi-account support.
- Multi-zone auto-discovery if it adds complexity.
- Advanced Access rule composition.
- Operator Lifecycle Manager packaging.
- Cross-cluster federation.

## Cloudflare dashboard workflow being modelled

The operator is intended to model the manual Cloudflare Zero Trust dashboard workflow for a self-hosted application.

In the dashboard, this usually involves two related operations:

1. Create a published application route / tunnel public hostname.
   - Give it a name such as `Jellyfin`.
   - Define the public hostname, such as `jellyfin.reid.ee`.
   - Define the private origin hostname and port, such as `http://jellyfin.media.svc.cluster.local:8096`.
   - Attach it to a Cloudflare Tunnel / connector.

2. Create a Cloudflare Access application protecting that hostname.
   - Define an Access application named `Jellyfin`.
   - Bind the domain `jellyfin.reid.ee`.
   - Attach a policy such as `family-only`.

The operator should treat these as two separate Cloudflare resources reconciled from one Kubernetes exposure intent.

```text
CloudflareExposure
  -> Tunnel public hostname / published route
  -> Access application
  -> Access policy binding
```

## Primary user experience

### Service annotation example

A user should be able to annotate a `Service` directly:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: jellyfin
  namespace: media
  annotations:
    cfzt.reid.ee/enabled: "true"
    cfzt.reid.ee/name: "Jellyfin"
    cfzt.reid.ee/hostname: "jellyfin.reid.ee"
    cfzt.reid.ee/tunnel: "homelab"
    cfzt.reid.ee/origin-protocol: "http"
    cfzt.reid.ee/origin-port: "8096"
    cfzt.reid.ee/access-enabled: "true"
    cfzt.reid.ee/access-policy: "family-only"
spec:
  ports:
    - name: http
      port: 8096
      targetPort: 8096
```

The operator should derive the private origin as:

```text
http://jellyfin.media.svc.cluster.local:8096
```

### HTTPRoute annotation example

A user should also be able to annotate an `HTTPRoute`:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: jellyfin
  namespace: media
  annotations:
    cfzt.reid.ee/enabled: "true"
    cfzt.reid.ee/name: "Jellyfin"
    cfzt.reid.ee/tunnel: "homelab"
    cfzt.reid.ee/access-enabled: "true"
    cfzt.reid.ee/access-policy: "family-only"
spec:
  hostnames:
    - jellyfin.reid.ee
  parentRefs:
    - name: internal-gateway
      namespace: gateway-system
  rules:
    - backendRefs:
        - name: jellyfin
          port: 8096
```

For `HTTPRoute`, the operator should usually route Cloudflare to the Kubernetes Gateway service rather than directly to the backend service.

This keeps Gateway API responsible for in-cluster HTTP routing:

```text
Cloudflare public hostname
  -> cloudflared
  -> Kubernetes Gateway service
  -> HTTPRoute
  -> backend Service
```

The initial implementation may require the operator or `CloudflareTunnel` to be configured with a default Gateway origin, for example:

```text
http://gateway.gateway-system.svc.cluster.local:80
```

A later implementation can resolve Gateway listener and Service information more automatically.

## CRD model

### CloudflareTunnel

`CloudflareTunnel` represents a Cloudflare Tunnel and optionally the Kubernetes `cloudflared` connector workload used to run it.

Example shape:

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareTunnel
metadata:
  name: homelab
spec:
  accountIdSecretRef:
    name: cloudflare-credentials
    key: accountId
  apiTokenSecretRef:
    name: cloudflare-credentials
    key: apiToken
  tunnelName: homelab-rke2
  cloudflared:
    enabled: true
    mode: Deployment
    replicas: 2
    image: cloudflare/cloudflared:latest
    namespace: cfzt-system
  defaultGatewayOrigin:
    service: gateway.gateway-system.svc.cluster.local
    port: 80
    protocol: http
status:
  tunnelId: ""
  tokenSecretRef:
    name: ""
  conditions: []
```

The first implementation may support existing tunnel tokens before it supports creating tunnels. However, the architecture should allow the operator to create or adopt a tunnel later.

The tunnel controller should eventually be responsible for:

- Finding or creating the Cloudflare Tunnel.
- Storing Cloudflare tunnel ID in status.
- Obtaining or referencing a tunnel token.
- Creating or updating a Kubernetes Secret containing the token.
- Creating or updating the `cloudflared` Deployment.
- Ensuring the connector workload uses the desired token and image.
- Reporting connector readiness where possible.

### CloudflareExposure

`CloudflareExposure` is the central abstraction. It represents one public hostname exposure of one Kubernetes workload.

Example shape:

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: jellyfin
  namespace: media
spec:
  displayName: Jellyfin
  hostname: jellyfin.reid.ee
  sourceRef:
    apiVersion: v1
    kind: Service
    name: jellyfin
    namespace: media
  tunnelRef:
    name: homelab
  origin:
    protocol: http
    host: jellyfin.media.svc.cluster.local
    port: 8096
  access:
    enabled: true
    policyRef:
      name: family-only
status:
  cloudflare:
    tunnelId: ""
    accessApplicationId: ""
    publicHostnameRouteId: ""
  conditions: []
```

The `CloudflareExposure` controller should reconcile:

- The Cloudflare Tunnel public hostname / published application route.
- The Cloudflare Access application for the hostname.
- The binding between the Access application and the intended policy.
- Status IDs and conditions.
- Cleanup of owned resources on deletion.

### CloudflareAccessPolicy

This may be either MVP or post-MVP depending on complexity.

If included early, it should be intentionally small:

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareAccessPolicy
metadata:
  name: family-only
spec:
  decision: allow
  include:
    emails:
      - andrew@reid.ee
```

The goal is to avoid embedding complex Access policy JSON in workload annotations.

If `CloudflareAccessPolicy` is deferred, then `CloudflareExposure.spec.access.policyRef` can initially refer to an existing Cloudflare policy by name or ID.

## Annotation keys

Use a single stable annotation prefix:

```text
cfzt.reid.ee/
```

Proposed keys:

```text
cfzt.reid.ee/enabled
cfzt.reid.ee/name
cfzt.reid.ee/hostname
cfzt.reid.ee/tunnel
cfzt.reid.ee/origin-protocol
cfzt.reid.ee/origin-port
cfzt.reid.ee/origin-host
cfzt.reid.ee/access-enabled
cfzt.reid.ee/access-policy
```

`enabled` must be explicit. The controller should ignore resources without:

```yaml
cfzt.reid.ee/enabled: "true"
```

For `HTTPRoute`, `hostname` should be optional if exactly one `spec.hostnames` value exists.

For `Service`, `hostname` should be required.

For `Service`, `origin-port` should be optional if the service has exactly one port.

For `HTTPRoute`, origin fields should normally be derived from the selected tunnel's configured default Gateway origin.

## Controller responsibilities

### Annotation controllers

There should be lightweight controllers or watches for:

- `Service`.
- `HTTPRoute`.

Their job is only to:

1. Watch annotated resources.
2. Parse annotations.
3. Validate that enough information exists to create a `CloudflareExposure`.
4. Create or update the derived `CloudflareExposure`.
5. Add owner references where valid.
6. Remove or mark the derived exposure when the annotation is disabled or the source resource is deleted.

They should not directly call the Cloudflare API.

### CloudflareExposure controller

This controller owns the external Cloudflare reconciliation.

Its job is to:

1. Resolve the referenced `CloudflareTunnel`.
2. Resolve origin URL.
3. Ensure the Cloudflare Tunnel has a public hostname route for `spec.hostname`.
4. Ensure the Access application exists for `spec.hostname` when access is enabled.
5. Ensure the referenced Access policy is attached.
6. Update status with Cloudflare resource IDs.
7. Add and honour a finalizer.
8. Delete owned Cloudflare resources when the `CloudflareExposure` is deleted.

### CloudflareTunnel controller

This controller owns tunnel and connector lifecycle.

Its job is to:

1. Resolve Cloudflare credentials from Secrets.
2. Find, adopt, or create the Cloudflare Tunnel according to spec.
3. Ensure tunnel token Secret exists.
4. Ensure `cloudflared` workload exists if enabled.
5. Report readiness and Cloudflare tunnel ID in status.

## Ownership and deletion semantics

The operator must be conservative.

It must only delete Cloudflare resources that it created or clearly owns.

All Cloudflare resources created by the operator should be marked using whatever metadata Cloudflare supports, such as names, comments, tags, or deterministic naming. Where Cloudflare does not support tags, the operator should record Cloudflare IDs in Kubernetes status and use finalizers carefully.

Suggested ownership metadata values:

```text
managed-by=cfzt-operator
cluster=<configured cluster name>
namespace=<source namespace>
source-kind=<Service or HTTPRoute>
source-name=<name>
source-uid=<kubernetes uid>
```

If Cloudflare resources are manually modified, the controller should reconcile them back to desired state where safe. If a conflict is detected, such as another exposure already owning the same hostname, the controller should set a clear error condition and avoid destructive changes.

## Status and conditions

Every CRD should use Kubernetes-style conditions.

Useful `CloudflareExposure` conditions:

```text
Accepted
OriginResolved
TunnelReady
PublishedRouteReady
AccessApplicationReady
AccessPolicyBound
Ready
Error
```

Useful `CloudflareTunnel` conditions:

```text
CredentialsResolved
TunnelReady
TokenReady
ConnectorWorkloadReady
Ready
Error
```

Status should contain Cloudflare resource IDs, but annotations should not be used as state storage.

## Credentials

Cloudflare credentials should come from Kubernetes Secrets.

The MVP can use a single Cloudflare API token with enough permissions for:

- Tunnel read/write.
- Access application read/write.
- Access policy read/write or read-only, depending on whether policies are managed.
- DNS edit if DNS records are explicitly managed.

The credentials model should not hard-code a single global Secret if avoidable. Referencing credentials from `CloudflareTunnel` is probably sufficient for MVP.

## DNS management

Cloudflare Tunnel public hostname configuration may create the necessary public route depending on Cloudflare API behaviour and configuration, but DNS behaviour should be made explicit during implementation planning.

The operator should decide one of two approaches:

1. Manage DNS records itself.
2. Require DNS to be managed separately, for example by Cloudflare tunnel route configuration or `external-dns`.

Given the user's existing interest in `external-dns`, the first MVP should avoid duplicating DNS automation unless required by the Cloudflare published application API. If the tunnel public hostname API creates or implies the required DNS record, use that. If not, document the decision and add a narrow DNS reconciliation function.

## Origin resolution

For `Service` sources:

```text
<protocol>://<service>.<namespace>.svc.cluster.local:<port>
```

For `HTTPRoute` sources:

Use the referenced tunnel's default Gateway origin initially:

```text
<protocol>://<gateway-service>.<gateway-namespace>.svc.cluster.local:<port>
```

The operator should not initially attempt complex automatic Gateway resolution unless it remains simple. A simple explicit default Gateway origin is acceptable and keeps the MVP small.

## Reconcile idempotency

Every reconcile operation must be safe to run repeatedly.

The implementation should follow this pattern:

1. Observe Kubernetes desired state.
2. Observe Cloudflare actual state.
3. Compute desired Cloudflare state.
4. Create missing resources.
5. Update differing resources.
6. Avoid deleting anything unless finalizing an owned resource.
7. Write status.
8. Requeue only when necessary.

## Suggested package layout

The repository will likely start as a Kubebuilder project. A possible layout after scaffolding:

```text
api/v1alpha1/
  cloudflaretunnel_types.go
  cloudflareexposure_types.go
  cloudflareaccesspolicy_types.go

internal/controller/
  cloudflaretunnel_controller.go
  cloudflareexposure_controller.go
  service_annotation_controller.go
  httproute_annotation_controller.go

internal/annotations/
  parser.go
  service.go
  httproute.go

internal/cloudflare/
  client.go
  tunnels.go
  access_applications.go
  access_policies.go
  published_routes.go

internal/origin/
  service.go
  httproute.go

internal/naming/
  names.go

config/
  default/
  manager/
  rbac/
  crd/
```

## Naming conventions

Use deterministic names where possible.

For a source resource:

```text
namespace/name/kind
```

The derived `CloudflareExposure` name should be stable. It may be:

```text
<source-name>
```

when created in the same namespace as the source, or a sanitized version if collisions are possible.

Cloudflare Access application name should default to `cfzt.reid.ee/name`, falling back to the source resource name.

Cloudflare public hostname should always be explicit or derived unambiguously.

## Cross-compatibility between HTTPRoute and Service

The shared abstraction is `CloudflareExposure`.

Source-specific controllers should only differ in how they derive:

- hostname
- origin
- display name
- tunnel reference
- access policy reference

Once a `CloudflareExposure` is produced, the rest of the system should not care whether it came from a `Service`, `HTTPRoute`, or future `Ingress`.

This is critical to keeping the footprint low.

## Expected Codex implementation approach

Codex should not try to implement every feature in this specification in one pass.

Recommended first milestones:

1. Scaffold Kubebuilder project.
2. Add `CloudflareTunnel` and `CloudflareExposure` API types only.
3. Add CRD generation and manifests.
4. Implement annotation parser package with unit tests.
5. Implement `Service` annotation controller that creates `CloudflareExposure`.
6. Implement a fake/mock Cloudflare client interface.
7. Implement `CloudflareExposure` reconciliation against the fake client.
8. Add real Cloudflare client implementation.
9. Add `cloudflared` Deployment reconciliation for `CloudflareTunnel`.
10. Add `HTTPRoute` annotation controller.
11. Add status conditions and finalizers.
12. Add integration tests with envtest where practical.

The first working vertical slice should be:

```text
Annotated Service
  -> CloudflareExposure created
  -> Cloudflare published hostname route reconciled
  -> Cloudflare Access application reconciled
  -> Access policy attached
  -> status Ready
```

Only after that should HTTPRoute and tunnel lifecycle features be expanded.

## Non-goals for early implementation

The early implementation should not:

- Build a web UI.
- Replace `external-dns` broadly.
- Manage all Cloudflare DNS records.
- Manage arbitrary Zero Trust settings.
- Support every Access policy rule type.
- Implement multi-tenant SaaS semantics.
- Depend on Helm operators, Ansible operators, or Crossplane.

## Important design warning

The hard part of this operator is not making Cloudflare API calls.

The hard part is safe reconciliation:

- deciding what the operator owns
- preventing accidental deletion
- handling partial failure
- handling manual drift in the Cloudflare dashboard
- reporting useful status
- making repeated reconciles idempotent
- keeping annotation UX simple without losing explicit state

Prioritise correctness and clarity over broad feature coverage.
