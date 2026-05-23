---
name: cfzt-cr-authoring
description: Create, review, or validate CFZT operator Kubernetes YAML and GitOps manifests for cfzt.reid.ee/v1alpha1 resources. Use when working with CloudflareTunnel, CloudflareExposure, CloudflareAccessPolicy, CloudflareTunnelRoute, Cloudflare Access policy binding by UUID or managed policy name, Service or HTTPRoute sourceRef, external origins, DNS opt-out, cloudflared hostNetwork, or private tunnel CIDR routes.
---

# CFZT CR Authoring

Use this skill to produce accurate `cfzt.reid.ee/v1alpha1` manifests for the CFZT operator.

## Workflow

1. Identify the requested outcome and emit the minimal CR set:
   - Public hostname through a tunnel: `CloudflareTunnel` if missing, plus one `CloudflareExposure`.
   - Reusable Cloudflare Access rules: `CloudflareAccessPolicy`, then `CloudflareExposure.spec.access.policyRef.name`.
   - Private network routing: `CloudflareTunnelRoute` referencing an existing tunnel.
2. Prefer the current repo implementation when available:
   - Use `api/v1alpha1/*_types.go` as the field contract.
   - Use `README.md` examples as public-facing examples.
   - Treat older spec prose as secondary if it disagrees with implemented structs.
3. Emit plain Kubernetes YAML only. Do not include agent-only wrappers such as `rtk` in user-facing manifests or commands.
4. Include `metadata.namespace` only on namespaced resources. `CloudflareExposure` is namespaced; the other CFZT CRDs are cluster-scoped.
5. Avoid unsupported APIs: annotations as the user interface, Ingress source, WARP policy management, Cloudflare Gateway policy management, private DNS management, broad DNS management, validating webhooks, conversion webhooks, and generic Cloudflare resource management.

## References

Load only what is needed:

- `references/crd-contract.md`: resource scopes, ownership, conditions, deletion, and upgrade behavior.
- `references/field-rules.md`: required fields, defaults, validation rules, immutability, and gotchas.
- `references/recipes.md`: copy-ready examples for common tunnel, exposure, policy, sourceRef, external-origin, DNS opt-out, and route cases.

## Authoring Checks

Before returning manifests:

- Confirm `apiVersion: cfzt.reid.ee/v1alpha1` on every CFZT CR.
- Confirm Access-enabled Exposures set exactly one of `policyRef.uuid` or `policyRef.name`.
- Confirm Service `sourceRef` examples keep `hostname` and set at least `origin.protocol`.
- Confirm HTTPRoute `sourceRef` examples keep explicit `origin`.
- Confirm LAN/external-origin examples account for reachability, usually by setting `CloudflareTunnel.spec.cloudflared.hostNetwork: true` when node networking is required.
- Confirm route examples use one CIDR in `spec.network` and omit `virtualNetworkId` unless a non-default Cloudflare VNet UUID is intentionally required.
