package controller

import (
	"context"
	"errors"
	"sort"
	"time"

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
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
	"github.com/andrewreid/cfzt-operator/internal/naming"
	"github.com/andrewreid/cfzt-operator/internal/origin"
	"github.com/andrewreid/cfzt-operator/internal/ownership"
)

const exposureTestNamespace = "media"

var _ = Describe("CloudflareExposure Controller", func() {
	const namespace = exposureTestNamespace

	var (
		ctx                 context.Context
		fakeCF              *cloudflare.FakeClient
		tunnelReconciler    *CloudflareTunnelReconciler
		exposureReconciler  *CloudflareExposureReconciler
		policyReconciler    *CloudflareAccessPolicyReconciler
		tunnelRecorder      *record.FakeRecorder
		exposureRecorder    *record.FakeRecorder
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
		tunnelRecorder = record.NewFakeRecorder(1024)
		exposureRecorder = record.NewFakeRecorder(1024)
		tunnelReconciler = &CloudflareTunnelReconciler{
			Base: Base{
				Client:              indexedClient,
				Scheme:              indexedClient.Scheme(),
				NewCloudflareClient: newFakeCloudflareClient(indexedClient, fakeCF),
				Recorder:            tunnelRecorder,
			},
		}
		exposureReconciler = &CloudflareExposureReconciler{
			Base: Base{
				Client:              indexedClient,
				Scheme:              indexedClient.Scheme(),
				NewCloudflareClient: newFakeCloudflareClient(indexedClient, fakeCF),
				Recorder:            exposureRecorder,
			},
		}
		policyReconciler = &CloudflareAccessPolicyReconciler{
			Base: Base{
				Client:              indexedClient,
				Scheme:              indexedClient.Scheme(),
				NewCloudflareClient: newFakeCloudflareClient(indexedClient, fakeCF),
				Recorder:            newTestRecorder(),
			},
		}
	})

	It("TestAccessOwnerTagsFitCloudflareLimit", func() {
		uid := types.UID("c4fe2a8a-39b3-48cd-9a2a-d471cb045b4a")
		tags := ownership.From(uid).Tags()

		for _, tag := range tags {
			Expect(len(tag)).To(BeNumerically("<=", 35))
		}
		Expect(ownership.From(uid).MatchesTags(tags)).To(BeTrue())
	})

	It("TestExposureCreate", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, defaultTunnelName, defaultTunnelCFName)
		exposure := createExposure(ctx, "jellyfin", tunnel.Name, "jellyfin.example.com", true)

		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, exposure.Name)
		Expect(current.Status.Cloudflare.AccessApplications).To(HaveLen(1))
		Expect(current.Status.Cloudflare.AccessApplications[0].AppID).NotTo(BeEmpty())
		Expect(current.Status.Cloudflare.AccessApplications[0].CanonicalDomainHash).NotTo(BeEmpty())
		Expect(current.Status.Cloudflare.AccessApplications[0].PolicyHash).NotTo(BeEmpty())
		Expect(current.Status.Cloudflare.DnsRecordId).NotTo(BeEmpty())
		Expect(current.Status.Cloudflare.PublicHostnameRouteHash).NotTo(BeEmpty())
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))

		cfTunnel := fetchTunnel(ctx, tunnel.Name)
		config, err := fakeCF.Configuration(cfTunnel.Status.TunnelId)
		Expect(err).NotTo(HaveOccurred())
		Expect(config.Ingress[0].Hostname).To(Equal("jellyfin.example.com"))
		Expect(config.Ingress[0].Service).To(Equal("http://jellyfin.media.svc.cluster.local:8096"))
		records, err := fakeCF.DNSRecords().List(ctx, "zone-example", "jellyfin.example.com", "CNAME")
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(HaveLen(1))
		Expect(records[0].Type).To(Equal("CNAME"))
		Expect(records[0].Content).To(Equal(cfTunnel.Status.TunnelId + ".cfargotunnel.com"))
		Expect(records[0].Proxied).To(BeTrue())
		apps, err := fakeCF.AccessApplications().List(ctx, "jellyfin.example.com")
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(HaveLen(1))
		Expect(apps[0].Name).To(Equal("jellyfin-root-cfzt"))
		Expect(apps[0].Domains).To(Equal([]string{"jellyfin.example.com"}))
		Expect(apps[0].PolicyUUIDs).To(Equal([]string{defaultPolicyUUID}))
		Expect(current.Status.Cloudflare.AccessApplications[0].Name).To(Equal("root"))
		for _, tag := range apps[0].Tags {
			Expect(fakeCF.AccessTags().Delete(ctx, tag)).To(Succeed())
		}
	})

	It("TestExposureReadyStatusIsStableOnAccessNoop", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "stable-status", "stable-status")
		exposure := createExposure(ctx, "stable-status", tunnel.Name, "stable-status.example.com", true)

		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, exposure.Name)
		ready := meta.FindStatusCondition(current.Status.Conditions, ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		transitionBefore := ready.LastTransitionTime
		resourceVersionBefore := current.ResourceVersion
		time.Sleep(10 * time.Millisecond)

		reconcileExposure(ctx, exposureReconciler, current)

		updated := fetchExposure(ctx, exposure.Name)
		updatedReady := meta.FindStatusCondition(updated.Status.Conditions, ConditionReady)
		Expect(updatedReady).NotTo(BeNil())
		Expect(updatedReady.Status).To(Equal(metav1.ConditionTrue))
		Expect(updatedReady.Reason).To(Equal(ReasonReconciled))
		Expect(updatedReady.LastTransitionTime).To(Equal(transitionBefore))
		Expect(updated.ResourceVersion).To(Equal(resourceVersionBefore))
	})

	It("TestTunnelConfigUpdateSkippedWhenUnchanged", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "skip-config", "skip-config")
		drainRecordedEvents(tunnelRecorder)
		exposure := createExposure(ctx, "skip-config-app", tunnel.Name, "skip.example.com", false)
		reconcileExposure(ctx, exposureReconciler, exposure)
		cfTunnel := fetchTunnel(ctx, tunnel.Name)
		before := fakeCF.ConfigurationUpdateCalls(cfTunnel.Status.TunnelId)

		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		expectRecordedEvent(tunnelRecorder, EventUpdatedTunnelConfig)

		afterWrite := fakeCF.ConfigurationUpdateCalls(cfTunnel.Status.TunnelId)
		Expect(afterWrite).To(Equal(before + 1))
		Expect(fetchTunnel(ctx, tunnel.Name).Status.IngressDocHash).NotTo(BeEmpty())

		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)

		Expect(fakeCF.ConfigurationUpdateCalls(cfTunnel.Status.TunnelId)).To(Equal(afterWrite))
		expectNoRecordedEvent(tunnelRecorder, EventUpdatedTunnelConfig)
	})

	It("TestTunnelConfigUpdateOnDrift", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "drift-config", "drift-config")
		exposure := createExposure(ctx, "drift-config-app", tunnel.Name, "drift.example.com", false)
		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		cfTunnel := fetchTunnel(ctx, tunnel.Name)
		afterFirstWrite := fakeCF.ConfigurationUpdateCalls(cfTunnel.Status.TunnelId)

		current := fetchExposure(ctx, exposure.Name)
		current.Spec.Origin.Port = 9090
		Expect(k8sClient.Update(ctx, current)).To(Succeed())

		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)

		Expect(fakeCF.ConfigurationUpdateCalls(cfTunnel.Status.TunnelId)).To(Equal(afterFirstWrite + 1))
		config, err := fakeCF.Configuration(cfTunnel.Status.TunnelId)
		Expect(err).NotTo(HaveOccurred())
		Expect(config.Ingress[0].Service).To(Equal("http://drift-config-app.media.svc.cluster.local:9090"))
	})

	It("TestExposureDNSManagedOff", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "dns-off", "dns-off")
		tunnel.Spec.Dns.Manage = false
		Expect(k8sClient.Update(ctx, tunnel)).To(Succeed())
		exposure := createExposure(ctx, "nodns", tunnel.Name, "nodns.example.com", true)

		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, exposure.Name)
		Expect(current.Status.Cloudflare.DnsRecordId).To(BeEmpty())
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		records, err := fakeCF.DNSRecords().List(ctx, "zone-example", "nodns.example.com", "CNAME")
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(BeEmpty())
	})

	It("TestExposureDNSCreateEventNoDuplicate", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "dns-event-create", "dns-event-create")
		exposure := createExposure(ctx, "dns-event-create", tunnel.Name, "dns-event-create.example.com", false)

		reconcileExposure(ctx, exposureReconciler, exposure)
		expectRecordedEvent(exposureRecorder, EventCreatedDNSRecord)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)

		expectNoRecordedEvent(exposureRecorder, EventCreatedDNSRecord)
		expectNoRecordedEvent(exposureRecorder, EventUpdatedDNSRecord)
	})

	It("TestExposureDNSUpdateEvent", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "dns-event-update", "dns-event-update")
		exposure := createExposure(ctx, "dns-event-update", tunnel.Name, "dns-event-update.example.com", false)
		reconcileExposure(ctx, exposureReconciler, exposure)
		drainRecordedEvents(exposureRecorder)
		records, err := fakeCF.DNSRecords().List(ctx, "zone-example", "dns-event-update.example.com", "CNAME")
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(HaveLen(1))
		_, err = fakeCF.DNSRecords().Update(ctx, records[0].ID, cloudflare.DNSRecordInput{
			ZoneID:  records[0].ZoneID,
			Name:    records[0].Name,
			Type:    records[0].Type,
			Content: "stale.cfargotunnel.com",
			Proxied: false,
			Comment: records[0].Comment,
		})
		Expect(err).NotTo(HaveOccurred())

		reconcileExposure(ctx, exposureReconciler, exposure)

		expectRecordedEvent(exposureRecorder, EventUpdatedDNSRecord)
	})

	It("TestExposureAccessDisabled", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "access-off", "access-off")
		exposure := createExposure(ctx, "noauth", tunnel.Name, "noauth.example.com", false)

		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, exposure.Name)
		Expect(current.Status.Cloudflare.AccessApplications).To(BeEmpty())
		Expect(current.Status.Cloudflare.DnsRecordId).NotTo(BeEmpty())
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
	})

	It("TestExposureAccessApplicationsCreateRootAndPath", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "access-multi", "access-multi")
		exposure := createExposure(ctx, "multi-access", tunnel.Name, "multi-access.example.com", true)
		exposure.Spec.Access.Applications = []cfztv1alpha1.AccessApplicationTarget{
			{
				Name:    "root",
				Domains: []cfztv1alpha1.AccessApplicationDomain{cfztv1alpha1.AccessApplicationDomain("multi-access.example.com")},
				Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{{
					PolicyRef: cfztv1alpha1.AccessPolicyRef{UUID: defaultPolicyUUID},
				}},
			},
			{
				Name: "admin",
				Domains: []cfztv1alpha1.AccessApplicationDomain{
					cfztv1alpha1.AccessApplicationDomain("multi-access.example.com/admin"),
				},
				Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{{
					PolicyRef: cfztv1alpha1.AccessPolicyRef{UUID: defaultPolicyUUID},
				}},
			},
		}
		Expect(k8sClient.Update(ctx, exposure)).To(Succeed())

		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, exposure.Name)
		Expect(current.Status.Cloudflare.AccessApplications).To(HaveLen(2))
		Expect([]string{
			current.Status.Cloudflare.AccessApplications[0].Name,
			current.Status.Cloudflare.AccessApplications[1].Name,
		}).To(Equal([]string{"admin", "root"}))
		for _, statusEntry := range current.Status.Cloudflare.AccessApplications {
			Expect(statusEntry.AppID).NotTo(BeEmpty())
			Expect(statusEntry.CanonicalDomainHash).NotTo(BeEmpty())
			Expect(statusEntry.PolicyHash).NotTo(BeEmpty())
		}
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))

		apps, err := fakeCF.AccessApplications().List(ctx, exposure.Spec.Hostname)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(HaveLen(2))
		sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
		Expect(apps[0].Name).To(Equal("multi-access-admin-cfzt"))
		Expect(apps[1].Name).To(Equal("multi-access-root-cfzt"))
		Expect(apps[0].Domains).To(Equal([]string{"multi-access.example.com/admin"}))
		Expect(apps[1].Domains).To(Equal([]string{"multi-access.example.com"}))
	})

	It("TestExposureAccessDisabledDeletesOwnedLiveApps", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "access-toggle", "access-toggle")
		exposure := createExposure(ctx, "toggleauth", tunnel.Name, "toggleauth.example.com", true)
		exposure.Spec.Access.Applications = []cfztv1alpha1.AccessApplicationTarget{
			{
				Name:    "root",
				Domains: []cfztv1alpha1.AccessApplicationDomain{cfztv1alpha1.AccessApplicationDomain("toggleauth.example.com")},
				Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{{
					PolicyRef: cfztv1alpha1.AccessPolicyRef{UUID: defaultPolicyUUID},
				}},
			},
			{
				Name: "admin",
				Domains: []cfztv1alpha1.AccessApplicationDomain{
					cfztv1alpha1.AccessApplicationDomain("toggleauth.example.com/admin"),
				},
				Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{{
					PolicyRef: cfztv1alpha1.AccessPolicyRef{UUID: defaultPolicyUUID},
				}},
			},
		}
		Expect(k8sClient.Update(ctx, exposure)).To(Succeed())
		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, exposure.Name)
		Expect(current.Status.Cloudflare.AccessApplications).To(HaveLen(2))
		_, err := fakeCF.AccessApplications().Create(ctx, cloudflare.AccessApplicationInput{
			Name:        "toggleauth-reports-cfzt",
			Domains:     []string{"toggleauth.example.com/reports"},
			PolicyUUIDs: []string{defaultPolicyUUID},
			Tags:        exposureOwner(current).Tags(),
		})
		Expect(err).NotTo(HaveOccurred())
		drainRecordedEvents(exposureRecorder)
		current.Spec.Access.Enabled = false
		Expect(k8sClient.Update(ctx, current)).To(Succeed())
		reconcileExposure(ctx, exposureReconciler, current)
		expectRecordedEvent(exposureRecorder, EventDeletedAccessApp)

		updated := fetchExposure(ctx, exposure.Name)
		Expect(updated.Status.Cloudflare.AccessApplications).To(BeEmpty())
		apps, err := fakeCF.AccessApplications().List(ctx, "toggleauth.example.com")
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(BeEmpty())
	})

	It("TestExposureDNSManageToggleOffDeletesOwnedRecord", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "dns-toggle", "dns-toggle")
		exposure := createExposure(ctx, "toggledns", tunnel.Name, "toggledns.example.com", false)
		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, exposure.Name)
		Expect(current.Status.Cloudflare.DnsRecordId).NotTo(BeEmpty())
		drainRecordedEvents(exposureRecorder)
		latestTunnel := fetchTunnel(ctx, tunnel.Name)
		latestTunnel.Spec.Dns.Manage = false
		Expect(k8sClient.Update(ctx, latestTunnel)).To(Succeed())
		reconcileExposure(ctx, exposureReconciler, current)
		expectRecordedEvent(exposureRecorder, EventDeletedDNSRecord)

		updated := fetchExposure(ctx, exposure.Name)
		Expect(updated.Status.Cloudflare.DnsRecordId).To(BeEmpty())
		records, err := fakeCF.DNSRecords().List(ctx, "zone-example", "toggledns.example.com", "CNAME")
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(BeEmpty())
	})

	It("TestExposureHostnameConflictRequeuesAt30s", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "conflict-tunnel", "conflict-tunnel")
		first := createExposure(ctx, "one", tunnel.Name, "same.example.com", true)
		second := createExposure(ctx, "two", tunnel.Name, "same.example.com", true)

		reconcileExposureExpectRequeueAfter30(ctx, exposureReconciler, first)
		reconcileExposureExpectRequeueAfter30(ctx, exposureReconciler, second)

		Expect(meta.FindStatusCondition(fetchExposure(ctx, first.Name).Status.Conditions, ConditionReady).Reason).To(Equal(ReasonHostnameConflict))
		Expect(meta.FindStatusCondition(fetchExposure(ctx, second.Name).Status.Conditions, ConditionReady).Reason).To(Equal(ReasonHostnameConflict))
	})

	It("TestExposureCloudflareWriteFailureBacksOff", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "dns-write-failure-tunnel", "dns-write-failure-tunnel")
		exposure := createExposure(ctx, "dns-write-failure", tunnel.Name, "dns-write-failure.example.com", false)
		writeErr := errors.New("dns create failed")
		reconciler := &CloudflareExposureReconciler{
			Base: Base{
				Client: indexedClient,
				Scheme: indexedClient.Scheme(),
				NewCloudflareClient: func(ctx context.Context, ref CredentialsRef) (cloudflare.Client, error) {
					client, err := newFakeCloudflareClient(indexedClient, fakeCF)(ctx, ref)
					if err != nil {
						return nil, err
					}
					return dnsCreateErrorClient{Client: client, err: writeErr}, nil
				},
				Recorder: newTestRecorder(),
			},
		}

		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Name}})

		Expect(result).To(Equal(reconcile.Result{}))
		Expect(err).To(MatchError(ContainSubstring(ReasonDNSWriteFailed)))
		Expect(err).To(MatchError(ContainSubstring(writeErr.Error())))
		current := fetchExposure(ctx, exposure.Name)
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonDNSWriteFailed))
	})

	It("TestExposureIndexedHostnameConflictLookup", func() {
		first := &cfztv1alpha1.CloudflareExposure{
			ObjectMeta: metav1.ObjectMeta{Name: "indexed-one", Namespace: namespace, UID: types.UID("indexed-one")},
			Spec: cfztv1alpha1.CloudflareExposureSpec{
				Hostname:  "indexed.example.com",
				TunnelRef: cfztv1alpha1.TunnelRef{Name: "indexed-tunnel"},
			},
		}
		second := &cfztv1alpha1.CloudflareExposure{
			ObjectMeta: metav1.ObjectMeta{Name: "indexed-two", Namespace: namespace, UID: types.UID("indexed-two")},
			Spec: cfztv1alpha1.CloudflareExposureSpec{
				Hostname:  "indexed.example.com",
				TunnelRef: cfztv1alpha1.TunnelRef{Name: "indexed-tunnel"},
			},
		}
		otherTunnel := &cfztv1alpha1.CloudflareExposure{
			ObjectMeta: metav1.ObjectMeta{Name: "indexed-other-tunnel", Namespace: namespace, UID: types.UID("indexed-other")},
			Spec: cfztv1alpha1.CloudflareExposureSpec{
				Hostname:  "indexed.example.com",
				TunnelRef: cfztv1alpha1.TunnelRef{Name: "other-tunnel"},
			},
		}
		localClient := fakeclient.NewClientBuilder().
			WithScheme(k8sClient.Scheme()).
			WithIndex(&cfztv1alpha1.CloudflareExposure{}, exposureIndexHostname, func(obj client.Object) []string {
				exposure := obj.(*cfztv1alpha1.CloudflareExposure)
				return []string{exposure.Spec.Hostname}
			}).
			WithObjects(first, second, otherTunnel).
			Build()
		localReconciler := &CloudflareExposureReconciler{Base: Base{Client: localClient}}

		conflict, err := localReconciler.hasDuplicateHostname(ctx, first)
		Expect(err).NotTo(HaveOccurred())
		Expect(conflict).To(BeTrue())

		conflict, err = localReconciler.hasDuplicateHostname(ctx, otherTunnel)
		Expect(err).NotTo(HaveOccurred())
		Expect(conflict).To(BeFalse())
	})

	It("TestExposureForeignTaggedAccessApplicationIsHostnameConflict", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "foreign-resource", "foreign-resource")
		_, err := fakeCF.AccessApplications().Create(ctx, cloudflare.AccessApplicationInput{
			Name:        "manual",
			Domains:     []string{"foreign.example.com"},
			PolicyUUIDs: []string{defaultPolicyUUID},
			Tags:        []string{"managed-by=cfzt-operator", "source-uid=other-exposure"},
		})
		Expect(err).NotTo(HaveOccurred())
		exposure := createExposure(ctx, "foreign", tunnel.Name, "foreign.example.com", true)

		reconcileExposureExpectRequeueAfter30(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, exposure.Name)
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonHostnameConflict))
		Expect(current.Status.Cloudflare.AccessApplications).To(BeEmpty())
		apps, err := fakeCF.AccessApplications().List(ctx, exposure.Spec.Hostname)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(HaveLen(1))
		Expect(apps[0].Name).To(Equal("manual"))
		records, err := fakeCF.DNSRecords().List(ctx, "zone-example", exposure.Spec.Hostname, "CNAME")
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(BeEmpty())
	})

	It("TestExposureForeignMixedDomainAccessApplicationIsHostnameConflict", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "foreign-mixed", "foreign-mixed")
		_, err := fakeCF.AccessApplications().Create(ctx, cloudflare.AccessApplicationInput{
			Name:        "manual",
			Domains:     []string{"foreign-mixed.example.com", "foreign-mixed.example.com/reports", "other.example.net/reports"},
			PolicyUUIDs: []string{defaultPolicyUUID},
			Tags:        []string{"managed-by=cfzt-operator", "source-uid=other-exposure"},
		})
		Expect(err).NotTo(HaveOccurred())
		exposure := createExposure(ctx, "foreign-mixed", tunnel.Name, "foreign-mixed.example.com", true)

		reconcileExposureExpectRequeueAfter30(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, exposure.Name)
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonHostnameConflict))
		Expect(current.Status.Cloudflare.AccessApplications).To(BeEmpty())
		apps, err := fakeCF.AccessApplications().List(ctx, exposure.Spec.Hostname)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(HaveLen(1))
		Expect(apps[0].Name).To(Equal("manual"))
		Expect(apps[0].Domains).To(ConsistOf("foreign-mixed.example.com", "foreign-mixed.example.com/reports", "other.example.net/reports"))
		records, err := fakeCF.DNSRecords().List(ctx, "zone-example", exposure.Spec.Hostname, "CNAME")
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(BeEmpty())
	})

	It("TestExposureAccessApplicationsDuplicateCanonicalCoverageRejected", func() {
		exposure := &cfztv1alpha1.CloudflareExposure{
			ObjectMeta: metav1.ObjectMeta{Name: "dup-canon", Namespace: namespace, UID: types.UID("dup-canon")},
			Spec: cfztv1alpha1.CloudflareExposureSpec{
				Hostname: "dup-canon.example.com",
				Access: cfztv1alpha1.AccessSpec{
					Enabled: true,
					Applications: []cfztv1alpha1.AccessApplicationTarget{
						{
							Name:    "root",
							Domains: []cfztv1alpha1.AccessApplicationDomain{cfztv1alpha1.AccessApplicationDomain("dup-canon.example.com")},
							Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{{
								PolicyRef: cfztv1alpha1.AccessPolicyRef{UUID: defaultPolicyUUID},
							}},
						},
						{
							Name:    "dup",
							Domains: []cfztv1alpha1.AccessApplicationDomain{cfztv1alpha1.AccessApplicationDomain("dup-canon.example.com/*")},
							Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{{
								PolicyRef: cfztv1alpha1.AccessPolicyRef{UUID: defaultPolicyUUID},
							}},
						},
					},
				},
			},
		}
		_, err := exposureReconciler.desiredAccessApplications(ctx, exposure)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("both cover \"dup-canon.example.com\""))
	})

	It("TestExposureAccessApplicationsDuplicateCanonicalCoverageDoesNotMutate", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "dup-canon-reconcile", "dup-canon-reconcile")
		exposure := createExposure(ctx, "dup-canon-reconcile", tunnel.Name, "dup-canon-reconcile.example.com", true)
		exposure.Spec.Access.Applications[0].Domains = append(
			exposure.Spec.Access.Applications[0].Domains,
			cfztv1alpha1.AccessApplicationDomain("dup-canon-reconcile.example.com/*"),
		)
		Expect(k8sClient.Update(ctx, exposure)).To(Succeed())

		reconcileExposureExpectRequeueAfter30(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, exposure.Name)
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonHostnameConflict))
		Expect(current.Status.Cloudflare.AccessApplications).To(BeEmpty())
		Expect(current.Status.Cloudflare.DnsRecordId).To(BeEmpty())
		apps, err := fakeCF.AccessApplications().List(ctx, exposure.Spec.Hostname)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(BeEmpty())
		records, err := fakeCF.DNSRecords().List(ctx, "zone-example", exposure.Spec.Hostname, "CNAME")
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(BeEmpty())
	})

	It("TestExposureAccessApplicationsDuplicateCanonicalCoverageAcrossAppsDoesNotMutate", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "dup-canon-apps", "dup-canon-apps")
		exposure := createExposure(ctx, "dup-canon-apps", tunnel.Name, "dup-canon-apps.example.com", true)
		exposure.Spec.Access.Applications = append(exposure.Spec.Access.Applications, cfztv1alpha1.AccessApplicationTarget{
			Name:    "wildcard",
			Domains: []cfztv1alpha1.AccessApplicationDomain{cfztv1alpha1.AccessApplicationDomain("dup-canon-apps.example.com/*")},
			Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{{
				PolicyRef: cfztv1alpha1.AccessPolicyRef{UUID: defaultPolicyUUID},
			}},
		})
		Expect(k8sClient.Update(ctx, exposure)).To(Succeed())

		reconcileExposureExpectRequeueAfter30(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, exposure.Name)
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonHostnameConflict))
		Expect(current.Status.Cloudflare.AccessApplications).To(BeEmpty())
		Expect(current.Status.Cloudflare.DnsRecordId).To(BeEmpty())
		apps, err := fakeCF.AccessApplications().List(ctx, exposure.Spec.Hostname)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(BeEmpty())
		records, err := fakeCF.DNSRecords().List(ctx, "zone-example", exposure.Spec.Hostname, "CNAME")
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(BeEmpty())
	})

	It("TestExposureDNSForeignRecordConflict", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "foreign-dns", "foreign-dns")
		_, err := fakeCF.DNSRecords().Create(ctx, cloudflare.DNSRecordInput{
			ZoneID:  "zone-example",
			Name:    "foreign-dns.example.com",
			Type:    "CNAME",
			Content: "manual.example.com",
			Proxied: true,
			Comment: "managed-by=cfzt-operator source-uid=other-exposure",
		})
		Expect(err).NotTo(HaveOccurred())
		exposure := createExposure(ctx, "foreign-dns", tunnel.Name, "foreign-dns.example.com", false)

		reconcileExposureExpectRequeueAfter30(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, exposure.Name)
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonHostnameConflict))
	})

	It("TestExposureFinalizer", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "delete-exposure", "delete-exposure")
		exposure := createExposure(ctx, "delete-me", tunnel.Name, "delete-me.example.com", true)
		exposure.Spec.Access.Applications = []cfztv1alpha1.AccessApplicationTarget{
			{
				Name:    "root",
				Domains: []cfztv1alpha1.AccessApplicationDomain{cfztv1alpha1.AccessApplicationDomain("delete-me.example.com")},
				Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{{
					PolicyRef: cfztv1alpha1.AccessPolicyRef{UUID: defaultPolicyUUID},
				}},
			},
			{
				Name: "admin",
				Domains: []cfztv1alpha1.AccessApplicationDomain{
					cfztv1alpha1.AccessApplicationDomain("delete-me.example.com/admin"),
				},
				Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{{
					PolicyRef: cfztv1alpha1.AccessPolicyRef{UUID: defaultPolicyUUID},
				}},
			},
		}
		Expect(k8sClient.Update(ctx, exposure)).To(Succeed())
		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)
		current := fetchExposure(ctx, exposure.Name)
		Expect(current.Finalizers).To(ContainElement(naming.Finalizer))
		Expect(current.Status.Cloudflare.AccessApplications).To(HaveLen(2))
		appIDs := []string{current.Status.Cloudflare.AccessApplications[0].AppID, current.Status.Cloudflare.AccessApplications[1].AppID}
		recordID := current.Status.Cloudflare.DnsRecordId
		_, err := fakeCF.AccessApplications().Create(ctx, cloudflare.AccessApplicationInput{
			Name:        "delete-me-reports-cfzt",
			Domains:     []string{"delete-me.example.com/reports"},
			PolicyUUIDs: []string{defaultPolicyUUID},
			Tags:        exposureOwner(current).Tags(),
		})
		Expect(err).NotTo(HaveOccurred())
		drainRecordedEvents(exposureRecorder)

		Expect(k8sClient.Delete(ctx, current)).To(Succeed())
		reconcileExposure(ctx, exposureReconciler, current)
		expectRecordedEvent(exposureRecorder, EventDeletedDNSRecord)

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
		config, err := fakeCF.Configuration(fetchTunnel(ctx, tunnel.Name).Status.TunnelId)
		Expect(err).NotTo(HaveOccurred())
		Expect(config.Ingress).To(HaveLen(1))
		Expect(config.Ingress[0].Service).To(Equal("http_status:404"))
		for _, appID := range appIDs {
			Expect(appID).NotTo(BeEmpty())
		}
		Expect(recordID).NotTo(BeEmpty())
	})

	It("TestExposureFinalizerMissingCredentialsRequeues", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "delete-missing-creds", "delete-missing-creds")
		exposure := createExposure(ctx, "delete-missing-creds", tunnel.Name, "delete-missing-creds.example.com", true)
		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)
		current := fetchExposure(ctx, exposure.Name)
		Expect(current.Status.Cloudflare.AccessApplications).To(HaveLen(1))
		Expect(current.Status.Cloudflare.DnsRecordId).NotTo(BeEmpty())
		Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-credentials", Namespace: "cfzt-system"}})).To(Succeed())

		Expect(k8sClient.Delete(ctx, current)).To(Succeed())
		result, err := exposureReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: current.Namespace, Name: current.Name}})

		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())
		blocked := fetchExposure(ctx, exposure.Name)
		Expect(blocked.Finalizers).To(ContainElement(naming.Finalizer))
		Expect(meta.FindStatusCondition(blocked.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonCredentialsMissing))

		createCredentials(ctx)
		reconcileExposure(ctx, exposureReconciler, blocked)
		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: exposure.Name}, &cfztv1alpha1.CloudflareExposure{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())
	})

	It("TestExposureFinalizerFastPathWithoutTrackedAccessOrDNSDoesNotNeedCredentials", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "delete-fast-path", "delete-fast-path")
		tunnel.Spec.Dns.Manage = false
		Expect(k8sClient.Update(ctx, tunnel)).To(Succeed())
		exposure := createExposure(ctx, "delete-fast-path", tunnel.Name, "delete-fast-path.example.com", false)
		reconcileExposure(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, exposure.Name)
		Expect(current.Finalizers).To(ContainElement(naming.Finalizer))
		Expect(current.Status.Cloudflare.AccessApplications).To(BeEmpty())
		Expect(current.Status.Cloudflare.DnsRecordId).To(BeEmpty())
		Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cloudflare-credentials", Namespace: "cfzt-system"}})).To(Succeed())

		Expect(k8sClient.Delete(ctx, current)).To(Succeed())
		reconcileExposure(ctx, exposureReconciler, current)

		Eventually(func(g Gomega) {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: exposure.Name}, &cfztv1alpha1.CloudflareExposure{})
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())
	})

	It("TestTunnelBlockedByExposures", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "blocked-tunnel", "blocked-tunnel")
		_ = createExposure(ctx, "still-here", tunnel.Name, "still-here.example.com", false)

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
		requests := enqueueNamed(func(exposure *cfztv1alpha1.CloudflareExposure) []types.NamespacedName {
			if exposure.Spec.TunnelRef.Name == "" {
				return nil
			}
			return []types.NamespacedName{{Name: exposure.Spec.TunnelRef.Name}}
		})(ctx, exposure)
		Expect(requests).To(HaveLen(1))
		Expect(requests[0].Name).To(Equal("mapped-tunnel"))
	})

	It("TestTunnelStatusUpdatePropagatesToExposures", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "watch-tunnel", "watch-tunnel")
		first := createExposure(ctx, "watch-one", tunnel.Name, "watch-one.example.com", false)
		second := createExposure(ctx, "watch-two", tunnel.Name, "watch-two.example.com", false)

		Eventually(func() []reconcile.Request {
			return enqueueNamed(func(tunnel *cfztv1alpha1.CloudflareTunnel) []types.NamespacedName {
				exposures, err := exposureReconciler.exposuresForTunnel(context.Background(), tunnel.Name)
				if err != nil {
					return nil
				}
				requests := make([]types.NamespacedName, 0, len(exposures))
				for _, exposure := range exposures {
					requests = append(requests, types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Name})
				}
				return requests
			})(ctx, tunnel)
		}).Should(ConsistOf(
			reconcile.Request{NamespacedName: types.NamespacedName{Namespace: first.Namespace, Name: first.Name}},
			reconcile.Request{NamespacedName: types.NamespacedName{Namespace: second.Namespace, Name: second.Name}},
		))
	})

	It("TestExposureAccessApplicationsResolveNestedPolicyRefs", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "policy-ref-name", "policy-ref-name")
		policy := createAccessPolicy(ctx, "family-only", "")
		reconcileAccessPolicy(ctx, policyReconciler, policy.Name)
		policy = fetchAccessPolicy(ctx, policy.Name)
		exposure := createExposure(ctx, "named-policy", tunnel.Name, "named-policy.example.com", true)
		exposure.Spec.Access.Applications = []cfztv1alpha1.AccessApplicationTarget{
			{
				Name:    "root",
				Domains: []cfztv1alpha1.AccessApplicationDomain{cfztv1alpha1.AccessApplicationDomain("named-policy.example.com")},
				Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{
					{PolicyRef: cfztv1alpha1.AccessPolicyRef{Name: policy.Name}},
					{PolicyRef: cfztv1alpha1.AccessPolicyRef{UUID: defaultPolicyUUID}},
				},
			},
			{
				Name: "admin",
				Domains: []cfztv1alpha1.AccessApplicationDomain{
					cfztv1alpha1.AccessApplicationDomain("named-policy.example.com/admin"),
				},
				Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{
					{PolicyRef: cfztv1alpha1.AccessPolicyRef{UUID: defaultPolicyUUID}},
				},
			},
		}
		Expect(k8sClient.Update(ctx, exposure)).To(Succeed())

		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, exposure.Name)
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		Expect(current.Status.Cloudflare.AccessApplications).To(HaveLen(2))
		Expect([]string{
			current.Status.Cloudflare.AccessApplications[0].Name,
			current.Status.Cloudflare.AccessApplications[1].Name,
		}).To(Equal([]string{"admin", "root"}))
		apps, err := fakeCF.AccessApplications().List(ctx, exposure.Spec.Hostname)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(HaveLen(2))
		sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
		Expect(apps[0].PolicyUUIDs).To(Equal([]string{defaultPolicyUUID}))
		Expect(apps[1].PolicyUUIDs).To(Equal([]string{policy.Status.PolicyId, defaultPolicyUUID}))
	})

	It("TestExposureAccessApplicationsPolicyOrderAndDomainDriftUpdate", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "policy-drift-tunnel", "policy-drift-tunnel")
		policy := createAccessPolicy(ctx, "drift-policy-multiapp", "")
		reconcileAccessPolicy(ctx, policyReconciler, policy.Name)
		policy = fetchAccessPolicy(ctx, policy.Name)
		exposure := createExposure(ctx, "policy-drift", tunnel.Name, "policy-drift.example.com", true)
		exposure.Spec.Access.Applications = []cfztv1alpha1.AccessApplicationTarget{
			{
				Name:    "root",
				Domains: []cfztv1alpha1.AccessApplicationDomain{cfztv1alpha1.AccessApplicationDomain("policy-drift.example.com")},
				Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{
					{PolicyRef: cfztv1alpha1.AccessPolicyRef{UUID: defaultPolicyUUID}},
				},
			},
			{
				Name: "admin",
				Domains: []cfztv1alpha1.AccessApplicationDomain{
					cfztv1alpha1.AccessApplicationDomain("policy-drift.example.com/admin"),
					cfztv1alpha1.AccessApplicationDomain("policy-drift.example.com/admin/logs"),
				},
				Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{
					{PolicyRef: cfztv1alpha1.AccessPolicyRef{Name: policy.Name}},
					{PolicyRef: cfztv1alpha1.AccessPolicyRef{UUID: defaultPolicyUUID}},
				},
			},
		}
		Expect(k8sClient.Update(ctx, exposure)).To(Succeed())

		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)
		drainRecordedEvents(exposureRecorder)

		apps, err := fakeCF.AccessApplications().List(ctx, exposure.Spec.Hostname)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(HaveLen(2))
		sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
		admin := apps[0]
		Expect(admin.Name).To(Equal("policy-drift-admin-cfzt"))
		_, err = fakeCF.AccessApplications().Update(ctx, admin.ID, cloudflare.AccessApplicationInput{
			Name:        admin.Name,
			Domains:     []string{"policy-drift.example.com/admin/logs", "policy-drift.example.com/admin"},
			PolicyUUIDs: []string{defaultPolicyUUID, policy.Status.PolicyId},
			Tags:        admin.Tags,
		})
		Expect(err).NotTo(HaveOccurred())

		reconcileExposure(ctx, exposureReconciler, exposure)
		expectRecordedEvent(exposureRecorder, EventUpdatedAccessApp)

		current := fetchExposure(ctx, exposure.Name)
		Expect(current.Status.Cloudflare.AccessApplications).To(HaveLen(2))
		Expect([]string{
			current.Status.Cloudflare.AccessApplications[0].Name,
			current.Status.Cloudflare.AccessApplications[1].Name,
		}).To(Equal([]string{"admin", "root"}))
		apps, err = fakeCF.AccessApplications().List(ctx, exposure.Spec.Hostname)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(HaveLen(2))
		sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
		Expect(apps[0].Domains).To(Equal([]string{"policy-drift.example.com/admin", "policy-drift.example.com/admin/logs"}))
		Expect(apps[0].PolicyUUIDs).To(Equal([]string{policy.Status.PolicyId, defaultPolicyUUID}))
	})

	It("TestExposureOwnedMixedDomainAccessApplicationIsUpdated", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "owned-mixed", "owned-mixed")
		exposure := createExposure(ctx, "owned-mixed", tunnel.Name, "owned-mixed.example.com", true)
		_, err := fakeCF.AccessApplications().Create(ctx, cloudflare.AccessApplicationInput{
			Name:        desiredAccessApplicationName(exposure, "root"),
			Domains:     []string{"owned-mixed.example.com", "owned-mixed.example.com/reports", "other.example.net/reports"},
			PolicyUUIDs: []string{defaultPolicyUUID},
			Tags:        exposureOwner(exposure).Tags(),
		})
		Expect(err).NotTo(HaveOccurred())

		reconcileExposure(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, exposure.Name)
		Expect(current.Status.Cloudflare.AccessApplications).To(HaveLen(1))
		Expect(current.Status.Cloudflare.AccessApplications[0].Name).To(Equal("root"))
		apps, err := fakeCF.AccessApplications().List(ctx, exposure.Spec.Hostname)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(HaveLen(1))
		Expect(apps[0].Name).To(Equal(desiredAccessApplicationName(exposure, "root")))
		Expect(apps[0].Domains).To(Equal([]string{"owned-mixed.example.com"}))
	})

	It("TestExposureAccessApplicationsRemovedAppDeletesOnlyThatOwnedApp", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "removed-app", "removed-app")
		exposure := createExposure(ctx, "removed-app", tunnel.Name, "removed-app.example.com", true)
		exposure.Spec.Access.Applications = []cfztv1alpha1.AccessApplicationTarget{
			{
				Name:    "root",
				Domains: []cfztv1alpha1.AccessApplicationDomain{cfztv1alpha1.AccessApplicationDomain("removed-app.example.com")},
				Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{{
					PolicyRef: cfztv1alpha1.AccessPolicyRef{UUID: defaultPolicyUUID},
				}},
			},
			{
				Name: "admin",
				Domains: []cfztv1alpha1.AccessApplicationDomain{
					cfztv1alpha1.AccessApplicationDomain("removed-app.example.com/admin"),
				},
				Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{{
					PolicyRef: cfztv1alpha1.AccessPolicyRef{UUID: defaultPolicyUUID},
				}},
			},
		}
		Expect(k8sClient.Update(ctx, exposure)).To(Succeed())

		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)
		drainRecordedEvents(exposureRecorder)

		current := fetchExposure(ctx, exposure.Name)
		Expect(current.Status.Cloudflare.AccessApplications).To(HaveLen(2))
		_, err := fakeCF.AccessApplications().Create(ctx, cloudflare.AccessApplicationInput{
			Name:        "removed-app-reports-cfzt",
			Domains:     []string{"removed-app.example.com/reports"},
			PolicyUUIDs: []string{defaultPolicyUUID},
			Tags:        exposureOwner(current).Tags(),
		})
		Expect(err).NotTo(HaveOccurred())
		current.Spec.Access.Applications = current.Spec.Access.Applications[:1]
		Expect(k8sClient.Update(ctx, current)).To(Succeed())

		reconcileExposure(ctx, exposureReconciler, current)
		expectRecordedEvent(exposureRecorder, EventDeletedAccessApp)

		updated := fetchExposure(ctx, exposure.Name)
		Expect(updated.Status.Cloudflare.AccessApplications).To(HaveLen(1))
		Expect(updated.Status.Cloudflare.AccessApplications[0].Name).To(Equal("root"))
		apps, err := fakeCF.AccessApplications().List(ctx, exposure.Spec.Hostname)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(HaveLen(1))
		Expect(apps[0].Name).To(Equal("removed-app-root-cfzt"))
	})

	It("TestExposureAccessApplicationsPolicyRefNameNotReady", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "policy-not-ready", "policy-not-ready")
		policy := createAccessPolicy(ctx, "not-ready-policy", "")
		exposure := createExposure(ctx, "not-ready-exp", tunnel.Name, "not-ready.example.com", true)
		exposure.Spec.Access.Applications[0].Policies = []cfztv1alpha1.AccessApplicationPolicyBinding{{PolicyRef: cfztv1alpha1.AccessPolicyRef{Name: policy.Name}}}
		Expect(k8sClient.Update(ctx, exposure)).To(Succeed())

		result, err := exposureReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())

		current := fetchExposure(ctx, exposure.Name)
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonPolicyNotReady))
		Expect(current.Status.Cloudflare.AccessApplications).To(BeEmpty())
		apps, err := fakeCF.AccessApplications().List(ctx, exposure.Spec.Hostname)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(BeEmpty())
	})

	It("TestExposureAccessApplicationsPolicyRefNameStaleGenerationNotReady", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "policy-stale-generation", "policy-stale-generation")
		policy := createAccessPolicy(ctx, "stale-generation-policy", "")
		reconcileAccessPolicy(ctx, policyReconciler, policy.Name)
		readyPolicy := fetchAccessPolicy(ctx, policy.Name)
		readyPolicy.Spec.Rules.Include = []cfztv1alpha1.AccessRule{{EmailDomain: "edited.example.com"}}
		Expect(k8sClient.Update(ctx, readyPolicy)).To(Succeed())
		exposure := createExposure(ctx, "stale-generation-exp", tunnel.Name, "stale-generation.example.com", true)
		exposure.Spec.Access.Applications[0].Policies = []cfztv1alpha1.AccessApplicationPolicyBinding{{PolicyRef: cfztv1alpha1.AccessPolicyRef{Name: policy.Name}}}
		Expect(k8sClient.Update(ctx, exposure)).To(Succeed())

		result, err := exposureReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())

		current := fetchExposure(ctx, exposure.Name)
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonPolicyNotReady))
		Expect(current.Status.Cloudflare.AccessApplications).To(BeEmpty())
		apps, err := fakeCF.AccessApplications().List(ctx, exposure.Spec.Hostname)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(BeEmpty())
	})

	It("TestExposureAccessApplicationsPolicyRefNameDeletingPolicyNotReady", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "policy-deleting", "policy-deleting")
		policy := createAccessPolicy(ctx, "deleting-policy", "")
		reconcileAccessPolicy(ctx, policyReconciler, policy.Name)
		readyPolicy := fetchAccessPolicy(ctx, policy.Name)
		readyPolicy.Finalizers = []string{naming.Finalizer}
		Expect(k8sClient.Update(ctx, readyPolicy)).To(Succeed())
		Expect(k8sClient.Delete(ctx, readyPolicy)).To(Succeed())
		exposure := createExposure(ctx, "deleting-policy-exp", tunnel.Name, "deleting-policy.example.com", true)
		exposure.Spec.Access.Applications[0].Policies = []cfztv1alpha1.AccessApplicationPolicyBinding{{PolicyRef: cfztv1alpha1.AccessPolicyRef{Name: policy.Name}}}
		Expect(k8sClient.Update(ctx, exposure)).To(Succeed())

		result, err := exposureReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).NotTo(BeZero())

		current := fetchExposure(ctx, exposure.Name)
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonPolicyNotReady))
		Expect(current.Status.Cloudflare.AccessApplications).To(BeEmpty())
		apps, err := fakeCF.AccessApplications().List(ctx, exposure.Spec.Hostname)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(BeEmpty())
	})

	It("TestExposureAccessApplicationsPolicyRefNameMissingPolicyCR", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "policy-missing", "policy-missing")
		exposure := createExposure(ctx, "missing-policy-exp", tunnel.Name, "missing-policy.example.com", true)
		exposure.Spec.Access.Applications[0].Policies = []cfztv1alpha1.AccessApplicationPolicyBinding{{PolicyRef: cfztv1alpha1.AccessPolicyRef{Name: "missing-policy"}}}
		Expect(k8sClient.Update(ctx, exposure)).To(Succeed())

		reconcileExposure(ctx, exposureReconciler, exposure)

		current := fetchExposure(ctx, exposure.Name)
		Expect(meta.FindStatusCondition(current.Status.Conditions, ConditionReady).Reason).To(Equal(ReasonPolicyNotFound))
		Expect(current.Status.Cloudflare.AccessApplications).To(BeEmpty())
		apps, err := fakeCF.AccessApplications().List(ctx, exposure.Spec.Hostname)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps).To(BeEmpty())
	})

	It("TestPolicyStatusUpdatePropagatesToExposures", func() {
		policy := createAccessPolicy(ctx, "watch-policy", "")
		first := createPolicyRefExposure(ctx, "policy-watch-one", policy.Name)
		second := createPolicyRefExposure(ctx, "policy-watch-two", policy.Name)

		Eventually(func() []reconcile.Request {
			return enqueueNamed(func(policy *cfztv1alpha1.CloudflareAccessPolicy) []types.NamespacedName {
				exposures, err := exposureReconciler.exposuresForPolicy(context.Background(), policy.Name)
				if err != nil {
					return nil
				}
				requests := make([]types.NamespacedName, 0, len(exposures))
				for _, exposure := range exposures {
					requests = append(requests, types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Name})
				}
				return requests
			})(ctx, policy)
		}).Should(ConsistOf(
			reconcile.Request{NamespacedName: types.NamespacedName{Namespace: first.Namespace, Name: first.Name}},
			reconcile.Request{NamespacedName: types.NamespacedName{Namespace: second.Namespace, Name: second.Name}},
		))
	})

	It("TestExposureExternalOriginHostNetwork", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "external-origin", "external-origin")
		exposure := createExposure(ctx, "homeassistant", tunnel.Name, "ha.example.com", false)
		exposure.Spec.Origin = &cfztv1alpha1.OriginSpec{Protocol: "http", Host: "192.168.1.50", Port: 8123}
		Expect(k8sClient.Update(ctx, exposure)).To(Succeed())

		reconcileExposure(ctx, exposureReconciler, exposure)
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, exposure)

		cfTunnel := fetchTunnel(ctx, tunnel.Name)
		config, err := fakeCF.Configuration(cfTunnel.Status.TunnelId)
		Expect(err).NotTo(HaveOccurred())
		Expect(config.Ingress[0].Service).To(Equal("http://192.168.1.50:8123"))
		Expect(meta.FindStatusCondition(fetchExposure(ctx, exposure.Name).Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
	})

	It("TestSourceRefServiceSinglePort", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "source-service", "source-service")
		createService(ctx, namespace, "jellyfin", 8096)
		exposure := createSourceRefExposure(ctx, "jellyfin-source", tunnel.Name, "jellyfin-source.example.com", "jellyfin", false)

		reconcileExposure(ctx, exposureReconciler, exposure)
		current := fetchExposure(ctx, exposure.Name)
		Expect(current.Spec.Origin).To(Equal(&cfztv1alpha1.OriginSpec{Protocol: "http", Host: "jellyfin.media.svc.cluster.local", Port: 8096}))
		Expect(current.OwnerReferences).To(HaveLen(1))
		Expect(current.OwnerReferences[0].Kind).To(Equal(origin.ServiceKind))

		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, current)
		config, err := fakeCF.Configuration(fetchTunnel(ctx, tunnel.Name).Status.TunnelId)
		Expect(err).NotTo(HaveOccurred())
		Expect(config.Ingress[0].Service).To(Equal("http://jellyfin.media.svc.cluster.local:8096"))
		Expect(meta.FindStatusCondition(fetchExposure(ctx, exposure.Name).Status.Conditions, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
	})

	It("TestSourceRefServiceMultiPortRejected", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "source-multiport", "source-multiport")
		createService(ctx, namespace, "multiport", 8080, 9090)
		exposure := createSourceRefExposure(ctx, "multiport-source", tunnel.Name, "multiport-source.example.com", "multiport", false)

		reconcileExposure(ctx, exposureReconciler, exposure)

		ready := meta.FindStatusCondition(fetchExposure(ctx, exposure.Name).Status.Conditions, ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(ReasonOriginInvalid))
		Expect(ready.Message).To(ContainSubstring("has 2 ports"))
	})

	It("TestSourceRefOwnerReferenceAndFinalizerCleanup", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "source-delete", "source-delete")
		svc := createService(ctx, namespace, "delete-source", 8096)
		exposure := createSourceRefExposure(ctx, "delete-source-exp", tunnel.Name, "delete-source.example.com", svc.Name, true)
		reconcileExposure(ctx, exposureReconciler, exposure)
		current := fetchExposure(ctx, exposure.Name)
		Expect(current.OwnerReferences).To(HaveLen(1))
		reconcileTunnel(ctx, tunnelReconciler, tunnel.Name)
		reconcileExposure(ctx, exposureReconciler, current)
		current = fetchExposure(ctx, exposure.Name)
		Expect(current.Status.Cloudflare.AccessApplications).To(HaveLen(1))

		Expect(k8sClient.Delete(ctx, svc)).To(Succeed())
		// Envtest does not run the Kubernetes garbage collector. The source
		// ownerReference above is the cascade contract; this explicit delete
		// drives the finalizer cleanup path that GC would trigger in-cluster.
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
		localReconciler := &CloudflareExposureReconciler{Base: Base{Client: localClient, Scheme: k8sClient.Scheme()}, HTTPRouteSourceEnabled: true}

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
	createCredentials(ctx)
	reconcileTunnel(ctx, reconciler, tunnel.Name)
	markTunnelDaemonSetReady(ctx, tunnel.Name)
	reconcileTunnel(ctx, reconciler, tunnel.Name)
	return fetchTunnel(ctx, tunnel.Name)
}

