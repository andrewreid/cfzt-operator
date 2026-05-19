/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
)

var _ = Describe("CloudflareTunnel CRD validation", func() {
	const validImage = "ghcr.io/cloudflare/cloudflared:2025.1.0"

	validBase := func(name string) *cfztv1alpha1.CloudflareTunnel {
		return &cfztv1alpha1.CloudflareTunnel{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Spec: cfztv1alpha1.CloudflareTunnelSpec{
				CredentialsSecretRef: cfztv1alpha1.CredentialsSecretRef{
					Name: "cloudflare-credentials",
				},
				TunnelName: "homelab-rke2",
				Dns: cfztv1alpha1.DnsSpec{
					Manage: true,
				},
				Cloudflared: cfztv1alpha1.CloudflaredSpec{
					Namespace: "cfzt-system",
					Image:     validImage,
				},
			},
		}
	}

	Context("image :latest rejection", func() {
		It("rejects when cloudflared.image ends with :latest", func() {
			obj := validBase("cel-latest-image")
			obj.Spec.Cloudflared.Image = "ghcr.io/cloudflare/cloudflared:latest"

			err := k8sClient.Create(ctx, obj)
			Expect(err).To(HaveOccurred(), "expected CEL rejection for :latest image")
			Expect(err.Error()).To(ContainSubstring("cloudflared.image must not use the :latest tag"))
		})
	})

	Context("valid object", func() {
		It("accepts a well-formed CloudflareTunnel", func() {
			obj := validBase("valid-tunnel")

			err := k8sClient.Create(ctx, obj)
			Expect(err).NotTo(HaveOccurred())

			By("cleanup")
			Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
		})

		It("accepts defaulted cloudflared settings", func() {
			obj := validBase("defaulted-tunnel")
			obj.Spec.Cloudflared = cfztv1alpha1.CloudflaredSpec{}

			err := k8sClient.Create(ctx, obj)
			Expect(err).NotTo(HaveOccurred())

			Expect(obj.Spec.Cloudflared.Namespace).To(Equal("cfzt-system"))
			Expect(obj.Spec.Dns.Manage).To(BeTrue())
			Expect(obj.Spec.CredentialsSecretRef.Keys.AccountId).To(Equal("accountId"))
			Expect(obj.Spec.CredentialsSecretRef.Keys.ApiToken).To(Equal("apiToken"))

			By("cleanup")
			Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
		})

		It("defaults nested objects omitted from YAML-shaped input", func() {
			obj := &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "cfzt.reid.ee/v1alpha1",
					"kind":       "CloudflareTunnel",
					"metadata": map[string]any{
						"name": "yaml-defaulted-tunnel",
					},
					"spec": map[string]any{
						"credentialsSecretRef": map[string]any{
							"name": "cloudflare-credentials",
						},
						"tunnelName": "homelab-rke2",
					},
				},
			}

			Expect(k8sClient.Create(ctx, obj)).To(Succeed())

			created := &cfztv1alpha1.CloudflareTunnel{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "yaml-defaulted-tunnel"}, created)).To(Succeed())
			Expect(created.Spec.Cloudflared.Namespace).To(Equal("cfzt-system"))
			Expect(created.Spec.Dns.Manage).To(BeTrue())
			Expect(created.Spec.CredentialsSecretRef.Keys.AccountId).To(Equal("accountId"))
			Expect(created.Spec.CredentialsSecretRef.Keys.ApiToken).To(Equal("apiToken"))

			By("cleanup")
			Expect(k8sClient.Delete(ctx, created)).To(Succeed())
		})
	})
})
