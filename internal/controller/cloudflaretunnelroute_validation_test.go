package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
)

var _ = Describe("CloudflareTunnelRoute CRD validation", func() {
	validRoute := func(name string) *cfztv1alpha1.CloudflareTunnelRoute {
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

	It("TestCloudflareTunnelRouteCRDValidation accepts IPv4 and IPv6", func() {
		Expect(k8sClient.Create(ctx, validRoute("valid-route-v4"))).To(Succeed())
		ipv6 := validRoute("valid-route-v6")
		ipv6.Spec.Network = "fd00::/64"
		ipv6.Spec.VirtualNetworkId = ""
		Expect(k8sClient.Create(ctx, ipv6)).To(Succeed())
	})

	It("TestCloudflareTunnelRouteCRDValidation accepts explicit empty VNet", func() {
		route := validRoute("valid-route-empty-vnet")
		route.Spec.VirtualNetworkId = ""
		Expect(k8sClient.Create(ctx, route)).To(Succeed())
	})

	It("TestCloudflareTunnelRouteCRDValidation rejects bad IPv6 shape", func() {
		route := validRoute("bad-route-ipv6")
		route.Spec.Network = "not-a-cidr"
		err := k8sClient.Create(ctx, route)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.network"))
	})

	It("TestCloudflareTunnelRouteCRDValidation rejects bad VNet UUID", func() {
		route := validRoute("bad-route-vnet")
		route.Spec.VirtualNetworkId = "not-a-uuid"
		err := k8sClient.Create(ctx, route)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("virtualNetworkId"))
	})

	It("TestCloudflareTunnelRouteCRDValidation rejects overlong comment", func() {
		route := validRoute("bad-route-comment")
		route.Spec.Comment = "12345678901234567890123456789012345"
		err := k8sClient.Create(ctx, route)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("comment"))
	})

	It("TestCloudflareTunnelRouteCRDValidation rejects tunnelRef update", func() {
		route := validRoute("immutable-route-tunnel")
		Expect(k8sClient.Create(ctx, route)).To(Succeed())
		route.Spec.TunnelRef.Name = "other"
		err := k8sClient.Update(ctx, route)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("tunnelRef.name is immutable"))

		created := &cfztv1alpha1.CloudflareTunnelRoute{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "immutable-route-tunnel"}, created)).To(Succeed())
		Expect(k8sClient.Delete(ctx, created)).To(Succeed())
	})

	It("TestCloudflareTunnelRouteCRDValidation rejects clearing VNet after set", func() {
		route := validRoute("immutable-route-vnet-clear")
		Expect(k8sClient.Create(ctx, route)).To(Succeed())

		route.Spec.VirtualNetworkId = ""
		err := k8sClient.Update(ctx, route)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("virtualNetworkId cannot be cleared once set"))

		created := &cfztv1alpha1.CloudflareTunnelRoute{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "immutable-route-vnet-clear"}, created)).To(Succeed())
		Expect(k8sClient.Delete(ctx, created)).To(Succeed())
	})
})
