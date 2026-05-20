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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
	"github.com/andrewreid/cfzt-operator/internal/naming"
)

var _ = Describe("CloudflareAccessPolicy Controller", func() {
	const credsNamespace = "cfzt-system"

	var (
		ctx        context.Context
		fakeCF     *cloudflare.FakeClient
		reconciler *CloudflareAccessPolicyReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		ensureNamespace(ctx, credsNamespace)
		fakeCF = cloudflare.NewFake()
		reconciler = &CloudflareAccessPolicyReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			CloudflareClientFactory: func(accountID, apiToken string) (cloudflare.Client, error) {
				Expect(accountID).To(Equal("account-1"))
				Expect(apiToken).To(Equal("token-1"))
				return fakeCF, nil
			},
		}
		createCredentials(ctx)
	})

	It("TestAccessPolicyCreate", func() {
		policy := createAccessPolicy(ctx, "create-policy", "")

		reconcileAccessPolicy(ctx, reconciler, policy.Name)

		got := fetchAccessPolicy(ctx, policy.Name)
		Expect(got.Status.PolicyId).NotTo(BeEmpty())
		ready := meta.FindStatusCondition(got.Status.Conditions, ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(ready.Reason).To(Equal(ReasonReconciled))

		cfPolicy, err := fakeCF.AccessPolicies().Get(ctx, got.Status.PolicyId)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfPolicy.Name).To(Equal("create-policy-cfzt"))
		Expect(cfPolicy.Decision).To(Equal("allow"))
		Expect(cfPolicy.Include).To(HaveLen(1))
		Expect(cfPolicy.Include[0].EmailDomain).To(Equal("example.com"))
	})

	It("TestAccessPolicyCreateUsesSpecPolicyName", func() {
		policy := createAccessPolicy(ctx, "create-policy-name", "custom-policy-name")

		reconcileAccessPolicy(ctx, reconciler, policy.Name)

		got := fetchAccessPolicy(ctx, policy.Name)
		Expect(got.Status.PolicyId).NotTo(BeEmpty())
		cfPolicy, err := fakeCF.AccessPolicies().Get(ctx, got.Status.PolicyId)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfPolicy.Name).To(Equal("custom-policy-name"))
	})

	It("TestAccessPolicyForeignRefuses", func() {
		_, err := fakeCF.AccessPolicies().Create(ctx, cloudflare.AccessPolicyInput{
			Name:     "foreign-policy-cfzt",
			Decision: "allow",
			Include:  []cloudflare.AccessRule{{Everyone: true}},
		})
		Expect(err).NotTo(HaveOccurred())

		policy := createAccessPolicy(ctx, "foreign-policy", "")

		reconcileAccessPolicy(ctx, reconciler, policy.Name)

		got := fetchAccessPolicy(ctx, policy.Name)
		Expect(got.Status.PolicyId).To(BeEmpty())
		ready := meta.FindStatusCondition(got.Status.Conditions, ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(ReasonForeignPolicy))

		// Foreign policy untouched.
		all, err := fakeCF.AccessPolicies().List(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(all).To(HaveLen(1))
		Expect(all[0].Name).To(Equal("foreign-policy-cfzt"))
	})

	It("TestAccessPolicyStaleStatusIDRecreates", func() {
		policy := createAccessPolicy(ctx, "stale-policy", "")
		policy.Status.PolicyId = "missing-policy"
		Expect(k8sClient.Status().Update(ctx, policy)).To(Succeed())

		reconcileAccessPolicy(ctx, reconciler, policy.Name)
		reconcileAccessPolicy(ctx, reconciler, policy.Name)

		got := fetchAccessPolicy(ctx, policy.Name)
		Expect(got.Status.PolicyId).NotTo(BeEmpty())
		Expect(got.Status.PolicyId).NotTo(Equal("missing-policy"))
		cfPolicy, err := fakeCF.AccessPolicies().Get(ctx, got.Status.PolicyId)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfPolicy.Name).To(Equal("stale-policy-cfzt"))
	})

	It("TestAccessPolicyForeignStatusIDRefuses", func() {
		cfPolicy, err := fakeCF.AccessPolicies().Create(ctx, cloudflare.AccessPolicyInput{
			Name:     "actual-foreign-policy",
			Decision: "allow",
			Include:  []cloudflare.AccessRule{{Everyone: true}},
		})
		Expect(err).NotTo(HaveOccurred())
		policy := createAccessPolicy(ctx, "foreign-id-policy", "")
		policy.Status.PolicyId = cfPolicy.ID
		Expect(k8sClient.Status().Update(ctx, policy)).To(Succeed())

		reconcileAccessPolicy(ctx, reconciler, policy.Name)

		got := fetchAccessPolicy(ctx, policy.Name)
		Expect(got.Status.PolicyId).To(Equal(cfPolicy.ID))
		ready := meta.FindStatusCondition(got.Status.Conditions, ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(ReasonForeignPolicy))
	})

	It("TestAccessPolicyFinalizerBlocksForeignStatusID", func() {
		cfPolicy, err := fakeCF.AccessPolicies().Create(ctx, cloudflare.AccessPolicyInput{
			Name:     "actual-foreign-delete",
			Decision: "allow",
			Include:  []cloudflare.AccessRule{{Everyone: true}},
		})
		Expect(err).NotTo(HaveOccurred())
		policy := createAccessPolicy(ctx, "foreign-delete-policy", "")
		policy.Finalizers = []string{naming.Finalizer}
		Expect(k8sClient.Update(ctx, policy)).To(Succeed())
		policy.Status.PolicyId = cfPolicy.ID
		Expect(k8sClient.Status().Update(ctx, policy)).To(Succeed())

		Expect(k8sClient.Delete(ctx, policy)).To(Succeed())
		reconcileAccessPolicy(ctx, reconciler, policy.Name)

		got := fetchAccessPolicy(ctx, policy.Name)
		Expect(got.Finalizers).To(ContainElement(naming.Finalizer))
		Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonForeignPolicy))
		_, err = fakeCF.AccessPolicies().Get(ctx, cfPolicy.ID)
		Expect(err).NotTo(HaveOccurred())
	})
})

func createAccessPolicy(ctx context.Context, name, policyName string) *cfztv1alpha1.CloudflareAccessPolicy {
	policy := &cfztv1alpha1.CloudflareAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cfztv1alpha1.CloudflareAccessPolicySpec{
			CredentialsSecretRef: cfztv1alpha1.AccessPolicyCredentialsSecretRef{
				Name:      "cloudflare-credentials",
				Namespace: "cfzt-system",
			},
			PolicyName: policyName,
			Decision:   "allow",
			Rules: cfztv1alpha1.AccessRules{
				Include: []cfztv1alpha1.AccessRule{{EmailDomain: "example.com"}},
			},
		},
	}
	Expect(k8sClient.Create(ctx, policy)).To(Succeed())
	return policy
}

func fetchAccessPolicy(ctx context.Context, name string) *cfztv1alpha1.CloudflareAccessPolicy {
	policy := &cfztv1alpha1.CloudflareAccessPolicy{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, policy)).To(Succeed())
	return policy
}

func reconcileAccessPolicy(ctx context.Context, reconciler *CloudflareAccessPolicyReconciler, name string) {
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
	Expect(err).NotTo(HaveOccurred())
}
