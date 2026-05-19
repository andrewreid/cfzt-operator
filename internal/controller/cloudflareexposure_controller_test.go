package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
	"github.com/andrewreid/cfzt-operator/internal/naming"
	"github.com/andrewreid/cfzt-operator/internal/origin"
)

var _ = Describe("CloudflareExposure Controller", func() {
	const namespace = "media"

	var (
		ctx                 context.Context
		fakeCF              *cloudflare.FakeClient
		tunnelReconciler    *CloudflareTunnelReconciler
		exposureReconciler  *CloudflareExposureReconciler
		cloudflareFactory   CloudflareClientFactory
		defaultPolicyUUID   = "00000000-0000-4000-8000-000000000001"
		defaultTunnelName   = "homelab"
		defaultTunnelCFName = "homelab-rke2"
	)

	BeforeEach(func() {
		ctx = context.Background()
		ensureNamespace(ctx, "cfzt-system")
		ensureNamespace(ctx, namespace)
		fakeCF = cloudflare.NewFake()
		fakeCF.AddZone("zone-example", "example.com")
		cloudflareFactory = func(accountID, apiToken string) (cloudflare.Client, error) {
			Expect(accountID).To(Equal("account-1"))
			Expect(apiToken).To(Equal("token-1"))
			return fakeCF, nil
		}
		tunnelReconciler = &CloudflareTunnelReconciler{
			Client:                  k8sClient,
			Scheme:                  k8sClient.Scheme(),
			CloudflareClientFactory: cloudflareFactory,
		}
		exposureReconciler = &CloudflareExposureReconciler{
			Client:                  k8sClient,
			Scheme:                  k8sClient.Scheme(),
			CloudflareClientFactory: cloudflareFactory,
		}
	})

	It("TestExposureCreate", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, defaultTunnelName, defaultTunnelCFName)
		exposure := createExposure(ctx, namespace, "jellyfin", tunnel.Name, "jellyfin.example.com", true, true)

		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, namespace, exposure.Name)
		Expect(current.Status.Cloudflare.AccessApplicationId).NotTo(BeEmpty())
		Expect(current.Status.Cloudflare.DnsRecordId).NotTo(BeEmpty())
		Expect(current.Status.Cloudflare.PublicHostnameRouteHash).NotTo(BeEmpty())
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))

		cfTunnel := fetchTunnel(ctx, tunnel.Name)
		config, err := fakeCF.Configurations().Get(ctx, cfTunnel.Status.TunnelId)
		Expect(err).NotTo(HaveOccurred())
		Expect(config.Ingress[0].Hostname).To(Equal("jellyfin.example.com"))
		Expect(config.Ingress[0].Service).To(Equal("http://jellyfin.media.svc.cluster.local:8096"))
	})

	It("TestExposureDNSManagedOff", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "dns-off", "dns-off")
		tunnel.Spec.Dns.Manage = false
		Expect(k8sClient.Update(ctx, tunnel)).To(Succeed())
		exposure := createExposure(ctx, namespace, "nodns", tunnel.Name, "nodns.example.com", true, true)

		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, namespace, exposure.Name)
		Expect(current.Status.Cloudflare.DnsRecordId).To(BeEmpty())
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		records, err := fakeCF.DNSRecords().List(ctx, "zone-example", "nodns.example.com", "CNAME")
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(BeEmpty())
	})

	It("TestExposureAccessDisabled", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "access-off", "access-off")
		exposure := createExposure(ctx, namespace, "noauth", tunnel.Name, "noauth.example.com", false, true)

		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, namespace, exposure.Name)
		Expect(current.Status.Cloudflare.AccessApplicationId).To(BeEmpty())
		Expect(current.Status.Cloudflare.DnsRecordId).NotTo(BeEmpty())
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
	})

	It("TestExposureAccessToggleOffDeletesOwnedApp", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "access-toggle", "access-toggle")
		exposure := createExposure(ctx, namespace, "toggleauth", tunnel.Name, "toggleauth.example.com", true, true)
		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, namespace, exposure.Name)
		Expect(current.Status.Cloudflare.AccessApplicationId).NotTo(BeEmpty())
		current.Spec.Access.Enabled = false
		Expect(k8sClient.Update(ctx, current)).To(Succeed())
		reconcileExposure(ctx, exposureReconciler, current)

		updated := fetchExposure(ctx, namespace, exposure.Name)
		Expect(updated.Status.Cloudflare.AccessApplicationId).To(BeEmpty())
		apps, err := fakeCF.AccessApplications().List(ctx, "toggleauth.example.com")
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(BeEmpty())
	})

	It("TestExposureDNSManageToggleOffDeletesOwnedRecord", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "dns-toggle", "dns-toggle")
		exposure := createExposure(ctx, namespace, "toggledns", tunnel.Name, "toggledns.example.com", false, true)
		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, namespace, exposure.Name)
		Expect(current.Status.Cloudflare.DnsRecordId).NotTo(BeEmpty())
		latestTunnel := fetchTunnel(ctx, tunnel.Name)
		latestTunnel.Spec.Dns.Manage = false
		Expect(k8sClient.Update(ctx, latestTunnel)).To(Succeed())
		reconcileExposure(ctx, exposureReconciler, current)

		updated := fetchExposure(ctx, namespace, exposure.Name)
		Expect(updated.Status.Cloudflare.DnsRecordId).To(BeEmpty())
		records, err := fakeCF.DNSRecords().List(ctx, "zone-example", "toggledns.example.com", "CNAME")
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(BeEmpty())
	})

	It("TestExposureHostnameConflict", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "conflict-tunnel", "conflict-tunnel")
		first := createExposure(ctx, namespace, "one", tunnel.Name, "same.example.com", true, true)
		second := createExposure(ctx, namespace, "two", tunnel.Name, "same.example.com", true, true)

		reconcileExposure(ctx, exposureReconciler, first)
		reconcileExposure(ctx, exposureReconciler, second)

		Expect(meta.FindStatusCondition(fetchExposure(ctx, namespace, first.Name).Status.Conditions, ConditionReady).Reason).To(Equal(ReasonHostnameConflict))
		Expect(meta.FindStatusCondition(fetchExposure(ctx, namespace, second.Name).Status.Conditions, ConditionReady).Reason).To(Equal(ReasonHostnameConflict))
	})

	It("TestExposureForeignResource", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "foreign-resource", "foreign-resource")
		_, err := fakeCF.AccessApplications().Create(ctx, cloudflare.AccessApplicationInput{
			Name:       "manual",
			Domain:     "foreign.example.com",
			PolicyUUID: defaultPolicyUUID,
			Tags:       []string{"managed-by=someone-else"},
		})
		Expect(err).NotTo(HaveOccurred())
		exposure := createExposure(ctx, namespace, "foreign", tunnel.Name, "foreign.example.com", true, true)

		reconcileExposure(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, namespace, exposure.Name)
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonHostnameConflict))
	})

	It("TestExposureFinalizer", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "delete-exposure", "delete-exposure")
		exposure := createExposure(ctx, namespace, "delete-me", tunnel.Name, "delete-me.example.com", true, true)
		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)
		current := fetchExposure(ctx, namespace, exposure.Name)
		Expect(current.Finalizers).To(ContainElement(naming.Finalizer))
		appID := current.Status.Cloudflare.AccessApplicationId
		recordID := current.Status.Cloudflare.DnsRecordId

		Expect(k8sClient.Delete(ctx, current)).To(Succeed())
		reconcileExposure(ctx, exposureReconciler, current)

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: exposure.Name}, &cfztv1alpha1.CloudflareExposure{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())
		apps, err := fakeCF.AccessApplications().List(ctx, "delete-me.example.com")
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(BeEmpty())
		records, err := fakeCF.DNSRecords().List(ctx, "zone-example", "delete-me.example.com", "CNAME")
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(BeEmpty())
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		config, err := fakeCF.Configurations().Get(ctx, fetchTunnel(ctx, tunnel.Name).Status.TunnelId)
		Expect(err).NotTo(HaveOccurred())
		Expect(config.Ingress).To(HaveLen(1))
		Expect(config.Ingress[0].Service).To(Equal("http_status:404"))
		Expect(appID).NotTo(BeEmpty())
		Expect(recordID).NotTo(BeEmpty())
	})

	It("TestTunnelBlockedByExposures", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "blocked-tunnel", "blocked-tunnel")
		_ = createExposure(ctx, namespace, "still-here", tunnel.Name, "still-here.example.com", false, true)

		current := fetchTunnel(ctx, tunnel.Name)
		Expect(k8sClient.Delete(ctx, current)).To(Succeed())
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)

		blocked := fetchTunnel(ctx, tunnel.Name)
		Expect(blocked.Finalizers).To(ContainElement(naming.Finalizer))
		Expect(meta.FindStatusCondition(blocked.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonBlockedByExposures))
	})

	It("TestExposureEnqueuesTunnel", func() {
		exposure := &cfztv1alpha1.CloudflareExposure{
			Spec: cfztv1alpha1.CloudflareExposureSpec{TunnelRef: cfztv1alpha1.TunnelRef{Name: "mapped-tunnel"}},
		}
		requests := exposureToTunnel(ctx, exposure)
		Expect(requests).To(HaveLen(1))
		Expect(requests[0].Name).To(Equal("mapped-tunnel"))
	})

	It("TestTunnelStatusUpdatePropagatesToExposures", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "watch-tunnel", "watch-tunnel")
		first := createExposure(ctx, namespace, "watch-one", tunnel.Name, "watch-one.example.com", false, true)
		second := createExposure(ctx, namespace, "watch-two", tunnel.Name, "watch-two.example.com", false, true)

		requests := exposureReconciler.tunnelToExposures(ctx, tunnel)

		Expect(requests).To(ConsistOf(
			reconcile.Request{NamespacedName: types.NamespacedName{Namespace: first.Namespace, Name: first.Name}},
			reconcile.Request{NamespacedName: types.NamespacedName{Namespace: second.Namespace, Name: second.Name}},
		))
	})

	It("TestExposureExternalOriginHostNetwork", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "external-origin", "external-origin")
		exposure := createExposure(ctx, namespace, "homeassistant", tunnel.Name, "ha.example.com", false, false)
		exposure.Spec.Origin = &cfztv1alpha1.OriginSpec{Protocol: "http", Host: "192.168.1.50", Port: 8123}
		Expect(k8sClient.Update(ctx, exposure)).To(Succeed())

		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)

		cfTunnel := fetchTunnel(ctx, tunnel.Name)
		config, err := fakeCF.Configurations().Get(ctx, cfTunnel.Status.TunnelId)
		Expect(err).NotTo(HaveOccurred())
		Expect(config.Ingress[0].Service).To(Equal("http://192.168.1.50:8123"))
		Expect(meta.FindStatusCondition(fetchExposure(ctx, namespace, exposure.Name).Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
	})

	It("TestSourceRefServiceSinglePort", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "source-service", "source-service")
		createService(ctx, namespace, "jellyfin", 8096)
		exposure := createExposure(ctx, namespace, "jellyfin-source", tunnel.Name, "jellyfin-source.example.com", false, true)
		exposure.Spec.SourceRef = &cfztv1alpha1.SourceRef{ApiVersion: "v1", Kind: origin.ServiceKind, Name: "jellyfin"}
		exposure.Spec.Origin = nil
		Expect(k8sClient.Update(ctx, exposure)).To(Succeed())

		reconcileExposure(ctx, exposureReconciler, exposure)
		current := fetchExposure(ctx, namespace, exposure.Name)
		Expect(current.Spec.Origin).To(Equal(&cfztv1alpha1.OriginSpec{Protocol: "http", Host: "jellyfin.media.svc.cluster.local", Port: 8096}))
		Expect(current.OwnerReferences).To(HaveLen(1))
		Expect(current.OwnerReferences[0].Kind).To(Equal(origin.ServiceKind))

		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, current)
		config, err := fakeCF.Configurations().Get(ctx, fetchTunnel(ctx, tunnel.Name).Status.TunnelId)
		Expect(err).NotTo(HaveOccurred())
		Expect(config.Ingress[0].Service).To(Equal("http://jellyfin.media.svc.cluster.local:8096"))
		Expect(meta.FindStatusCondition(fetchExposure(ctx, namespace, exposure.Name).Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
	})

	It("TestSourceRefServiceMultiPortRejected", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "source-multiport", "source-multiport")
		createService(ctx, namespace, "multiport", 8080, 9090)
		exposure := createExposure(ctx, namespace, "multiport-source", tunnel.Name, "multiport-source.example.com", false, true)
		exposure.Spec.SourceRef = &cfztv1alpha1.SourceRef{ApiVersion: "v1", Kind: origin.ServiceKind, Name: "multiport"}
		exposure.Spec.Origin = nil
		Expect(k8sClient.Update(ctx, exposure)).To(Succeed())

		reconcileExposure(ctx, exposureReconciler, exposure)

		ready := meta.FindStatusCondition(fetchExposure(ctx, namespace, exposure.Name).Status.Conditions, ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(ReasonOriginInvalid))
		Expect(ready.Message).To(ContainSubstring("has 2 ports"))
	})

	It("TestSourceRefDeletionCascades", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "source-delete", "source-delete")
		svc := createService(ctx, namespace, "delete-source", 8096)
		exposure := createExposure(ctx, namespace, "delete-source-exp", tunnel.Name, "delete-source.example.com", true, true)
		exposure.Spec.SourceRef = &cfztv1alpha1.SourceRef{ApiVersion: "v1", Kind: origin.ServiceKind, Name: svc.Name}
		exposure.Spec.Origin = nil
		Expect(k8sClient.Update(ctx, exposure)).To(Succeed())
		reconcileExposure(ctx, exposureReconciler, exposure)
		current := fetchExposure(ctx, namespace, exposure.Name)
		Expect(current.OwnerReferences).To(HaveLen(1))
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, current)
		current = fetchExposure(ctx, namespace, exposure.Name)
		Expect(current.Status.Cloudflare.AccessApplicationId).NotTo(BeEmpty())

		Expect(k8sClient.Delete(ctx, svc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, current)).To(Succeed())
		reconcileExposure(ctx, exposureReconciler, current)

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: exposure.Name}, &cfztv1alpha1.CloudflareExposure{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())
		apps, err := fakeCF.AccessApplications().List(ctx, "delete-source.example.com")
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(BeEmpty())
	})

	It("TestHTTPRouteHostnameDerivation", func() {
		exposure := &cfztv1alpha1.CloudflareExposure{
			ObjectMeta: metav1.ObjectMeta{Name: "route-source", Namespace: namespace, UID: types.UID("exposure-uid")},
			Spec: cfztv1alpha1.CloudflareExposureSpec{
				TunnelRef: cfztv1alpha1.TunnelRef{Name: defaultTunnelName},
				SourceRef: &cfztv1alpha1.SourceRef{
					ApiVersion: origin.HTTPRouteAPIVersion,
					Kind:       origin.HTTPRouteKind,
					Name:       "jellyfin-route",
				},
				Origin: &cfztv1alpha1.OriginSpec{Protocol: "http", Host: "jellyfin.media.svc.cluster.local", Port: 8096},
			},
		}
		route := httpRoute(namespace, "jellyfin-route", "route.example.com")
		localClient := fakeclient.NewClientBuilder().WithScheme(k8sClient.Scheme()).WithObjects(exposure, route).Build()
		localReconciler := &CloudflareExposureReconciler{Client: localClient, Scheme: k8sClient.Scheme(), HTTPRouteSourceEnabled: true}

		defaulted, err := localReconciler.defaultFromSourceRef(ctx, exposure)

		Expect(err).NotTo(HaveOccurred())
		Expect(defaulted).To(BeTrue())
		current := &cfztv1alpha1.CloudflareExposure{}
		Expect(localClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: exposure.Name}, current)).To(Succeed())
		Expect(current.Spec.Hostname).To(Equal("route.example.com"))
		Expect(current.OwnerReferences).To(HaveLen(1))
		Expect(current.OwnerReferences[0].Kind).To(Equal(origin.HTTPRouteKind))
	})

	It("TestHTTPRouteCRDAbsentBootsClean", func() {
		present, err := HTTPRouteCRDPresent(ctx, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		Expect(present).To(BeFalse())
	})
})

