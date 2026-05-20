package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
)

var _ = Describe("CloudflareExposure CRD validation", func() {
	validExposure := func(name string) *cfztv1alpha1.CloudflareExposure {
		return &cfztv1alpha1.CloudflareExposure{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: cfztv1alpha1.CloudflareExposureSpec{
				Hostname:  name + ".example.com",
				TunnelRef: cfztv1alpha1.TunnelRef{Name: "homelab"},
				Origin:    &cfztv1alpha1.OriginSpec{Protocol: "http", Host: "jellyfin.default.svc.cluster.local", Port: 8096},
				Access: cfztv1alpha1.AccessSpec{
					Enabled: true,
					PolicyRef: cfztv1alpha1.AccessPolicyRef{
						UUID: "00000000-0000-4000-8000-000000000001",
					},
				},
			},
		}
	}

	It("TestCloudflareExposureCRDValidation accepts a valid exposure", func() {
		ensureNamespace(ctx, "default")
		obj := validExposure("valid-exposure")
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
	})

	It("TestCloudflareExposureCRDValidation rejects invalid hostname", func() {
		ensureNamespace(ctx, "default")
		obj := validExposure("bad-hostname")
		obj.Spec.Hostname = "Bad_Host"
		err := k8sClient.Create(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.hostname"))
	})

	It("TestCloudflareExposureCRDValidation rejects port out of range", func() {
		ensureNamespace(ctx, "default")
		obj := validExposure("bad-port")
		obj.Spec.Origin.Port = 70000
		err := k8sClient.Create(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.origin.port"))
	})

	It("TestCloudflareExposureCRDValidation requires policy UUID when Access is enabled", func() {
		ensureNamespace(ctx, "default")
		obj := validExposure("missing-policy")
		obj.Spec.Access.PolicyRef.UUID = ""
		err := k8sClient.Create(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("access.policyRef requires exactly one of uuid or name"))
	})

	It("TestExposurePolicyRefOneOfValidation accepts uuid alone", func() {
		ensureNamespace(ctx, "default")
		obj := validExposure("policyref-uuid-alone")
		obj.Spec.Access.PolicyRef = cfztv1alpha1.AccessPolicyRef{UUID: "00000000-0000-4000-8000-000000000002"}
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
	})

	It("TestExposurePolicyRefOneOfValidation accepts name alone", func() {
		ensureNamespace(ctx, "default")
		obj := validExposure("policyref-name-alone")
		obj.Spec.Access.PolicyRef = cfztv1alpha1.AccessPolicyRef{Name: "family-only"}
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
	})

	It("TestExposurePolicyRefOneOfValidation rejects both uuid and name", func() {
		ensureNamespace(ctx, "default")
		obj := validExposure("policyref-both")
		obj.Spec.Access.PolicyRef = cfztv1alpha1.AccessPolicyRef{
			UUID: "00000000-0000-4000-8000-000000000003",
			Name: "family-only",
		}
		err := k8sClient.Create(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("access.policyRef requires exactly one of uuid or name"))
	})

	It("TestExposurePolicyRefOneOfValidation rejects neither uuid nor name when enabled", func() {
		ensureNamespace(ctx, "default")
		obj := validExposure("policyref-neither")
		obj.Spec.Access.PolicyRef = cfztv1alpha1.AccessPolicyRef{}
		err := k8sClient.Create(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("access.policyRef requires exactly one of uuid or name"))
	})

	It("TestExposureCRDValidationSliceThreeRelaxed accepts Service sourceRef without origin", func() {
		ensureNamespace(ctx, "default")
		obj := validExposure("service-source")
		obj.Spec.SourceRef = &cfztv1alpha1.SourceRef{ApiVersion: "v1", Kind: "Service", Name: "jellyfin"}
		obj.Spec.Origin = nil
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
	})

	It("TestExposureCRDValidationSliceThreeRelaxed accepts HTTPRoute sourceRef without hostname", func() {
		ensureNamespace(ctx, "default")
		obj := validExposure("httproute-source")
		obj.Spec.SourceRef = &cfztv1alpha1.SourceRef{ApiVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute", Name: "jellyfin"}
		obj.Spec.Hostname = ""
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
	})

	It("TestExposureCRDValidationSliceThreeRelaxed rejects missing hostname without HTTPRoute", func() {
		ensureNamespace(ctx, "default")
		obj := validExposure("missing-hostname")
		obj.Spec.Hostname = ""
		err := k8sClient.Create(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("hostname is required"))
	})
})
