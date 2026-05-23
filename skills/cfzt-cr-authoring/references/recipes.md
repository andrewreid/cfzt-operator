# CFZT Recipes

Replace example names, namespaces, hostnames, and IDs before use.

## Tunnel

Assumes Secret `cloudflare-credentials` exists in `cfzt-system` with keys `accountId` and `apiToken`.

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareTunnel
metadata:
  name: homelab
spec:
  tunnelName: homelab-rke2
  credentialsSecretRef:
    name: cloudflare-credentials
  cloudflared:
    namespace: cfzt-system
```

## Public Exposure Without Access

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: jellyfin
  namespace: media
spec:
  hostname: jellyfin.example.com
  tunnelRef:
    name: homelab
  origin:
    protocol: http
    host: jellyfin.media.svc.cluster.local
    port: 8096
  access:
    enabled: false
```

## Exposure With Existing Access Policy UUID

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: jellyfin
  namespace: media
spec:
  hostname: jellyfin.example.com
  tunnelRef:
    name: homelab
  origin:
    protocol: http
    host: jellyfin.media.svc.cluster.local
    port: 8096
  access:
    enabled: true
    policyRef:
      uuid: 00000000-0000-4000-8000-000000000001
```

## Managed Access Policy And Exposure

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareAccessPolicy
metadata:
  name: family-only
spec:
  credentialsSecretRef:
    namespace: cfzt-system
    name: cloudflare-credentials
  policyName: Family Only
  decision: allow
  rules:
    include:
      - emailDomain: example.com
  sessionDuration: 24h
---
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: jellyfin
  namespace: media
spec:
  hostname: jellyfin.example.com
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
```

## Service sourceRef Exposure

Use this when a same-namespace Service has exactly one port. The Exposure still needs `hostname`; the Service can derive origin host and port.

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: jellyfin
  namespace: media
spec:
  hostname: jellyfin.example.com
  tunnelRef:
    name: homelab
  sourceRef:
    apiVersion: v1
    kind: Service
    name: jellyfin
  origin:
    protocol: http
  access:
    enabled: false
```

## HTTPRoute sourceRef Exposure

Use this when the Gateway API CRD is installed before the operator starts and the route has exactly one hostname. Keep explicit origin fields.

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: jellyfin
  namespace: media
spec:
  tunnelRef:
    name: homelab
  sourceRef:
    apiVersion: gateway.networking.k8s.io/v1
    kind: HTTPRoute
    name: jellyfin
  origin:
    protocol: http
    host: jellyfin.media.svc.cluster.local
    port: 8096
  access:
    enabled: false
```

## External LAN Origin With hostNetwork

Use `hostNetwork: true` when cloudflared pods need node networking to reach a LAN target.

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareTunnel
metadata:
  name: homelab
spec:
  tunnelName: homelab-rke2
  credentialsSecretRef:
    name: cloudflare-credentials
  cloudflared:
    namespace: cfzt-system
    hostNetwork: true
---
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareExposure
metadata:
  name: homeassistant
  namespace: home
spec:
  hostname: ha.example.com
  tunnelRef:
    name: homelab
  origin:
    protocol: http
    host: 192.168.20.10
    port: 8123
  access:
    enabled: true
    policyRef:
      name: family-only
```

## DNS Opt-Out Tunnel

When `dns.manage: false`, Exposures still publish tunnel ingress rules and Access apps, but the operator creates no DNS records. DNS must be managed elsewhere.

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareTunnel
metadata:
  name: homelab-external-dns
spec:
  tunnelName: homelab-rke2
  credentialsSecretRef:
    name: cloudflare-credentials
  dns:
    manage: false
  cloudflared:
    namespace: cfzt-system
```

## Private CIDR Route

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareTunnelRoute
metadata:
  name: homelab-lan
spec:
  tunnelRef:
    name: homelab
  network: 192.168.20.0/24
  comment: homelab LAN
```

## Private CIDR Route With Explicit VNet

```yaml
apiVersion: cfzt.reid.ee/v1alpha1
kind: CloudflareTunnelRoute
metadata:
  name: homelab-lan-vnet
spec:
  tunnelRef:
    name: homelab
  network: 192.168.20.0/24
  virtualNetworkId: 00000000-0000-4000-8000-000000000002
  comment: homelab LAN
```
