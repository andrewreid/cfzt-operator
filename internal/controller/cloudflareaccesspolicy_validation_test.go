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

var _ = Describe("CloudflareAccessPolicy CRD validation", func() {
	validPolicy := func(name string) *cfztv1alpha1.CloudflareAccessPolicy {
		return &cfztv1alpha1.CloudflareAccessPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: cfztv1alpha1.CloudflareAccessPolicySpec{
				CredentialsSecretRef: cfztv1alpha1.AccessPolicyCredentialsSecretRef{
					Name:      "cloudflare-credentials",
					Namespace: "cfzt-system",
				},
				Decision: "allow",
				Rules: cfztv1alpha1.AccessRules{
					Include: []cfztv1alpha1.AccessRule{
						{EmailDomain: "example.com"},
					},
				},
			},
		}
	}

	It("TestCloudflareAccessPolicyCRDValidation accepts a valid minimal policy", func() {
		obj := validPolicy("valid-policy")
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
	})

	It("TestCloudflareAccessPolicyCRDValidation rejects decision outside enum", func() {
		obj := validPolicy("bad-decision")
		obj.Spec.Decision = "maybe"
		err := k8sClient.Create(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.decision"))
	})

	It("TestCloudflareAccessPolicyCRDValidation rejects rule with two fields set", func() {
		obj := validPolicy("two-fields")
		obj.Spec.Rules.Include = []cfztv1alpha1.AccessRule{
			{Email: "alice@example.com", EmailDomain: "example.com"},
		}
		err := k8sClient.Create(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exactly one of"))
	})

	It("TestCloudflareAccessPolicyCRDValidation rejects rule with zero fields set", func() {
		obj := validPolicy("zero-fields")
		obj.Spec.Rules.Include = []cfztv1alpha1.AccessRule{{}}
		err := k8sClient.Create(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exactly one of"))
	})

	It("TestCloudflareAccessPolicyCRDValidation rejects everyone false", func() {
		obj := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "cfzt.reid.ee/v1alpha1",
				"kind":       "CloudflareAccessPolicy",
				"metadata": map[string]any{
					"name": "everyone-false",
				},
				"spec": map[string]any{
					"credentialsSecretRef": map[string]any{
						"name":      "cloudflare-credentials",
						"namespace": "cfzt-system",
					},
					"decision": "allow",
					"rules": map[string]any{
						"include": []any{
							map[string]any{"everyone": false},
						},
					},
				},
			},
		}

		err := k8sClient.Create(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exactly one of"))
	})

	It("TestCloudflareAccessPolicyCRDValidation rejects empty rules", func() {
		obj := validPolicy("empty-rules")
		obj.Spec.Rules = cfztv1alpha1.AccessRules{}
		err := k8sClient.Create(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("at least one item"))
	})

	It("TestCloudflareAccessPolicyCRDValidation rejects sessionDuration not matching pattern", func() {
		obj := validPolicy("bad-session")
		obj.Spec.SessionDuration = "24hr"
		err := k8sClient.Create(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.sessionDuration"))
	})

	It("TestCloudflareAccessPolicyCRDValidation rejects missing credentialsSecretRef.namespace", func() {
		obj := validPolicy("missing-ns")
		obj.Spec.CredentialsSecretRef.Namespace = ""
		err := k8sClient.Create(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("credentialsSecretRef.namespace"))
	})

	It("TestCloudflareAccessPolicyCRDValidation rejects missing decision", func() {
		obj := validPolicy("missing-decision")
		obj.Spec.Decision = ""
		err := k8sClient.Create(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.decision"))
	})

	It("TestCloudflareAccessPolicyCRDValidation rejects policyName update", func() {
		obj := validPolicy("immutable-policy-name")
		obj.Spec.PolicyName = "family-only-cfzt"
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())

		obj.Spec.PolicyName = "other-family-cfzt"
		err := k8sClient.Update(ctx, obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("policyName is immutable"))

		created := &cfztv1alpha1.CloudflareAccessPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "immutable-policy-name"}, created)).To(Succeed())
		Expect(k8sClient.Delete(ctx, created)).To(Succeed())
	})
})
