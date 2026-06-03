package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
)

var _ = Describe("CRD admission", func() {
	BeforeEach(func() {
		ensureNamespace(ctx, "default")
	})

	for _, tc := range []struct {
		name    string
		object  client.Object
		wantErr string
		check   func(client.Object)
	}{
		{"accepts a valid CloudflareTunnel", validAdmissionTunnel("valid-tunnel"), "", nil},
		{"accepts registry ports in cloudflared.image", tunnelWith("valid-registry-port-image", func(t *cfztv1alpha1.CloudflareTunnel) {
			t.Spec.Cloudflared.Image = "registry.local:5000/cloudflared:2025.1.0"
		}), "", nil},
		{"rejects cloudflared.image latest tag", tunnelWith("cel-latest-image", func(t *cfztv1alpha1.CloudflareTunnel) {
			t.Spec.Cloudflared.Image = "cloudflare/cloudflared:latest"
		}), "cloudflared.image must not use the :latest tag", nil},
		{"defaults typed CloudflareTunnel nested settings", tunnelWith("defaulted-tunnel", func(t *cfztv1alpha1.CloudflareTunnel) {
			t.Spec.Cloudflared = cfztv1alpha1.CloudflaredSpec{}
		}), "", func(obj client.Object) {
			t := obj.(*cfztv1alpha1.CloudflareTunnel)
			Expect(t.Spec.Cloudflared.Namespace).To(Equal("cfzt-system"))
			Expect(t.Spec.Dns.Manage).To(BeTrue())
			Expect(t.Spec.CredentialsSecretRef.Keys.AccountId).To(Equal("accountId"))
			Expect(t.Spec.CredentialsSecretRef.Keys.ApiToken).To(Equal("apiToken"))
		}},
		{"defaults YAML-shaped CloudflareTunnel nested settings", yamlDefaultedTunnel("yaml-defaulted-tunnel"), "", func(client.Object) {
			created := &cfztv1alpha1.CloudflareTunnel{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "yaml-defaulted-tunnel"}, created)).To(Succeed())
			Expect(created.Spec.Cloudflared.Namespace).To(Equal("cfzt-system"))
			Expect(created.Spec.Dns.Manage).To(BeTrue())
			Expect(created.Spec.CredentialsSecretRef.Keys.AccountId).To(Equal("accountId"))
			Expect(created.Spec.CredentialsSecretRef.Keys.ApiToken).To(Equal("apiToken"))
		}},
		{"accepts a valid CloudflareExposure", validAdmissionExposure("valid-exposure"), "", nil},
		{"rejects invalid exposure hostname", exposureWith("bad-hostname", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.Hostname = "Bad_Host"
		}), "spec.hostname", nil},
		{"rejects exposure origin port out of range", exposureWith("bad-port", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.Origin.Port = 70000
		}), "spec.origin.port", nil},
		{"rejects access enabled without applications", exposureWith("missing-apps", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.Access.Applications = nil
		}), "access.enabled=true requires spec.hostname and access.applications[]", nil},
		{"rejects removed top-level access policyRef", exposureWith("policyref-removed", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.Access.PolicyRef = cfztv1alpha1.AccessPolicyRef{UUID: "00000000-0000-4000-8000-000000000002"}
		}), "access.policyRef was removed in v1alpha1; use access.applications[]", nil},
		{"accepts access policy UUID alone", exposureWith("policyref-uuid-alone", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.Access.Applications[0].Policies[0].PolicyRef = cfztv1alpha1.AccessPolicyRef{UUID: "00000000-0000-4000-8000-000000000002"}
		}), "", nil},
		{"accepts access policy name alone", exposureWith("policyref-name-alone", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.Access.Applications[0].Policies[0].PolicyRef = cfztv1alpha1.AccessPolicyRef{Name: "family-only"}
		}), "", nil},
		{"rejects access policy UUID and name together", exposureWith("policyref-both", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.Access.Applications[0].Policies[0].PolicyRef = cfztv1alpha1.AccessPolicyRef{UUID: "00000000-0000-4000-8000-000000000003", Name: "family-only"}
		}), "policyRef requires exactly one of uuid or name", nil},
		{"rejects empty access policy reference when enabled", exposureWith("policyref-neither", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.Access.Applications[0].Policies[0].PolicyRef = cfztv1alpha1.AccessPolicyRef{}
		}), "policyRef requires exactly one of uuid or name", nil},
		{"rejects access application domain outside hostname", exposureWith("bad-app-domain", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.Access.Applications[0].Domains = []cfztv1alpha1.AccessApplicationDomain{"other.example.com"}
		}), "access.applications[].domains", nil},
		{"accepts Service sourceRef without origin", exposureWith("service-source", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.SourceRef = &cfztv1alpha1.SourceRef{ApiVersion: "v1", Kind: "Service", Name: "jellyfin"}
			e.Spec.Origin = nil
		}), "", nil},
		{"accepts HTTPRoute sourceRef without hostname", exposureWith("httproute-source", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.SourceRef = &cfztv1alpha1.SourceRef{ApiVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute", Name: "jellyfin"}
			e.Spec.Hostname = ""
			e.Spec.Access.Enabled = false
			e.Spec.Access.Applications = nil
		}), "", nil},
		{"rejects missing hostname without HTTPRoute sourceRef", exposureWith("missing-hostname", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.Hostname = ""
		}), "hostname is required", nil},
		{"rejects unsupported Service sourceRef apiVersion", exposureWith("bad-service-apiversion", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.SourceRef = &cfztv1alpha1.SourceRef{ApiVersion: "apps/v1", Kind: "Service", Name: "jellyfin"}
			e.Spec.Origin = nil
		}), "sourceRef.apiVersion", nil},
		{"rejects unsupported HTTPRoute sourceRef apiVersion", exposureWith("bad-httproute-apiversion", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.SourceRef = &cfztv1alpha1.SourceRef{ApiVersion: "gateway.networking.k8s.io/v1beta1", Kind: "HTTPRoute", Name: "jellyfin"}
			e.Spec.Hostname = ""
		}), "sourceRef.apiVersion", nil},
		{"accepts a valid CloudflareAccessPolicy", validAdmissionPolicy("valid-policy"), "", nil},
		{"rejects invalid Access policy decision", policyWith("bad-decision", func(p *cfztv1alpha1.CloudflareAccessPolicy) {
			p.Spec.Decision = "maybe"
		}), "spec.decision", nil},
		{"rejects Access rule with two fields set", policyWith("two-fields", func(p *cfztv1alpha1.CloudflareAccessPolicy) {
			p.Spec.Rules.Include = []cfztv1alpha1.AccessRule{{Email: "alice@example.com", EmailDomain: "example.com"}}
		}), "exactly one of", nil},
		{"rejects Access rule with zero fields set", policyWith("zero-fields", func(p *cfztv1alpha1.CloudflareAccessPolicy) {
			p.Spec.Rules.Include = []cfztv1alpha1.AccessRule{{}}
		}), "exactly one of", nil},
		{"rejects Access rule everyone false", everyoneFalsePolicy("everyone-false"), "exactly one of", nil},
		{"rejects empty Access policy rules", policyWith("empty-rules", func(p *cfztv1alpha1.CloudflareAccessPolicy) {
			p.Spec.Rules = cfztv1alpha1.AccessRules{}
		}), "at least one item", nil},
		{"rejects invalid sessionDuration", policyWith("bad-session", func(p *cfztv1alpha1.CloudflareAccessPolicy) {
			p.Spec.SessionDuration = "24hr"
		}), "spec.sessionDuration", nil},
		{"rejects missing Access policy credentials namespace", policyWith("missing-ns", func(p *cfztv1alpha1.CloudflareAccessPolicy) {
			p.Spec.CredentialsSecretRef.Namespace = ""
		}), "credentialsSecretRef.namespace", nil},
		{"rejects missing Access policy decision", policyWith("missing-decision", func(p *cfztv1alpha1.CloudflareAccessPolicy) {
			p.Spec.Decision = ""
		}), "spec.decision", nil},
		{"accepts valid CloudflareTunnelRoute IPv4", validAdmissionRoute("valid-route-v4"), "", nil},
		{"accepts valid CloudflareTunnelRoute IPv6", routeWith("valid-route-v6", func(r *cfztv1alpha1.CloudflareTunnelRoute) {
			r.Spec.Network = "fd00::/64"
			r.Spec.VirtualNetworkId = ""
		}), "", nil},
		{"accepts CloudflareTunnelRoute explicit empty VNet", routeWith("valid-route-empty-vnet", func(r *cfztv1alpha1.CloudflareTunnelRoute) {
			r.Spec.VirtualNetworkId = ""
		}), "", nil},
		{"rejects invalid CloudflareTunnelRoute CIDR", routeWith("bad-route-ipv6", func(r *cfztv1alpha1.CloudflareTunnelRoute) {
			r.Spec.Network = "not-a-cidr"
		}), "spec.network", nil},
		{"rejects invalid CloudflareTunnelRoute VNet UUID", routeWith("bad-route-vnet", func(r *cfztv1alpha1.CloudflareTunnelRoute) {
			r.Spec.VirtualNetworkId = "not-a-uuid"
		}), "virtualNetworkId", nil},
		{"rejects overlong CloudflareTunnelRoute comment", routeWith("bad-route-comment", func(r *cfztv1alpha1.CloudflareTunnelRoute) {
			r.Spec.Comment = "12345678901234567890123456789012345"
		}), "comment", nil},
		{"accepts a valid CloudflareExposure failover block", exposureWith("failover-valid", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.Failover = &cfztv1alpha1.FailoverSpec{Group: "jellyfin-dr", LeaseSeconds: 60}
		}), "", nil},
		{"defaults failover.leaseSeconds when omitted", failoverYAMLExposure("failover-default-lease", "jellyfin-dr"), "", func(client.Object) {
			created := &cfztv1alpha1.CloudflareExposure{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "failover-default-lease", Namespace: "default"}, created)).To(Succeed())
			Expect(created.Spec.Failover).NotTo(BeNil())
			Expect(created.Spec.Failover.LeaseSeconds).To(Equal(int32(60)))
		}},
		{"rejects failover.group below MinLength", exposureWith("failover-short-group", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.Failover = &cfztv1alpha1.FailoverSpec{Group: "ab"}
		}), "spec.failover.group", nil},
		{"rejects failover.group with invalid characters", exposureWith("failover-bad-group", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.Failover = &cfztv1alpha1.FailoverSpec{Group: "Jelly_Fin"}
		}), "spec.failover.group", nil},
		{"rejects failover block missing group", failoverYAMLExposure("failover-missing-group", ""), "spec.failover.group", nil},
		{"rejects failover.leaseSeconds below minimum", exposureWith("failover-lease-low", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.Failover = &cfztv1alpha1.FailoverSpec{Group: "jellyfin-dr", LeaseSeconds: 10}
		}), "spec.failover.leaseSeconds", nil},
		{"rejects failover.leaseSeconds above maximum", exposureWith("failover-lease-high", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.Failover = &cfztv1alpha1.FailoverSpec{Group: "jellyfin-dr", LeaseSeconds: 601}
		}), "spec.failover.leaseSeconds", nil},
	} {
		It(tc.name, func() {
			err := k8sClient.Create(ctx, tc.object)
			if tc.wantErr != "" {
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(tc.wantErr))
				return
			}
			Expect(err).NotTo(HaveOccurred())
			if tc.check != nil {
				tc.check(tc.object)
			}
			Expect(k8sClient.Delete(ctx, tc.object)).To(Succeed())
		})
	}

	for _, tc := range []struct {
		name    string
		object  client.Object
		mutate  func(client.Object)
		wantErr string
	}{
		{"rejects CloudflareTunnel tunnelName update", validAdmissionTunnel("immutable-tunnel-name"), func(obj client.Object) {
			obj.(*cfztv1alpha1.CloudflareTunnel).Spec.TunnelName = "renamed-tunnel"
		}, "tunnelName is immutable"},
		{"allows first HTTPRoute-derived hostname write", exposureWith("httproute-hostname-default", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.SourceRef = &cfztv1alpha1.SourceRef{ApiVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute", Name: "jellyfin"}
			e.Spec.Hostname = ""
			e.Spec.Access.Enabled = false
			e.Spec.Access.Applications = nil
		}), func(obj client.Object) {
			obj.(*cfztv1alpha1.CloudflareExposure).Spec.Hostname = "derived.example.com"
		}, ""},
		{"rejects CloudflareExposure hostname update", validAdmissionExposure("immutable-hostname"), func(obj client.Object) {
			obj.(*cfztv1alpha1.CloudflareExposure).Spec.Hostname = "changed.example.com"
		}, "hostname is immutable"},
		{"rejects CloudflareExposure tunnelRef update", validAdmissionExposure("immutable-tunnelref"), func(obj client.Object) {
			obj.(*cfztv1alpha1.CloudflareExposure).Spec.TunnelRef.Name = "other-tunnel"
		}, "tunnelRef.name is immutable"},
		{"rejects CloudflareExposure sourceRef update", exposureWith("immutable-sourceref", func(e *cfztv1alpha1.CloudflareExposure) {
			e.Spec.SourceRef = &cfztv1alpha1.SourceRef{ApiVersion: "v1", Kind: "Service", Name: "jellyfin"}
			e.Spec.Origin = nil
		}), func(obj client.Object) {
			obj.(*cfztv1alpha1.CloudflareExposure).Spec.SourceRef.Name = "other-service"
		}, "sourceRef is immutable"},
		{"rejects CloudflareAccessPolicy policyName update", policyWith("immutable-policy-name", func(p *cfztv1alpha1.CloudflareAccessPolicy) {
			p.Spec.PolicyName = "family-only-cfzt"
		}), func(obj client.Object) {
			obj.(*cfztv1alpha1.CloudflareAccessPolicy).Spec.PolicyName = "other-family-cfzt"
		}, "policyName is immutable"},
		{"rejects CloudflareTunnelRoute tunnelRef update", validAdmissionRoute("immutable-route-tunnel"), func(obj client.Object) {
			obj.(*cfztv1alpha1.CloudflareTunnelRoute).Spec.TunnelRef.Name = "other"
		}, "tunnelRef.name is immutable"},
		{"rejects CloudflareTunnelRoute VNet clearing", validAdmissionRoute("immutable-route-vnet-clear"), func(obj client.Object) {
			obj.(*cfztv1alpha1.CloudflareTunnelRoute).Spec.VirtualNetworkId = ""
		}, "virtualNetworkId cannot be cleared once set"},
	} {
		It(tc.name, func() {
			Expect(k8sClient.Create(ctx, tc.object)).To(Succeed())
			tc.mutate(tc.object)
			err := k8sClient.Update(ctx, tc.object)
			if tc.wantErr == "" {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring(tc.wantErr))
			}
			Expect(k8sClient.Delete(ctx, tc.object)).To(Succeed())
		})
	}
})

