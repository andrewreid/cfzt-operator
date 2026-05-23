package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
	"github.com/andrewreid/cfzt-operator/internal/naming"
)

var _ = Describe("CloudflareTunnelRoute Controller", func() {
	var (
		ctx        context.Context
		fakeCF     *cloudflare.FakeClient
		reconciler *CloudflareTunnelRouteReconciler
		recorder   *record.FakeRecorder
	)

	BeforeEach(func() {
		ctx = context.Background()
		ensureNamespace(ctx, "cfzt-system")
		createCredentials(ctx)
		fakeCF = cloudflare.NewFake()
		recorder = record.NewFakeRecorder(1024)
		reconciler = &CloudflareTunnelRouteReconciler{
			Base: Base{
				Client:              indexedClient,
				Scheme:              indexedClient.Scheme(),
				NewCloudflareClient: newFakeCloudflareClient(indexedClient, fakeCF),
				Recorder:            recorder,
			},
		}
	})

	It("TestRouteCreate", func() {
		tunnel := createReadyTunnel(ctx, "route-create-tunnel", "cf-tunnel-1")
		route := createTunnelRoute(ctx, "route-create", tunnel.Name, "172.16.0.0/24")

		reconcileRoute(ctx, reconciler, route.Name)

		ready := fetchTunnelRoute(ctx, route.Name)
		Expect(ready.Status.RouteId).NotTo(BeEmpty())
		Expect(meta.FindStatusCondition(ready.Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		cfRoute, err := fakeCF.TunnelRoutes().Get(ctx, ready.Status.RouteId)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfRoute.Network).To(Equal("172.16.0.0/24"))
		Expect(cfRoute.TunnelID).To(Equal(tunnel.Status.TunnelId))
		Expect(cfRoute.Comment).To(ContainSubstring("managed-by=cfzt source-uid="))
	})

	It("TestRouteCreateIPv6", func() {
		tunnel := createReadyTunnel(ctx, "route-ipv6-tunnel", "cf-tunnel-ipv6")
		route := createTunnelRoute(ctx, "route-ipv6", tunnel.Name, "fd00::/64")

		reconcileRoute(ctx, reconciler, route.Name)

		ready := fetchTunnelRoute(ctx, route.Name)
		cfRoute, err := fakeCF.TunnelRoutes().Get(ctx, ready.Status.RouteId)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfRoute.Network).To(Equal("fd00::/64"))
	})

	It("TestRouteInvalidNetwork", func() {
		tunnel := createReadyTunnel(ctx, "route-invalid-tunnel", "cf-tunnel-invalid")
		route := createTunnelRoute(ctx, "route-invalid", tunnel.Name, "999.999.999.999/24")

		reconcileRoute(ctx, reconciler, route.Name)

		invalid := fetchTunnelRoute(ctx, route.Name)
		ready := meta.FindStatusCondition(invalid.Status.Conditions, ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(ReasonNetworkInvalid))
	})

	It("TestRouteForeignRefuses", func() {
		tunnel := createReadyTunnel(ctx, "route-foreign-tunnel", "cf-tunnel-foreign")
		_, err := fakeCF.TunnelRoutes().Create(ctx, cloudflare.TunnelRouteInput{
			Network:  "172.16.2.0/24",
			TunnelID: tunnel.Status.TunnelId,
			Comment:  "manual foreign route",
		})
		Expect(err).NotTo(HaveOccurred())
		route := createTunnelRoute(ctx, "route-foreign", tunnel.Name, "172.16.2.0/24")

		reconcileRoute(ctx, reconciler, route.Name)

		foreign := fetchTunnelRoute(ctx, route.Name)
		Expect(foreign.Status.RouteId).To(BeEmpty())
		Expect(meta.FindStatusCondition(foreign.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonForeignRoute))
	})

	It("TestRouteTunnelNotReady", func() {
		route := createTunnelRoute(ctx, "route-tunnel-not-ready", "missing-tunnel", "172.16.3.0/24")

		reconcileRoute(ctx, reconciler, route.Name)

		blocked := fetchTunnelRoute(ctx, route.Name)
		Expect(meta.FindStatusCondition(blocked.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonTunnelNotReady))
	})

	It("TestRouteDriftCorrection", func() {
		tunnel := createReadyTunnel(ctx, "route-drift-tunnel", "cf-tunnel-drift")
		route := createTunnelRoute(ctx, "route-drift", tunnel.Name, "172.16.4.0/24")
		reconcileRoute(ctx, reconciler, route.Name)
		ready := fetchTunnelRoute(ctx, route.Name)
		routeID := ready.Status.RouteId
		drainRecordedEvents(recorder)

		ready.Spec.Network = "172.16.5.0/24"
		Expect(k8sClient.Update(ctx, ready)).To(Succeed())
		reconcileRoute(ctx, reconciler, route.Name)
		expectRecordedEvent(recorder, EventUpdatedRoute)

		updated := fetchTunnelRoute(ctx, route.Name)
		Expect(updated.Status.RouteId).To(Equal(routeID))
		cfRoute, err := fakeCF.TunnelRoutes().Get(ctx, routeID)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfRoute.Network).To(Equal("172.16.5.0/24"))
	})

	It("TestRouteVNetDriftCorrection", func() {
		tunnel := createReadyTunnel(ctx, "route-vnet-tunnel", "cf-tunnel-vnet")
		route := createTunnelRoute(ctx, "route-vnet", tunnel.Name, "172.16.6.0/24")
		route.Spec.VirtualNetworkId = "00000000-0000-4000-8000-000000000001"
		Expect(k8sClient.Update(ctx, route)).To(Succeed())
		reconcileRoute(ctx, reconciler, route.Name)
		ready := fetchTunnelRoute(ctx, route.Name)

		ready.Spec.VirtualNetworkId = "00000000-0000-4000-8000-000000000002"
		Expect(k8sClient.Update(ctx, ready)).To(Succeed())
		reconcileRoute(ctx, reconciler, route.Name)

		cfRoute, err := fakeCF.TunnelRoutes().Get(ctx, ready.Status.RouteId)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfRoute.VirtualNetworkID).To(Equal("00000000-0000-4000-8000-000000000002"))
	})

	It("TestRouteEditPreflightForeignRefuses", func() {
		tunnel := createReadyTunnel(ctx, "route-preflight-tunnel", "cf-tunnel-preflight")
		route := createTunnelRoute(ctx, "route-preflight", tunnel.Name, "172.16.7.0/24")
		reconcileRoute(ctx, reconciler, route.Name)
		ready := fetchTunnelRoute(ctx, route.Name)
		_, err := fakeCF.TunnelRoutes().Create(ctx, cloudflare.TunnelRouteInput{Network: "172.16.8.0/24", TunnelID: tunnel.Status.TunnelId, Comment: "manual foreign route"})
		Expect(err).NotTo(HaveOccurred())

		ready.Spec.Network = "172.16.8.0/24"
		Expect(k8sClient.Update(ctx, ready)).To(Succeed())
		reconcileRoute(ctx, reconciler, route.Name)

		blocked := fetchTunnelRoute(ctx, route.Name)
		Expect(blocked.Status.RouteId).To(Equal(ready.Status.RouteId))
		Expect(meta.FindStatusCondition(blocked.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonForeignRoute))
	})

	It("TestRouteCommentDrift", func() {
		tunnel := createReadyTunnel(ctx, "route-comment-tunnel", "cf-tunnel-comment")
		route := createTunnelRoute(ctx, "route-comment", tunnel.Name, "172.16.9.0/24")
		route.Spec.Comment = "old"
		Expect(k8sClient.Update(ctx, route)).To(Succeed())
		reconcileRoute(ctx, reconciler, route.Name)
		ready := fetchTunnelRoute(ctx, route.Name)

		ready.Spec.Comment = "new"
		Expect(k8sClient.Update(ctx, ready)).To(Succeed())
		reconcileRoute(ctx, reconciler, route.Name)

		cfRoute, err := fakeCF.TunnelRoutes().Get(ctx, ready.Status.RouteId)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfRoute.Comment).To(HaveSuffix(" | new"))
	})

	It("TestRouteFinalizerDeletes", func() {
		tunnel := createReadyTunnel(ctx, "route-delete-tunnel", "cf-tunnel-delete")
		route := createTunnelRoute(ctx, "route-delete", tunnel.Name, "172.16.10.0/24")
		reconcileRoute(ctx, reconciler, route.Name)
		ready := fetchTunnelRoute(ctx, route.Name)
		routeID := ready.Status.RouteId

		Expect(k8sClient.Delete(ctx, ready)).To(Succeed())
		reconcileRoute(ctx, reconciler, route.Name)

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: route.Name}, &cfztv1alpha1.CloudflareTunnelRoute{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())
		_, err := fakeCF.TunnelRoutes().Get(ctx, routeID)
		Expect(err).To(MatchError(cloudflare.ErrNotFound))
	})

	It("TestRouteFinalizerLeavesForeign", func() {
		tunnel := createReadyTunnel(ctx, "route-delete-foreign-tunnel", "cf-tunnel-delete-foreign")
		cfRoute, err := fakeCF.TunnelRoutes().Create(ctx, cloudflare.TunnelRouteInput{Network: "172.16.11.0/24", TunnelID: tunnel.Status.TunnelId, Comment: "foreign"})
		Expect(err).NotTo(HaveOccurred())
		route := createTunnelRoute(ctx, "route-delete-foreign", tunnel.Name, "172.16.11.0/24")
		route.Finalizers = []string{naming.Finalizer}
		Expect(k8sClient.Update(ctx, route)).To(Succeed())
		route.Status.RouteId = cfRoute.ID
		Expect(k8sClient.Status().Update(ctx, route)).To(Succeed())

		Expect(k8sClient.Delete(ctx, route)).To(Succeed())
		reconcileRoute(ctx, reconciler, route.Name)

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: route.Name}, &cfztv1alpha1.CloudflareTunnelRoute{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())
		_, err = fakeCF.TunnelRoutes().Get(ctx, cfRoute.ID)
		Expect(err).NotTo(HaveOccurred())
	})

	It("TestRouteConditionsTransition", func() {
		tunnel := createTunnel(ctx, "route-condition-tunnel", "cf-tunnel-condition")
		route := createTunnelRoute(ctx, "route-condition", tunnel.Name, "172.16.12.0/24")

		reconcileRoute(ctx, reconciler, route.Name)
		blocked := fetchTunnelRoute(ctx, route.Name)
		Expect(meta.FindStatusCondition(blocked.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonTunnelNotReady))

		tunnel.Status.TunnelId = "cf-tunnel-condition-id"
		Expect(k8sClient.Status().Update(ctx, tunnel)).To(Succeed())
		reconcileRoute(ctx, reconciler, route.Name)

		ready := fetchTunnelRoute(ctx, route.Name)
		Expect(meta.FindStatusCondition(ready.Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		Expect(meta.FindStatusCondition(ready.Status.Conditions, ConditionProgressing).Status).To(Equal(metav1.ConditionFalse))
	})

	It("TestRouteRetriesOnTunnelID", func() {
		tunnel := createTunnel(ctx, "route-watch-tunnel", "cf-tunnel-watch")
		route := createTunnelRoute(ctx, "route-watch", tunnel.Name, "172.16.13.0/24")

		Eventually(func() []reconcile.Request {
			return enqueueNamed(func(tunnel *cfztv1alpha1.CloudflareTunnel) []types.NamespacedName {
				routes, err := reconciler.routesForTunnel(context.Background(), tunnel.Name)
				if err != nil {
					return nil
				}
				requests := make([]types.NamespacedName, 0, len(routes))
				for _, route := range routes {
					requests = append(requests, types.NamespacedName{Name: route.Name})
				}
				return requests
			})(ctx, tunnel)
		}).Should(ContainElement(reconcile.Request{NamespacedName: types.NamespacedName{Name: route.Name}}))
	})
})

