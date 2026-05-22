package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
)

var _ = Describe("Controller Base", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
	})

	It("TestBaseSetReadyIdempotent", func() {
		tunnel := createTunnel(ctx, "base-ready-idempotent", "base-ready-idempotent")
		base := &Base{Client: k8sClient}

		latest := fetchTunnel(ctx, tunnel.Name)
		Expect(base.SetReady(ctx, latest, &latest.Status.Conditions, latest.Generation, true, ReasonReconciled, "ready")).To(Succeed())
		ready := fetchTunnel(ctx, tunnel.Name)
		resourceVersion := ready.ResourceVersion

		Expect(base.SetReady(ctx, ready, &ready.Status.Conditions, ready.Generation, true, ReasonReconciled, "ready")).To(Succeed())

		again := fetchTunnel(ctx, tunnel.Name)
		Expect(again.ResourceVersion).To(Equal(resourceVersion))
		Expect(meta.FindStatusCondition(again.Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
	})

	It("TestCredentialsLoaderTunnelNamespace", func() {
		ensureCredentialsSecret(ctx, "custom-tunnel-creds", "cloudflare-credentials")
		tunnel := &cfztv1alpha1.CloudflareTunnel{
			Spec: cfztv1alpha1.CloudflareTunnelSpec{
				CredentialsSecretRef: cfztv1alpha1.CredentialsSecretRef{Name: "cloudflare-credentials"},
				Cloudflared:          cfztv1alpha1.CloudflaredSpec{Namespace: "custom-tunnel-creds"},
			},
		}

		accountID, apiToken, err := cloudflare.Load(ctx, k8sClient, credentialsRefFromTunnel(tunnel))

		Expect(err).NotTo(HaveOccurred())
		Expect(accountID).To(Equal("account-1"))
		Expect(apiToken).To(Equal("token-1"))
	})

	It("TestCredentialsLoaderExplicitNamespace", func() {
		ensureCredentialsSecret(ctx, "custom-policy-creds", "policy-credentials")
		policy := &cfztv1alpha1.CloudflareAccessPolicy{
			Spec: cfztv1alpha1.CloudflareAccessPolicySpec{
				CredentialsSecretRef: cfztv1alpha1.AccessPolicyCredentialsSecretRef{
					Name:      "policy-credentials",
					Namespace: "custom-policy-creds",
				},
			},
		}

		accountID, apiToken, err := cloudflare.Load(ctx, k8sClient, credentialsRefFromAccessPolicy(policy))

		Expect(err).NotTo(HaveOccurred())
		Expect(accountID).To(Equal("account-1"))
		Expect(apiToken).To(Equal("token-1"))
	})
})

func ensureCredentialsSecret(ctx context.Context, namespace, name string) {
	ensureNamespace(ctx, namespace)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data: map[string][]byte{
			"accountId": []byte("account-1"),
			"apiToken":  []byte("token-1"),
		},
	}
	err := k8sClient.Create(ctx, secret)
	if errors.IsAlreadyExists(err) {
		current := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, current)).To(Succeed())
		current.Data = secret.Data
		Expect(k8sClient.Update(ctx, current)).To(Succeed())
		return
	}
	Expect(err).NotTo(HaveOccurred())
}