func validAdmissionTunnel(name string) *cfztv1alpha1.CloudflareTunnel {
	return &cfztv1alpha1.CloudflareTunnel{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cfztv1alpha1.CloudflareTunnelSpec{
			CredentialsSecretRef: cfztv1alpha1.CredentialsSecretRef{Name: "cloudflare-credentials"},
			TunnelName:           "homelab-rke2",
			Dns:                  cfztv1alpha1.DnsSpec{Manage: true},
			Cloudflared:          cfztv1alpha1.CloudflaredSpec{Namespace: "cfzt-system", Image: "cloudflare/cloudflared:2025.1.0"},
		},
	}
}

func tunnelWith(name string, mutate func(*cfztv1alpha1.CloudflareTunnel)) *cfztv1alpha1.CloudflareTunnel {
	obj := validAdmissionTunnel(name)
	mutate(obj)
	return obj
}

func yamlDefaultedTunnel(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cfzt.reid.ee/v1alpha1",
		"kind":       "CloudflareTunnel",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"credentialsSecretRef": map[string]any{"name": "cloudflare-credentials"},
			"tunnelName":           "homelab-rke2",
		},
	}}
}

func validAdmissionExposure(name string) *cfztv1alpha1.CloudflareExposure {
	return &cfztv1alpha1.CloudflareExposure{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: cfztv1alpha1.CloudflareExposureSpec{
			Hostname:  name + ".example.com",
			TunnelRef: cfztv1alpha1.TunnelRef{Name: "homelab"},
			Origin:    &cfztv1alpha1.OriginSpec{Protocol: "http", Host: "jellyfin.default.svc.cluster.local", Port: 8096},
			Access: cfztv1alpha1.AccessSpec{
				Enabled: true,
				Applications: []cfztv1alpha1.AccessApplicationTarget{{
					Name:    "root",
					Domains: []cfztv1alpha1.AccessApplicationDomain{cfztv1alpha1.AccessApplicationDomain(name + ".example.com")},
					Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{{
						PolicyRef: cfztv1alpha1.AccessPolicyRef{UUID: "00000000-0000-4000-8000-000000000001"},
					}},
				}},
			},
		},
	}
}