func readyTunnel(ctx context.Context, reconciler *CloudflareTunnelReconciler, name, tunnelName string) *cfztv1alpha1.CloudflareTunnel {
	tunnel := createTunnel(ctx, name, tunnelName)
	createCredentials(ctx, "cfzt-system", "cloudflare-credentials")
	reconcileTunnel(ctx, reconciler, tunnel.Name)
	markTunnelDaemonSetReady(ctx, tunnel.Name)
	reconcileTunnel(ctx, reconciler, tunnel.Name)
	return fetchTunnel(ctx, tunnel.Name)
}

func createExposure(ctx context.Context, namespace, name, tunnelName, hostname string, accessEnabled, dnsManaged bool) *cfztv1alpha1.CloudflareExposure {
	_ = dnsManaged
	exposure := &cfztv1alpha1.CloudflareExposure{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: cfztv1alpha1.CloudflareExposureSpec{
			Hostname:  hostname,
			TunnelRef: cfztv1alpha1.TunnelRef{Name: tunnelName},
			Origin: &cfztv1alpha1.OriginSpec{
				Protocol: "http",
				Host:     name + "." + namespace + ".svc.cluster.local",
				Port:     8096,
			},
			Access: cfztv1alpha1.AccessSpec{
				Enabled: accessEnabled,
				PolicyRef: cfztv1alpha1.AccessPolicyRef{
					UUID: "00000000-0000-4000-8000-000000000001",
				},
			},
		},
	}
	Expect(k8sClient.Create(ctx, exposure)).To(Succeed())
	return exposure
}