func createExposure(ctx context.Context, name, tunnelName, hostname string, accessEnabled bool) *cfztv1alpha1.CloudflareExposure {
	exposure := &cfztv1alpha1.CloudflareExposure{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: exposureTestNamespace},
		Spec: cfztv1alpha1.CloudflareExposureSpec{
			Hostname:  hostname,
			TunnelRef: cfztv1alpha1.TunnelRef{Name: tunnelName},
			Origin: &cfztv1alpha1.OriginSpec{
				Protocol: "http",
				Host:     name + "." + exposureTestNamespace + ".svc.cluster.local",
				Port:     8096,
			},
			Access: cfztv1alpha1.AccessSpec{
				Enabled:      accessEnabled,
				Applications: defaultAccessApplications(hostname),
			},
		},
	}
	if !accessEnabled {
		exposure.Spec.Access.Applications = nil
	}
	Expect(k8sClient.Create(ctx, exposure)).To(Succeed())
	return exposure
}

func createSourceRefExposure(ctx context.Context, name, tunnelName, hostname, sourceName string, accessEnabled bool) *cfztv1alpha1.CloudflareExposure {
	exposure := &cfztv1alpha1.CloudflareExposure{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: exposureTestNamespace},
		Spec: cfztv1alpha1.CloudflareExposureSpec{
			Hostname:  hostname,
			TunnelRef: cfztv1alpha1.TunnelRef{Name: tunnelName},
			SourceRef: &cfztv1alpha1.SourceRef{
				ApiVersion: "v1",
				Kind:       origin.ServiceKind,
				Name:       sourceName,
			},
			Access: cfztv1alpha1.AccessSpec{
				Enabled:      accessEnabled,
				Applications: defaultAccessApplications(hostname),
			},
		},
	}
	if !accessEnabled {
		exposure.Spec.Access.Applications = nil
	}
	Expect(k8sClient.Create(ctx, exposure)).To(Succeed())
	return exposure
}

