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
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
		ensureNamespace(ctx, exposureTestNamespace)
		fakeCF = cloudflare.NewFake()
		reconciler = &CloudflareAccessPolicyReconciler{
			Client: indexedClient,
			Scheme: indexedClient.Scheme(),
			CloudflareClientFactory: func(accountID, apiToken string) (cloudflare.Client, error) {
				Expect(accountID).To(Equal("account-1"))
				Expect(apiToken).To(Equal("token-1"))
				return fakeCF, nil
			},
			Recorder: newTestRecorder(),
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
		Expect(got.Status.ObservedRulesHash).To(HavePrefix("sha256:"))
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

	It("TestAccessPolicyRulesDrift", func() {
		policy := createAccessPolicy(ctx, "drift-policy", "")
		reconcileAccessPolicy(ctx, reconciler, policy.Name)
		current := fetchAccessPolicy(ctx, policy.Name)
		oldHash := current.Status.ObservedRulesHash
		current.Spec.Rules.Include = []cfztv1alpha1.AccessRule{{EmailDomain: "changed.example.com"}}
		Expect(k8sClient.Update(ctx, current)).To(Succeed())

		reconcileAccessPolicy(ctx, reconciler, current.Name)

		updated := fetchAccessPolicy(ctx, current.Name)
		Expect(updated.Status.ObservedRulesHash).NotTo(Equal(oldHash))
		cfPolicy, err := fakeCF.AccessPolicies().Get(ctx, updated.Status.PolicyId)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfPolicy.Include).To(Equal([]cloudflare.AccessRule{{EmailDomain: "changed.example.com"}}))
	})

	It("TestAccessPolicyUnsupportedDrift", func() {
		policy := createAccessPolicy(ctx, "unsupported-drift-policy", "")
		reconcileAccessPolicy(ctx, reconciler, policy.Name)
		current := fetchAccessPolicy(ctx, policy.Name)
		oldHash := current.Status.ObservedRulesHash
		fakeCF.MarkAccessPolicyUnsupported(current.Status.PolicyId)

		reconcileAccessPolicy(ctx, reconciler, policy.Name)

		updated := fetchAccessPolicy(ctx, policy.Name)
		ready := meta.FindStatusCondition(updated.Status.Conditions, ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(ReasonUnsupportedDrift))
		Expect(updated.Status.ObservedRulesHash).To(Equal(oldHash))
	})

	It("TestAccessPolicyReferencedByPopulated", func() {
		policy := createAccessPolicy(ctx, "ref-policy", "")
		first := createPolicyRefExposure(ctx, "ref-one", policy.Name)
		second := createPolicyRefExposure(ctx, "ref-two", policy.Name)

		reconcileAccessPolicy(ctx, reconciler, policy.Name)

		got := fetchAccessPolicy(ctx, policy.Name)
		Expect(got.Status.ReferencedByCount).To(Equal(int32(2)))
		Expect(got.Status.ReferencedBy).To(ConsistOf(
			cfztv1alpha1.ReferencedExposure{Namespace: first.Namespace, Name: first.Name, Uid: first.UID},
			cfztv1alpha1.ReferencedExposure{Namespace: second.Namespace, Name: second.Name, Uid: second.UID},
		))
	})

	It("TestAccessPolicyReferencedByDecrements", func() {
		policy := createAccessPolicy(ctx, "ref-decrement-policy", "")
		first := createPolicyRefExposure(ctx, "ref-dec-one", policy.Name)
		_ = createPolicyRefExposure(ctx, "ref-dec-two", policy.Name)
		reconcileAccessPolicy(ctx, reconciler, policy.Name)
		Expect(k8sClient.Delete(ctx, first)).To(Succeed())

		reconcileAccessPolicy(ctx, reconciler, policy.Name)

		got := fetchAccessPolicy(ctx, policy.Name)
		Expect(got.Status.ReferencedByCount).To(Equal(int32(1)))
		Expect(got.Status.ReferencedBy[0].Name).To(Equal("ref-dec-two"))
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

	It("TestAccessPolicyFinalizerBlockedByExposures", func() {
		policy := createAccessPolicy(ctx, "blocked-policy", "")
		_ = createPolicyRefExposure(ctx, "blocked-ref", policy.Name)
		reconcileAccessPolicy(ctx, reconciler, policy.Name)
		current := fetchAccessPolicy(ctx, policy.Name)

		Expect(k8sClient.Delete(ctx, current)).To(Succeed())
		reconcileAccessPolicy(ctx, reconciler, current.Name)

		blocked := fetchAccessPolicy(ctx, policy.Name)
		Expect(blocked.Finalizers).To(ContainElement(naming.Finalizer))
		Expect(meta.FindStatusCondition(blocked.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonBlockedByExposures))
	})

	It("TestAccessPolicyFinalizerUnblocks", func() {
		policy := createAccessPolicy(ctx, "unblock-policy", "")
		ref := createPolicyRefExposure(ctx, "unblock-ref", policy.Name)
		reconcileAccessPolicy(ctx, reconciler, policy.Name)
		current := fetchAccessPolicy(ctx, policy.Name)
		policyID := current.Status.PolicyId
		Expect(k8sClient.Delete(ctx, current)).To(Succeed())
		reconcileAccessPolicy(ctx, reconciler, current.Name)
		Expect(k8sClient.Delete(ctx, ref)).To(Succeed())

		reconcileAccessPolicy(ctx, reconciler, current.Name)

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: policy.Name}, &cfztv1alpha1.CloudflareAccessPolicy{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())
		_, err := fakeCF.AccessPolicies().Get(ctx, policyID)
		Expect(err).To(MatchError(cloudflare.ErrNotFound))
	})

	It("TestAccessPolicyFinalizerMissingCredentialsRequeues", func() {
		policy := createAccessPolicy(ctx, "missing-creds-delete-policy", "")
		reconcileAccessPolicy(ctx, reconciler, policy.Name)
		current := fetchAccessPolicy(ctx, policy.Name)
		policyID := current.Status.PolicyId
		Expect(policyID).NotTo(BeEmpty())
		Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-credentials", Namespace: credsNamespace}})).To(Succeed())

		Expect(k8sClient.Delete(ctx, current)).To(Succeed())
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: policy.Name}})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())
		blocked := fetchAccessPolicy(ctx, policy.Name)
		Expect(blocked.Finalizers).To(ContainElement(naming.Finalizer))
		Expect(meta.FindStatusCondition(blocked.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonCredentialsMissing))
		_, err = fakeCF.AccessPolicies().Get(ctx, policyID)
		Expect(err).NotTo(HaveOccurred())

		createCredentials(ctx)
		reconcileAccessPolicy(ctx, reconciler, policy.Name)
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: policy.Name}, &cfztv1alpha1.CloudflareAccessPolicy{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())
	})

	It("TestAccessPolicyFinalizerDeletesUnsupportedDrift", func() {
		policy := createAccessPolicy(ctx, "unsupported-delete-policy", "")
		reconcileAccessPolicy(ctx, reconciler, policy.Name)
		current := fetchAccessPolicy(ctx, policy.Name)
		policyID := current.Status.PolicyId
		Expect(policyID).NotTo(BeEmpty())
		fakeCF.MarkAccessPolicyUnsupported(policyID)

		Expect(k8sClient.Delete(ctx, current)).To(Succeed())
		reconcileAccessPolicy(ctx, reconciler, current.Name)

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: policy.Name}, &cfztv1alpha1.CloudflareAccessPolicy{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())
		_, err := fakeCF.AccessPolicies().Get(ctx, policyID)
		Expect(err).To(MatchError(cloudflare.ErrNotFound))
	})

	It("TestAccessPolicyConditionsTransition", func() {
		_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-credentials", Namespace: credsNamespace}})
		policy := createAccessPolicy(ctx, "condition-policy", "")
		_ = createPolicyRefExposure(ctx, "condition-ref", policy.Name)

		reconcileAccessPolicy(ctx, reconciler, policy.Name)

		missing := fetchAccessPolicy(ctx, policy.Name)
		Expect(meta.FindStatusCondition(missing.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonCredentialsMissing))
		Expect(missing.Status.ReferencedByCount).To(Equal(int32(1)))
		createCredentials(ctx)
		reconcileAccessPolicy(ctx, reconciler, policy.Name)
		ready := fetchAccessPolicy(ctx, policy.Name)
		Expect(meta.FindStatusCondition(ready.Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		Expect(meta.FindStatusCondition(ready.Status.Conditions, ConditionProgressing).Status).To(Equal(metav1.ConditionFalse))
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

func createPolicyRefExposure(ctx context.Context, name, policyName string) *cfztv1alpha1.CloudflareExposure {
	exposure := &cfztv1alpha1.CloudflareExposure{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: exposureTestNamespace},
		Spec: cfztv1alpha1.CloudflareExposureSpec{
			Hostname:  name + ".example.com",
			TunnelRef: cfztv1alpha1.TunnelRef{Name: "policy-ref-" + policyName},
			Origin: &cfztv1alpha1.OriginSpec{
				Protocol: "http",
				Host:     name + "." + exposureTestNamespace + ".svc.cluster.local",
				Port:     8096,
			},
			Access: cfztv1alpha1.AccessSpec{
				Enabled:   true,
				PolicyRef: cfztv1alpha1.AccessPolicyRef{Name: policyName},
			},
		},
	}
	Expect(k8sClient.Create(ctx, exposure)).To(Succeed())
	return exposure
}
