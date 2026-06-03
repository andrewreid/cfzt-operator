package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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

	// seedLeasePeerOverwrite replaces the existing lease record's payload with
	// a peer-owned lease (Update, not Create) so no duplicate is produced —
	// simulating a peer legitimately taking over the single lease record.
	seedLeasePeerOverwrite := func(group, site, tunnelID string, expires time.Time) {
		name := naming.FailoverLeaseTXTName(group, zoneName)
		records, err := fakeCF.DNSRecords().List(ctx, zoneID, name, "TXT")
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(HaveLen(1))
		lease := dr.Lease{Version: dr.LeaseSchemaVersion, Site: site, Tunnel: tunnelID, Renewed: expires.Add(-60 * time.Second), Expires: expires}
		_, err = fakeCF.DNSRecords().Update(ctx, records[0].ID, cloudflare.DNSRecordInput{
			ZoneID:  zoneID,
			Name:    name,
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

	It("TestFailoverGroupConflict", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "fo-grp", "fo-grp")
		a := createFailoverExposure(ctx, "fo-grp-a", tunnel.Name, "fo-grp-a.example.com", "dup-group")
		b := createFailoverExposure(ctx, "fo-grp-b", tunnel.Name, "fo-grp-b.example.com", "dup-group")

		reconcileExposureExpectRequeueAfter30(ctx, exposureRec, a)
		reconcileExposureExpectRequeueAfter30(ctx, exposureRec, b)

		for _, name := range []string{a.Name, b.Name} {
			cur := fetchExposure(ctx, name)
			ready := meta.FindStatusCondition(cur.Status.Conditions, ConditionReady)
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(ReasonFailoverGroupConflict))
		}
		// No lease written for the contended group.
		_, found := readLease("dup-group")
		Expect(found).To(BeFalse())
	})

	It("TestFailoverGroupConflictCrossNamespace", func() {
		// Same group in two different namespaces of one cluster must still
		// conflict — the guard is cluster-wide, not per-namespace. This case
		// would have passed under the previous namespace-scoped bug.
		const otherNamespace = exposureTestNamespace + "-b"
		ensureNamespace(ctx, otherNamespace)
		tunnel := readyTunnel(ctx, tunnelReconciler, "fo-xns", "fo-xns")
		a := createFailoverExposure(ctx, "fo-xns-a", tunnel.Name, "fo-xns-a.example.com", "xns-group")
		b := createFailoverExposureInNamespace(ctx, otherNamespace, "fo-xns-b", tunnel.Name, "fo-xns-b.example.com", "xns-group")

		reconcileExposureExpectRequeueAfter30(ctx, exposureRec, a)
		reconcileExposureExpectRequeueAfter30(ctx, exposureRec, b)

		for _, e := range []*cfztv1alpha1.CloudflareExposure{a, b} {
			cur := &cfztv1alpha1.CloudflareExposure{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: e.Namespace, Name: e.Name}, cur)).To(Succeed())
			ready := meta.FindStatusCondition(cur.Status.Conditions, ConditionReady)
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(ReasonFailoverGroupConflict))
		}
		// No TXT lease written for the contended group.
		_, found := readLease("xns-group")
		Expect(found).To(BeFalse())
	})

	It("TestFailoverRequiresDistinctSiteID", func() {
		exposureRec.SiteID = defaultSiteID
		tunnel := readyTunnel(ctx, tunnelReconciler, "fo-defsite", "fo-defsite")
		exposure := createFailoverExposure(ctx, "fo-defsite-app", tunnel.Name, "fo-defsite.example.com", "fo-defsite-grp")

		reconcileExposureExpectRequeueAfter30(ctx, exposureRec, exposure)

		cur := fetchExposure(ctx, exposure.Name)
		ready := meta.FindStatusCondition(cur.Status.Conditions, ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(ReasonFailoverRequiresDistinctSiteID))
		_, found := readLease("fo-defsite-grp")
		Expect(found).To(BeFalse())
	})

	It("TestFailoverDuplicateLeaseResolves", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "fo-dup", "fo-dup")
		cfTunnel := fetchTunnel(ctx, tunnel.Name)
		exposure := createFailoverExposure(ctx, "fo-dup-app", tunnel.Name, "fo-dup.example.com", "fo-dup-grp")
		// Two group-owned lease records exist (a create race). site-self sorts
		// lexicographically below "zpeer", so this site is the deterministic
		// winner and keeps its record.
		seedLease("fo-dup-grp", siteSelf, cfTunnel.Status.TunnelId, testNow.Add(5*time.Minute))
		seedLease("fo-dup-grp", "zpeer", "peer-tunnel", testNow.Add(5*time.Minute))

		// First reconcile resolves duplicates (deletes the loser) and requeues.
		reconcileExposure(ctx, exposureRec, exposure)
		records, err := fakeCF.DNSRecords().List(ctx, zoneID, naming.FailoverLeaseTXTName("fo-dup-grp", zoneName), "TXT")
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(HaveLen(1))
		lease, perr := dr.ParseLease(records[0].Content)
		Expect(perr).NotTo(HaveOccurred())
		Expect(lease.Site).To(Equal(siteSelf))

		// Next reconcile acts on the converged single record -> Primary.
		reconcileExposure(ctx, exposureRec, fetchExposure(ctx, exposure.Name))
		Expect(fetchExposure(ctx, exposure.Name).Status.Failover.Role).To(Equal(string(dr.RolePrimary)))
	})

	It("TestFailoverDeleteRequiresLiveOwnership", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "fo-del", "fo-del")
		exposure := createFailoverExposure(ctx, "fo-del-app", tunnel.Name, "fo-del.example.com", "fo-del-grp")
		seedLease("fo-del-grp", sitePeer, "peer-tunnel", testNow.Add(-1*time.Second))

		// This site auto-promotes and writes the shared CNAME.
		reconcileExposure(ctx, exposureRec, exposure)
		Expect(fetchExposure(ctx, exposure.Name).Status.Failover.Role).To(Equal(string(dr.RolePrimary)))
		cnames, err := fakeCF.DNSRecords().List(ctx, zoneID, "fo-del.example.com", "CNAME")
		Expect(err).NotTo(HaveOccurred())
		Expect(cnames).To(HaveLen(1))

		// A peer then steals the live lease (single record, future expiry). This
		// site's status still says Primary (stale).
		seedLeasePeerOverwrite("fo-del-grp", sitePeer, "peer-tunnel", testNow.Add(5*time.Minute))

		// Delete this CR: it must NOT remove the shared CNAME the peer now owns.
		current := fetchExposure(ctx, exposure.Name)
		Expect(k8sClient.Delete(ctx, current)).To(Succeed())
		reconcileExposure(ctx, exposureRec, current)

		cnamesAfter, err := fakeCF.DNSRecords().List(ctx, zoneID, "fo-del.example.com", "CNAME")
		Expect(err).NotTo(HaveOccurred())
		Expect(cnamesAfter).To(HaveLen(1), "stale-primary delete must not tear down the peer's shared CNAME")
		// Peer's lease is untouched.
		lease, found := readLease("fo-del-grp")
		Expect(found).To(BeTrue())
		Expect(lease.Site).To(Equal(sitePeer))
		// Finalizer removed (CR can be garbage-collected).
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: exposure.Name}, &cfztv1alpha1.CloudflareExposure{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("TestFailoverDeleteWithDuplicateLeasesFailsSafe", func() {
		tunnel := readyTunnel(ctx, tunnelReconciler, "fo-deldup", "fo-deldup")
		exposure := createFailoverExposure(ctx, "fo-deldup-app", tunnel.Name, "fo-deldup.example.com", "fo-deldup-grp")
		seedLease("fo-deldup-grp", sitePeer, "peer-tunnel", testNow.Add(-1*time.Second))

		// This site promotes and writes the shared CNAME (lease becomes its own).
		reconcileExposure(ctx, exposureRec, exposure)
		Expect(fetchExposure(ctx, exposure.Name).Status.Failover.Role).To(Equal(string(dr.RolePrimary)))

		// A create race leaves a second, peer-owned lease record alongside this
		// site's — an ambiguous duplicate set with no proven single owner.
		seedLease("fo-deldup-grp", sitePeer, "peer-tunnel", testNow.Add(5*time.Minute))
		leaseName := naming.FailoverLeaseTXTName("fo-deldup-grp", zoneName)
		dupRecords, err := fakeCF.DNSRecords().List(ctx, zoneID, leaseName, "TXT")
		Expect(err).NotTo(HaveOccurred())
		Expect(dupRecords).To(HaveLen(2))

		// Delete this CR. status.failover.role is stale Primary, but the live
		// lease set is ambiguous, so the finalizer must fail safe: leave the
		// shared CNAME alone and remove only this site's own lease record.
		current := fetchExposure(ctx, exposure.Name)
		Expect(k8sClient.Delete(ctx, current)).To(Succeed())
		reconcileExposure(ctx, exposureRec, current)

		cnamesAfter, err := fakeCF.DNSRecords().List(ctx, zoneID, "fo-deldup.example.com", "CNAME")
		Expect(err).NotTo(HaveOccurred())
		Expect(cnamesAfter).To(HaveLen(1), "ambiguous-duplicate delete must not tear down the shared CNAME")
		// Only the peer's lease record remains (this site's own duplicate is gone).
		remaining, err := fakeCF.DNSRecords().List(ctx, zoneID, leaseName, "TXT")
		Expect(err).NotTo(HaveOccurred())
		Expect(remaining).To(HaveLen(1))
		lease, perr := dr.ParseLease(remaining[0].Content)
		Expect(perr).NotTo(HaveOccurred())
		Expect(lease.Site).To(Equal(sitePeer))
		// Finalizer removed despite the fail-safe shared-resource skip.
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: exposure.Name}, &cfztv1alpha1.CloudflareExposure{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

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

	It("TestFailoverPrimaryAdoptsPeerGroupCNAME", func() {
		// A peer (other site) previously created the shared, group-owned CNAME.
		// When this site promotes it must UPDATE that record to its own tunnel —
		// the mutation guard accepts the group ID from the other site — not
		// create a duplicate or trip HostnameConflict.
		tunnel := readyTunnel(ctx, tunnelReconciler, "fo-adopt", "fo-adopt")
		cfTunnel := fetchTunnel(ctx, tunnel.Name)
		exposure := createFailoverExposure(ctx, "fo-adopt-app", tunnel.Name, "fo-adopt.example.com", "fo-adopt-grp")
		_, err := fakeCF.DNSRecords().Create(ctx, cloudflare.DNSRecordInput{
			ZoneID:  zoneID,
			Name:    "fo-adopt.example.com",
			Type:    "CNAME",
			Content: "peer-tunnel.cfargotunnel.com",
			Proxied: true,
			Comment: ownership.FromFailoverGroup("fo-adopt-grp").Comment(),
		})
		Expect(err).NotTo(HaveOccurred())
		seedLease("fo-adopt-grp", sitePeer, "peer-tunnel", testNow.Add(-1*time.Second))

		reconcileExposure(ctx, exposureRec, exposure)

		Expect(fetchExposure(ctx, exposure.Name).Status.Failover.Role).To(Equal(string(dr.RolePrimary)))
		records, err := fakeCF.DNSRecords().List(ctx, zoneID, "fo-adopt.example.com", "CNAME")
		Expect(err).NotTo(HaveOccurred())
		Expect(records).To(HaveLen(1), "must adopt the peer's group-owned CNAME, not duplicate it")
		Expect(records[0].Content).To(Equal(cfTunnel.Status.TunnelId + ".cfargotunnel.com"))
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
		current.Annotations = map[string]string{annotationForcePromote: "token-1"}
		Expect(k8sClient.Update(ctx, current)).To(Succeed())

		reconcileExposure(ctx, exposureRec, current)

		refreshed := fetchExposure(ctx, exposure.Name)
		Expect(refreshed.Status.Failover.Role).To(Equal(string(dr.RolePrimary)))
		// Token recorded in status; annotation is NOT mutated (GitOps-safe).
		Expect(refreshed.Annotations).To(HaveKeyWithValue(annotationForcePromote, "token-1"))
		Expect(refreshed.Status.Failover.LastForcePromoteToken).To(Equal("token-1"))
		lease, found := readLease("fo-force-grp")
		Expect(found).To(BeTrue())
		Expect(lease.Site).To(Equal(siteSelf))
		Expect(lease.Tunnel).To(Equal(cfTunnel.Status.TunnelId))
		expectRecordedEvent(exposureRecorder, EventForcePromoted)

		// Replay guard: a peer re-takes the lease, the same token is re-applied
		// (as GitOps would), and the controller must NOT force-promote again.
		seedLeasePeerOverwrite("fo-force-grp", sitePeer, "peer-tunnel", testNow.Add(5*time.Minute))
		reconcileExposure(ctx, exposureRec, fetchExposure(ctx, exposure.Name))
		Expect(fetchExposure(ctx, exposure.Name).Status.Failover.Role).To(Equal(string(dr.RoleStandby)))
		leaseAfter, _ := readLease("fo-force-grp")
		Expect(leaseAfter.Site).To(Equal(sitePeer))
	})
})

func createFailoverExposure(ctx context.Context, name, tunnelName, hostname, group string) *cfztv1alpha1.CloudflareExposure {
	return createFailoverExposureInNamespace(ctx, exposureTestNamespace, name, tunnelName, hostname, group)
}

func createFailoverExposureInNamespace(ctx context.Context, namespace, name, tunnelName, hostname, group string) *cfztv1alpha1.CloudflareExposure {
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
			Failover: &cfztv1alpha1.FailoverSpec{Group: group, LeaseSeconds: 60},
		},
	}
	Expect(k8sClient.Create(ctx, exposure)).To(Succeed())
	return exposure
}
