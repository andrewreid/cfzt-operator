package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
	"github.com/andrewreid/cfzt-operator/internal/naming"
	"github.com/andrewreid/cfzt-operator/internal/workload"
)

var _ = Describe("CloudflareTunnel Controller", func() {
	const namespace = "cfzt-system"

	var (
		ctx        context.Context
		fakeCF     *cloudflare.FakeClient
		reconciler *CloudflareTunnelReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		ensureNamespace(ctx, namespace)
		fakeCF = cloudflare.NewFake()
		reconciler = &CloudflareTunnelReconciler{
			Client: indexedClient,
			Scheme: indexedClient.Scheme(),
			CloudflareClientFactory: func(accountID, apiToken string) (cloudflare.Client, error) {
				Expect(accountID).To(Equal("account-1"))
				Expect(apiToken).To(Equal("token-1"))
				return fakeCF, nil
			},
			Recorder: newTestRecorder(),
		}
	})

	It("TestTunnelCreate", func() {
		tunnel := createTunnel(ctx, "create-tunnel", "homelab-rke2")
		createCredentials(ctx)

		reconcileTunnel(ctx, reconciler, tunnel.Name)

		created := fetchTunnel(ctx, tunnel.Name)
		Expect(created.Status.TunnelId).NotTo(BeEmpty())
		Expect(created.Status.TokenSecretRef.Name).To(Equal(naming.TokenSecretName(tunnel.Name)))
		Expect(meta.FindStatusCondition(created.Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionFalse))
		Expect(meta.FindStatusCondition(created.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonWorkloadNotReady))

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: naming.TokenSecretName(tunnel.Name)}, secret)).To(Succeed())
		Expect(secret.Data).To(HaveKey(naming.TokenSecretKey))

		ds := &appsv1.DaemonSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: naming.DaemonSetName(tunnel.Name)}, ds)).To(Succeed())
		Expect(ds.Spec.Template.Spec.Containers[0].Image).To(Equal(workload.DefaultCloudflaredImage))
		Expect(ds.Spec.Template.Annotations).To(HaveKey(workload.TokenChecksumAnnotation))

		markDaemonSetReady(ctx, ds)
		reconcileTunnel(ctx, reconciler, tunnel.Name)

		ready := fetchTunnel(ctx, tunnel.Name)
		Expect(meta.FindStatusCondition(ready.Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		Expect(meta.FindStatusCondition(ready.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonReconciled))
	})

	It("TestTunnelAdopt", func() {
		cfTunnel, err := fakeCF.Tunnels().Create(ctx, cloudflare.CreateTunnelInput{Name: "adopt-me", ConfigSrc: "cloudflare"})
		Expect(err).NotTo(HaveOccurred())
		tunnel := createTunnel(ctx, "adopt-tunnel", "adopt-me")
		createCredentials(ctx)
		tunnel.Status.TunnelId = cfTunnel.ID
		Expect(k8sClient.Status().Update(ctx, tunnel)).To(Succeed())

		reconcileTunnel(ctx, reconciler, tunnel.Name)

		adopted := fetchTunnel(ctx, tunnel.Name)
		Expect(adopted.Status.TunnelId).To(Equal(cfTunnel.ID))
	})

	It("TestTunnelForeignTunnelRefuses", func() {
		_, err := fakeCF.Tunnels().Create(ctx, cloudflare.CreateTunnelInput{Name: "occupied-name", ConfigSrc: "cloudflare"})
		Expect(err).NotTo(HaveOccurred())
		tunnel := createTunnel(ctx, "foreign-tunnel", "occupied-name")
		createCredentials(ctx)

		reconcileTunnel(ctx, reconciler, tunnel.Name)

		foreign := fetchTunnel(ctx, tunnel.Name)
		Expect(foreign.Status.TunnelId).To(BeEmpty())
		ready := meta.FindStatusCondition(foreign.Status.Conditions, ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(ReasonForeignTunnel))
	})

	It("TestTunnelStaleStatusIDRecreates", func() {
		tunnel := createTunnel(ctx, "stale-id-tunnel", "stale-id")
		createCredentials(ctx)
		tunnel.Status.TunnelId = "missing-cloudflare-tunnel"
		Expect(k8sClient.Status().Update(ctx, tunnel)).To(Succeed())

		reconcileTunnel(ctx, reconciler, tunnel.Name)

		current := fetchTunnel(ctx, tunnel.Name)
		Expect(current.Status.TunnelId).NotTo(BeEmpty())
		Expect(current.Status.TunnelId).NotTo(Equal("missing-cloudflare-tunnel"))
		cfTunnel, err := fakeCF.Tunnels().Get(ctx, current.Status.TunnelId)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfTunnel.Name).To(Equal("stale-id"))
	})

	It("TestTunnelFinalizerBlocksForeignStatusID", func() {
		cfTunnel, err := fakeCF.Tunnels().Create(ctx, cloudflare.CreateTunnelInput{Name: "actual-foreign", ConfigSrc: "cloudflare"})
		Expect(err).NotTo(HaveOccurred())
		tunnel := createTunnel(ctx, "foreign-delete", "wanted-name")
		createCredentials(ctx)
		tunnel.Finalizers = []string{naming.Finalizer}
		Expect(k8sClient.Update(ctx, tunnel)).To(Succeed())
		tunnel.Status.TunnelId = cfTunnel.ID
		Expect(k8sClient.Status().Update(ctx, tunnel)).To(Succeed())

		Expect(k8sClient.Delete(ctx, tunnel)).To(Succeed())
		reconcileTunnel(ctx, reconciler, tunnel.Name)

		blocked := fetchTunnel(ctx, tunnel.Name)
		Expect(blocked.Finalizers).To(ContainElement(naming.Finalizer))
		Expect(meta.FindStatusCondition(blocked.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonForeignTunnel))
		_, err = fakeCF.Tunnels().Get(ctx, cfTunnel.ID)
		Expect(err).NotTo(HaveOccurred())
	})

	It("TestTunnelTokenRotation", func() {
		tunnel := createTunnel(ctx, "rotate-tunnel", "rotate-me")
		createCredentials(ctx)
		reconcileTunnel(ctx, reconciler, tunnel.Name)

		current := fetchTunnel(ctx, tunnel.Name)
		ds := &appsv1.DaemonSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: naming.DaemonSetName(tunnel.Name)}, ds)).To(Succeed())
		oldChecksum := ds.Spec.Template.Annotations[workload.TokenChecksumAnnotation]
		fakeCF.SetTunnelToken(current.Status.TunnelId, "rotated-token")

		reconcileTunnel(ctx, reconciler, tunnel.Name)

		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: naming.DaemonSetName(tunnel.Name)}, ds)).To(Succeed())
		Expect(ds.Spec.Template.Annotations[workload.TokenChecksumAnnotation]).NotTo(Equal(oldChecksum))
		Expect(ds.Spec.Template.Annotations[workload.TokenChecksumAnnotation]).To(Equal(workload.TokenChecksum("rotated-token")))
	})

	It("TestTunnelFinalizerNoop", func() {
		tunnel := createTunnel(ctx, "delete-tunnel", "delete-me")
		createCredentials(ctx)
		reconcileTunnel(ctx, reconciler, tunnel.Name)
		current := fetchTunnel(ctx, tunnel.Name)
		Expect(current.Finalizers).To(ContainElement(naming.Finalizer))
		Expect(current.Status.TunnelId).NotTo(BeEmpty())
		tunnelID := current.Status.TunnelId

		Expect(k8sClient.Delete(ctx, current)).To(Succeed())
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: tunnel.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())
		draining := fetchTunnel(ctx, tunnel.Name)
		Expect(draining.Finalizers).To(ContainElement(naming.Finalizer))
		Expect(meta.FindStatusCondition(draining.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonWorkloadNotReady))

		reconcileTunnel(ctx, reconciler, tunnel.Name)

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: tunnel.Name}, &cfztv1alpha1.CloudflareTunnel{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: naming.TokenSecretName(tunnel.Name)}, &corev1.Secret{})).To(MatchError(ContainSubstring("not found")))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: naming.DaemonSetName(tunnel.Name)}, &appsv1.DaemonSet{})).To(MatchError(ContainSubstring("not found")))
		_, err = fakeCF.Tunnels().Get(ctx, tunnelID)
		Expect(err).To(MatchError(cloudflare.ErrNotFound))
	})

	It("TestTunnelFinalizerMissingCredentialsRequeues", func() {
		tunnel := createTunnel(ctx, "delete-tunnel-missing-creds", "delete-missing-creds")
		createCredentials(ctx)
		reconcileTunnel(ctx, reconciler, tunnel.Name)
		current := fetchTunnel(ctx, tunnel.Name)
		tunnelID := current.Status.TunnelId
		Expect(tunnelID).NotTo(BeEmpty())
		Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-credentials", Namespace: namespace}})).To(Succeed())

		Expect(k8sClient.Delete(ctx, current)).To(Succeed())
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: tunnel.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())
		blocked := fetchTunnel(ctx, tunnel.Name)
		Expect(blocked.Finalizers).To(ContainElement(naming.Finalizer))
		Expect(meta.FindStatusCondition(blocked.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonWorkloadNotReady))

		result, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: tunnel.Name}})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())
		blocked = fetchTunnel(ctx, tunnel.Name)
		Expect(blocked.Finalizers).To(ContainElement(naming.Finalizer))
		Expect(meta.FindStatusCondition(blocked.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonCredentialsMissing))
		_, err = fakeCF.Tunnels().Get(ctx, tunnelID)
		Expect(err).NotTo(HaveOccurred())

		createCredentials(ctx)
		reconcileTunnel(ctx, reconciler, tunnel.Name)
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: tunnel.Name}, &cfztv1alpha1.CloudflareTunnel{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())
	})

	It("TestTunnelBlockedByRoutes", func() {
		tunnel := createTunnel(ctx, "route-blocked-tunnel", "route-blocked")
		createCredentials(ctx)
		reconcileTunnel(ctx, reconciler, tunnel.Name)
		_ = createTunnelRoute(ctx, "route-blocking-delete", tunnel.Name, "172.16.200.0/24")
		current := fetchTunnel(ctx, tunnel.Name)

		Expect(k8sClient.Delete(ctx, current)).To(Succeed())
		reconcileTunnel(ctx, reconciler, tunnel.Name)

		blocked := fetchTunnel(ctx, tunnel.Name)
		Expect(blocked.Finalizers).To(ContainElement(naming.Finalizer))
		Expect(meta.FindStatusCondition(blocked.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonBlockedByRoutes))
		_, err := fakeCF.Tunnels().Get(ctx, blocked.Status.TunnelId)
		Expect(err).NotTo(HaveOccurred())
	})

	It("TestTunnelConditionsTransition", func() {
		tunnel := createTunnel(ctx, "condition-tunnel", "condition-me")
		_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-credentials", Namespace: namespace}})

		reconcileTunnel(ctx, reconciler, tunnel.Name)

		missing := fetchTunnel(ctx, tunnel.Name)
		ready := meta.FindStatusCondition(missing.Status.Conditions, ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(ReasonCredentialsMissing))

		createCredentials(ctx)
		reconcileTunnel(ctx, reconciler, tunnel.Name)
		ds := &appsv1.DaemonSet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: naming.DaemonSetName(tunnel.Name)}, ds)).To(Succeed())
		markDaemonSetReady(ctx, ds)
		reconcileTunnel(ctx, reconciler, tunnel.Name)

		reconciled := fetchTunnel(ctx, tunnel.Name)
		Expect(meta.FindStatusCondition(reconciled.Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		Expect(meta.FindStatusCondition(reconciled.Status.Conditions, ConditionProgressing).Status).To(Equal(metav1.ConditionFalse))
	})
})

