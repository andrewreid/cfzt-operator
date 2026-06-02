package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
	"github.com/andrewreid/cfzt-operator/internal/dr"
	"github.com/andrewreid/cfzt-operator/internal/naming"
	"github.com/andrewreid/cfzt-operator/internal/ownership"
)

var _ = Describe("CloudflareExposure failover role gate", func() {
	const (
		namespace = exposureTestNamespace
		siteSelf  = "site-self"
		sitePeer  = "site-peer"
		zoneName  = "example.com"
		zoneID    = "zone-example"
	)

	var (
		ctx              context.Context
		fakeCF           *cloudflare.FakeClient
		tunnelReconciler *CloudflareTunnelReconciler
		exposureRec      *CloudflareExposureReconciler
		exposureRecorder *record.FakeRecorder
		testNow          time.Time
	)

	BeforeEach(func() {
		ctx = context.Background()
		ensureNamespace(ctx, "cfzt-system")
		ensureNamespace(ctx, namespace)
		fakeCF = cloudflare.NewFake()
		fakeCF.AddZone(zoneID, zoneName)
		exposureRecorder = record.NewFakeRecorder(1024)
		tunnelReconciler = &CloudflareTunnelReconciler{
			Base: Base{
				Client:              indexedClient,
				Scheme:              indexedClient.Scheme(),
				NewCloudflareClient: newFakeCloudflareClient(indexedClient, fakeCF),
				Recorder:            record.NewFakeRecorder(1024),
			},
		}
		testNow = time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
		exposureRec = &CloudflareExposureReconciler{
			Base: Base{
				Client:              indexedClient,
				Scheme:              indexedClient.Scheme(),
				NewCloudflareClient: newFakeCloudflareClient(indexedClient, fakeCF),
				Recorder:            exposureRecorder,
			},
			SiteID: siteSelf,
			Now:    func() time.Time { return testNow },
		}
	})

	seedLease := func(group, site, tunnelID string, expires time.Time) {
		lease := dr.Lease{
			Version: dr.LeaseSchemaVersion,
			Site:    site,
			Tunnel:  tunnelID,
			Renewed: expires.Add(-60 * time.Second),
			Expires: expires,
		}
		_, err := fakeCF.DNSRecords().Create(ctx, cloudflare.DNSRecordInput{
			ZoneID:  zoneID,
			Name:    naming.FailoverLeaseTXTName(group, zoneName),
			Type:    "TXT",
			Content: lease.Serialize(),
			Comment: ownership.FromFailoverGroup(group).Comment(),
		})
		Expect(err).NotTo(HaveOccurred())
	}

	readLease := func(group string) (dr.Lease, bool) {
		records, err := fakeCF.DNSRecords().List(ctx, zoneID, naming.FailoverLeaseTXTName(group, zoneName), "TXT")
		Expect(err).NotTo(HaveOccurred())
		if len(records) == 0 {
			return dr.Lease{}, false
		}
		lease, perr := dr.ParseLease(records[0].Content)
		Expect(perr).NotTo(HaveOccurred())
		return lease, true
	}

	It("TestFailoverDNSManagedRequired", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "fo-nodns", "fo-nodns")
		tunnel.Spec.Dns.Manage = false
		Expect(k8sClient.Update(ctx, tunnel)).To(Succeed())
		exposure := createFailoverExposure(ctx, "fo-nodns-app", tunnel.Name, "fo-nodns.example.com", "fo-nodns-grp")

		reconcileExposure(ctx, exposureRec, exposure)

		current := fetchExposure(ctx, exposure.Name)
		ready := meta.FindStatusCondition(current.Status.Conditions, ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(ReasonFailoverRequiresManagedDNS))
		// No lease written.
		_, found := readLease("fo-nodns-grp")
		Expect(found).To(BeFalse())
	})

	It("TestFailoverAutoPromoteOnExpiry", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "fo-promote", "fo-promote")
		cfTunnel := fetchTunnel(ctx, tunnel.Name)
		exposure := createFailoverExposure(ctx, "fo-promote-app", tunnel.Name, "fo-promote.example.com", "fo-promote-grp")
		// Peer holds an already-expired lease.
		seedLease("fo-promote-grp", sitePeer, "peer-tunnel", testNow.Add(-1*time.Second))

		reconcileExposure(ctx, exposureRec, exposure)

		current := fetchExposure(ctx, exposure.Name)
		Expect(current.Status.Failover.Role).To(Equal(string(dr.RolePrimary)))
		Expect(current.Status.Failover.LeaseOwner).To(Equal(siteSelf))
		Expect(current.Status.Failover.LeaseExpiresAt).NotTo(BeNil())
		// Lease record now owned by this site, pointing at this tunnel.
		lease, found := readLease("fo-promote-grp")
		Expect(found).To(BeTrue())
		Expect(lease.Site).To(Equal(siteSelf))
		Expect(lease.Tunnel).To(Equal(cfTunnel.Status.TunnelId))
		// Public CNAME flips to this site's tunnel.
		records, err := fakeCF.DNSRecords().List(ctx, zoneID, "fo-promote.example.com", "CNAME")
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(HaveLen(1))
		Expect(records[0].Content).To(Equal(cfTunnel.Status.TunnelId + ".cfargotunnel.com"))
		// Events are asserted in emission order — expectRecordedEvent drains
		// the channel as it searches.
		expectRecordedEvent(exposureRecorder, EventLeaseAcquired)
		expectRecordedEvent(exposureRecorder, EventPromotedToPrimary)
	})

	It("TestFailoverReturnedPrimaryStandsDown", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "fo-return", "fo-return")
		exposure := createFailoverExposure(ctx, "fo-return-app", tunnel.Name, "fo-return.example.com", "fo-return-grp")
		// This CR's persisted status says it was Primary, but a peer now holds
		// a live lease — the returning primary must stand down.
		current := fetchExposure(ctx, exposure.Name)
		current.Status.Failover = cfztv1alpha1.ExposureFailoverStatus{Role: string(dr.RolePrimary), SiteID: siteSelf}
		Expect(k8sClient.Status().Update(ctx, current)).To(Succeed())
		seedLease("fo-return-grp", sitePeer, "peer-tunnel", testNow.Add(5*time.Minute))

		reconcileExposure(ctx, exposureRec, current)

		refreshed := fetchExposure(ctx, exposure.Name)
		Expect(refreshed.Status.Failover.Role).To(Equal(string(dr.RoleStandby)))
		Expect(refreshed.Status.Failover.LeaseOwner).To(Equal(sitePeer))
		// Performed no Cloudflare writes for the shared hostname.
		records, err := fakeCF.DNSRecords().List(ctx, zoneID, "fo-return.example.com", "CNAME")
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(BeEmpty())
		// Peer's lease is untouched.
		lease, found := readLease("fo-return-grp")
		Expect(found).To(BeTrue())
		Expect(lease.Site).To(Equal(sitePeer))
		expectRecordedEvent(exposureRecorder, EventDemotedToStandby)
	})

	It("TestFailoverForcePromoteAnnotation", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "fo-force", "fo-force")
		cfTunnel := fetchTunnel(ctx, tunnel.Name)
		exposure := createFailoverExposure(ctx, "fo-force-app", tunnel.Name, "fo-force.example.com", "fo-force-grp")
		// Peer holds a LIVE lease; only force-promote can override it.
		seedLease("fo-force-grp", sitePeer, "peer-tunnel", testNow.Add(5*time.Minute))
		current := fetchExposure(ctx, exposure.Name)
		current.Annotations = map[string]string{annotationForcePromote: "true"}
		Expect(k8sClient.Update(ctx, current)).To(Succeed())

		reconcileExposure(ctx, exposureRec, current)

		refreshed := fetchExposure(ctx, exposure.Name)
		Expect(refreshed.Status.Failover.Role).To(Equal(string(dr.RolePrimary)))
		// Annotation cleared after successful acquire.
		Expect(refreshed.Annotations).NotTo(HaveKey(annotationForcePromote))
		lease, found := readLease("fo-force-grp")
		Expect(found).To(BeTrue())
		Expect(lease.Site).To(Equal(siteSelf))
		Expect(lease.Tunnel).To(Equal(cfTunnel.Status.TunnelId))
		expectRecordedEvent(exposureRecorder, EventForcePromoted)
	})
})

func createFailoverExposure(ctx context.Context, name, tunnelName, hostname, group string) *cfztv1alpha1.CloudflareExposure {
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
			Failover: &cfztv1alpha1.FailoverSpec{Group: group, LeaseSeconds: 60},
		},
	}
	Expect(k8sClient.Create(ctx, exposure)).To(Succeed())
	return exposure
}