func createReadyTunnel(ctx context.Context, name, tunnelID string) *cfztv1alpha1.CloudflareTunnel {
	tunnel := createTunnel(ctx, name, name+"-cf")
	tunnel.Status.TunnelId = tunnelID
	Expect(k8sClient.Status().Update(ctx, tunnel)).To(Succeed())
	return fetchTunnel(ctx, name)
}

func createTunnelRoute(ctx context.Context, name, tunnelName, network string) *cfztv1alpha1.CloudflareTunnelRoute {
	route := &cfztv1alpha1.CloudflareTunnelRoute{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: cfztv1alpha1.CloudflareTunnelRouteSpec{
			TunnelRef: cfztv1alpha1.TunnelRouteTunnelRef{Name: tunnelName},
			Network:   network,
		},
	}
	Expect(k8sClient.Create(ctx, route)).To(Succeed())
	return route
}

func fetchTunnelRoute(ctx context.Context, name string) *cfztv1alpha1.CloudflareTunnelRoute {
	route := &cfztv1alpha1.CloudflareTunnelRoute{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, route)).To(Succeed())
	return route
}

func reconcileRoute(ctx context.Context, reconciler *CloudflareTunnelRouteReconciler, name string) {
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
	Expect(err).NotTo(HaveOccurred())
}