func ensureNamespace(ctx context.Context, name string) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	err := k8sClient.Create(ctx, ns)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
	}
}

func createCredentials(ctx context.Context) {
	const namespace = "cfzt-system"
	const name = "cloudflare-credentials"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data: map[string][]byte{
			"accountId": []byte("account-1"),
			"apiToken":  []byte("token-1"),
		},
	}
	err := k8sClient.Create(ctx, secret)
	if apierrors.IsAlreadyExists(err) {
		current := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, current)).To(Succeed())
		current.Data = secret.Data
		Expect(k8sClient.Update(ctx, current)).To(Succeed())
		return
	}
	Expect(err).NotTo(HaveOccurred())
}

func createTunnel(ctx context.Context, name, tunnelName string) *cfztv1alpha1.CloudflareTunnel {
	tunnel := &cfztv1alpha1.CloudflareTunnel{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cfztv1alpha1.CloudflareTunnelSpec{
			CredentialsSecretRef: cfztv1alpha1.CredentialsSecretRef{Name: "cloudflare-credentials"},
			TunnelName:           tunnelName,
			Dns:                  cfztv1alpha1.DnsSpec{Manage: true},
			Cloudflared: cfztv1alpha1.CloudflaredSpec{
				Namespace: "cfzt-system",
			},
		},
	}
	Expect(k8sClient.Create(ctx, tunnel)).To(Succeed())
	return tunnel
}

func fetchTunnel(ctx context.Context, name string) *cfztv1alpha1.CloudflareTunnel {
	tunnel := &cfztv1alpha1.CloudflareTunnel{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, tunnel)).To(Succeed())
	return tunnel
}

func reconcileTunnel(ctx context.Context, reconciler *CloudflareTunnelReconciler, name string) {
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
	Expect(err).NotTo(HaveOccurred())
}

func markDaemonSetReady(ctx context.Context, ds *appsv1.DaemonSet) {
	latest := &appsv1.DaemonSet{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name}, latest)).To(Succeed())
	latest.Status.NumberReady = 1
	latest.Status.DesiredNumberScheduled = 1
	err := k8sClient.Status().Update(ctx, latest)
	if apierrors.IsNotFound(err) {
		Fail(fmt.Sprintf("DaemonSet %s/%s not found", ds.Namespace, ds.Name))
	}
	Expect(err).NotTo(HaveOccurred())
}