func createService(ctx context.Context, namespace, name string, ports ...int32) *corev1.Service {
	servicePorts := make([]corev1.ServicePort, 0, len(ports))
	for i, port := range ports {
		servicePorts = append(servicePorts, corev1.ServicePort{Name: "port-" + string(rune('a'+i)), Port: port})
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Ports: servicePorts,
		},
	}
	Expect(k8sClient.Create(ctx, svc)).To(Succeed())
	return svc
}

func httpRoute(namespace, name, hostname string) *unstructured.Unstructured {
	route := &unstructured.Unstructured{}
	route.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: origin.HTTPRouteKind})
	route.SetName(name)
	route.SetNamespace(namespace)
	route.SetUID(types.UID(name + "-uid"))
	route.Object["spec"] = map[string]any{"hostnames": []any{hostname}}
	return route
}

func fetchExposure(ctx context.Context, namespace, name string) *cfztv1alpha1.CloudflareExposure {
	exposure := &cfztv1alpha1.CloudflareExposure{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, exposure)).To(Succeed())
	return exposure
}

func reconcileExposure(ctx context.Context, reconciler *CloudflareExposureReconciler, exposure *cfztv1alpha1.CloudflareExposure) {
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Name}})
	Expect(err).NotTo(HaveOccurred())
}

func markTunnelDaemonSetReady(ctx context.Context, tunnelName string) {
	tunnel := fetchTunnel(ctx, tunnelName)
	ds := &appsv1.DaemonSet{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: tunnel.Spec.Cloudflared.Namespace, Name: naming.DaemonSetName(tunnel.Name)}, ds)).To(Succeed())
	markDaemonSetReady(ctx, ds)
}