func exposureWith(name string, mutate func(*cfztv1alpha1.CloudflareExposure)) *cfztv1alpha1.CloudflareExposure {
	obj := validAdmissionExposure(name)
	mutate(obj)
	return obj
}

// failoverYAMLExposure builds an unstructured Exposure with an explicit
// failover block. When group is empty the failover map omits the key, so the
// CRD's required-field check fires. When non-empty leaseSeconds is omitted to
// exercise the +kubebuilder:default tag from the unstructured create path.
func failoverYAMLExposure(name, group string) *unstructured.Unstructured {
	failover := map[string]any{}
	if group != "" {
		failover["group"] = group
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cfzt.reid.ee/v1alpha1",
		"kind":       "CloudflareExposure",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"spec": map[string]any{
			"hostname":  name + ".example.com",
			"tunnelRef": map[string]any{"name": "homelab"},
			"origin":    map[string]any{"protocol": "http", "host": "jellyfin.default.svc.cluster.local", "port": int64(8096)},
			"access": map[string]any{
				"enabled": true,
				"applications": []any{
					map[string]any{
						"name":    "root",
						"domains": []any{name + ".example.com"},
						"policies": []any{
							map[string]any{
								"policyRef": map[string]any{"uuid": "00000000-0000-4000-8000-000000000001"},
							},
						},
					},
				},
			},
			"failover": failover,
		},
	}}
}