func defaultAccessApplications(hostname string) []cfztv1alpha1.AccessApplicationTarget {
	return []cfztv1alpha1.AccessApplicationTarget{{
		Name:    "root",
		Domains: []cfztv1alpha1.AccessApplicationDomain{cfztv1alpha1.AccessApplicationDomain(hostname)},
		Policies: []cfztv1alpha1.AccessApplicationPolicyBinding{{
			PolicyRef: cfztv1alpha1.AccessPolicyRef{UUID: "00000000-0000-4000-8000-000000000001"},
		}},
	}}
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

func fetchExposure(ctx context.Context, name string) *cfztv1alpha1.CloudflareExposure {
	exposure := &cfztv1alpha1.CloudflareExposure{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: exposureTestNamespace, Name: name}, exposure)).To(Succeed())
	return exposure
}

func reconcileExposure(ctx context.Context, reconciler *CloudflareExposureReconciler, exposure *cfztv1alpha1.CloudflareExposure) {
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Name}})
	Expect(err).NotTo(HaveOccurred())
}

func reconcileExposureExpectRequeueAfter30(ctx context.Context, reconciler *CloudflareExposureReconciler, exposure *cfztv1alpha1.CloudflareExposure) {
	result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Name}})
	Expect(result).To(Equal(reconcile.Result{RequeueAfter: 30 * time.Second}))
	Expect(err).NotTo(HaveOccurred())
}

type dnsCreateErrorClient struct {
	cloudflare.Client
	err error
}

func (c dnsCreateErrorClient) DNSRecords() cloudflare.DNSRecords {
	return dnsCreateErrorRecords{DNSRecords: c.Client.DNSRecords(), err: c.err}
}

type dnsCreateErrorRecords struct {
	cloudflare.DNSRecords
	err error
}

func (r dnsCreateErrorRecords) Create(context.Context, cloudflare.DNSRecordInput) (*cloudflare.DNSRecord, error) {
	return nil, r.err
}

func markTunnelDaemonSetReady(ctx context.Context, tunnelName string) {
	tunnel := fetchTunnel(ctx, tunnelName)
	ds := &appsv1.DaemonSet{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: tunnel.Spec.Cloudflared.Namespace, Name: naming.DaemonSetName(tunnel.Name)}, ds)).To(Succeed())
	markDaemonSetReady(ctx, ds)
}