func validAdmissionPolicy(name string) *cfztv1alpha1.CloudflareAccessPolicy {
	return &cfztv1alpha1.CloudflareAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cfztv1alpha1.CloudflareAccessPolicySpec{
			CredentialsSecretRef: cfztv1alpha1.AccessPolicyCredentialsSecretRef{Name: "cloudflare-credentials", Namespace: "cfzt-system"},
			Decision:             "allow",
			Rules:                cfztv1alpha1.AccessRules{Include: []cfztv1alpha1.AccessRule{{EmailDomain: "example.com"}}},
		},
	}
}

func policyWith(name string, mutate func(*cfztv1alpha1.CloudflareAccessPolicy)) *cfztv1alpha1.CloudflareAccessPolicy {
	obj := validAdmissionPolicy(name)
	mutate(obj)
	return obj
}

func everyoneFalsePolicy(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cfzt.reid.ee/v1alpha1",
		"kind":       "CloudflareAccessPolicy",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"credentialsSecretRef": map[string]any{"name": "cloudflare-credentials", "namespace": "cfzt-system"},
			"decision":             "allow",
			"rules":                map[string]any{"include": []any{map[string]any{"everyone": false}}},
		},
	}}
}

func validAdmissionRoute(name string) *cfztv1alpha1.CloudflareTunnelRoute {
	return &cfztv1alpha1.CloudflareTunnelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cfztv1alpha1.CloudflareTunnelRouteSpec{
			TunnelRef:        cfztv1alpha1.TunnelRouteTunnelRef{Name: "homelab"},
			Network:          "172.16.0.0/24",
			VirtualNetworkId: "00000000-0000-4000-8000-000000000001",
			Comment:          "homelab LAN",
		},
	}
}

func routeWith(name string, mutate func(*cfztv1alpha1.CloudflareTunnelRoute)) *cfztv1alpha1.CloudflareTunnelRoute {
	obj := validAdmissionRoute(name)
	mutate(obj)
	return obj
}
