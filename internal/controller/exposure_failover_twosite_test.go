package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
	"github.com/andrewreid/cfzt-operator/internal/dr"
	"github.com/andrewreid/cfzt-operator/internal/naming"
)

// twoSiteRun makes cluster-scoped tunnels and namespaced objects unique per
// spec — they outlive a single It in the shared envtest apiserver.
var twoSiteRun int

// These specs model two clusters in one test process: two CloudflareExposures
// in distinct namespaces, each with its own CloudflareTunnel (own tunnel ID)
// but a shared spec.failover.group, reconciled by two reconcilers with
// distinct --site-id values, all driving one shared fake CloudflareClient
// (CAS-by-record_id over shared CF state). This is the faithful D26 topology:
// distinct clusters, one Cloudflare account, one lease.
var _ = Describe("CloudflareExposure failover across two sites", func() {
	const (
		siteA    = "site-a"
		siteB    = "site-b"
		zoneName = "example.com"
		zoneID   = "zone-example"
		group    = "shared-grp"
		hostname = "shared.example.com"
	)

	var (
		ctx          context.Context
		fakeCF       *cloudflare.FakeClient
		tunnelRec    *CloudflareTunnelReconciler
		recA, recB   *CloudflareExposureReconciler
		recorderA    *record.FakeRecorder
		recorderB    *record.FakeRecorder
		nowVal       time.Time
		tunnelAID    string
		tunnelBID    string
		nsA, nsB     string
		tunA, tunB   string
		expoA, expoB *cfztv1alpha1.CloudflareExposure
	)

	BeforeEach(func() {
		ctx = context.Background()
		twoSiteRun++
		nsA = fmt.Sprintf("site-a-ns-%d", twoSiteRun)
		nsB = fmt.Sprintf("site-b-ns-%d", twoSiteRun)
		tunA = fmt.Sprintf("tunnel-a-%d", twoSiteRun)
		tunB = fmt.Sprintf("tunnel-b-%d", twoSiteRun)
		ensureNamespace(ctx, "cfzt-system")
		ensureNamespace(ctx, nsA)
		ensureNamespace(ctx, nsB)
		fakeCF = cloudflare.NewFake()
		fakeCF.AddZone(zoneID, zoneName)
		nowVal = time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
		clock := func() time.Time { return nowVal }

		tunnelRec = &CloudflareTunnelReconciler{Base: Base{
			Client:              indexedClient,
			Scheme:              indexedClient.Scheme(),
			NewCloudflareClient: newFakeCloudflareClient(indexedClient, fakeCF),
			Recorder:            record.NewFakeRecorder(1024),
		}}
		recorderA = record.NewFakeRecorder(1024)
		recorderB = record.NewFakeRecorder(1024)
		recA = &CloudflareExposureReconciler{Base: Base{
			Client:              indexedClient,
			Scheme:              indexedClient.Scheme(),
			NewCloudflareClient: newFakeCloudflareClient(indexedClient, fakeCF),
			Recorder:            recorderA,
		}, SiteID: siteA, Now: clock}
		recB = &CloudflareExposureReconciler{Base: Base{
			Client:              indexedClient,
			Scheme:              indexedClient.Scheme(),
			NewCloudflareClient: newFakeCloudflareClient(indexedClient, fakeCF),
			Recorder:            recorderB,
		}, SiteID: siteB, Now: clock}

		tunnelA := readyTunnel(ctx, tunnelRec, tunA, tunA)
		tunnelB := readyTunnel(ctx, tunnelRec, tunB, tunB)
		tunnelAID = fetchTunnel(ctx, tunnelA.Name).Status.TunnelId
		tunnelBID = fetchTunnel(ctx, tunnelB.Name).Status.TunnelId
		Expect(tunnelAID).NotTo(Equal(tunnelBID))
		expoA = createFailoverExposureNS(ctx, nsA, "shared-app", tunnelA.Name, hostname, group)
		expoB = createFailoverExposureNS(ctx, nsB, "shared-app", tunnelB.Name, hostname, group)
	})

	cname := func() (string, bool) {
		records, err := fakeCF.DNSRecords().List(ctx, zoneID, hostname, "CNAME")
		Expect(err).NotTo(HaveOccurred())
		if len(records) == 0 {
			return "", false
		}
		Expect(records).To(HaveLen(1))
		return records[0].Content, true
	}

	leaseSite := func() (string, bool) {
		records, err := fakeCF.DNSRecords().List(ctx, zoneID, naming.FailoverLeaseTXTName(group, zoneName), "TXT")
		Expect(err).NotTo(HaveOccurred())
		if len(records) == 0 {
			return "", false
		}
		lease, perr := dr.ParseLease(records[0].Content)
		Expect(perr).NotTo(HaveOccurred())
		return lease.Site, true
	}

	It("TestFailoverTwoSiteSinglePrimaryAndHandoff", func() {
		// First reconcile of each site: A wins the lease, B stands by.
		reconcileExposure(ctx, recA, expoA)
		reconcileExposure(ctx, recB, expoB)

		Expect(fetchExposureNS(ctx, nsA).Status.Failover.Role).To(Equal(string(dr.RolePrimary)))
		bStatus := fetchExposureNS(ctx, nsB).Status.Failover
		Expect(bStatus.Role).To(Equal(string(dr.RoleStandby)))
		Expect(bStatus.LeaseOwner).To(Equal(siteA))
		site, ok := leaseSite()
		Expect(ok).To(BeTrue())
		Expect(site).To(Equal(siteA))
		content, ok := cname()
		Expect(ok).To(BeTrue())
		Expect(content).To(Equal(tunnelAID + ".cfargotunnel.com"))

		// Site A stops renewing; its lease lapses.
		nowVal = nowVal.Add(90 * time.Second)
		reconcileExposure(ctx, recB, expoB)

		Expect(fetchExposureNS(ctx, nsB).Status.Failover.Role).To(Equal(string(dr.RolePrimary)))
		site, _ = leaseSite()
		Expect(site).To(Equal(siteB))
		// The mutation guard accepted site B updating the group-owned CNAME
		// that site A created — it now points at tunnel B.
		content, _ = cname()
		Expect(content).To(Equal(tunnelBID + ".cfargotunnel.com"))

		// Site A returns and reconciles; it must self-demote without writes.
		reconcileExposure(ctx, recA, expoA)
		aStatus := fetchExposureNS(ctx, nsA).Status.Failover
		Expect(aStatus.Role).To(Equal(string(dr.RoleStandby)))
		Expect(aStatus.LeaseOwner).To(Equal(siteB))
		content, _ = cname()
		Expect(content).To(Equal(tunnelBID + ".cfargotunnel.com"))
		// Still exactly one lease, owned by B.
		site, _ = leaseSite()
		Expect(site).To(Equal(siteB))
		expectRecordedEvent(recorderA, EventDemotedToStandby)
	})

	It("TestFailoverTwoSiteForcePromote", func() {
		reconcileExposure(ctx, recA, expoA)
		Expect(fetchExposureNS(ctx, nsA).Status.Failover.Role).To(Equal(string(dr.RolePrimary)))

		// Site B force-promotes against site A's live lease.
		current := fetchExposureNS(ctx, nsB)
		current.Annotations = map[string]string{annotationForcePromote: "true"}
		Expect(k8sClient.Update(ctx, current)).To(Succeed())
		reconcileExposure(ctx, recB, current)

		refreshedB := fetchExposureNS(ctx, nsB)
		Expect(refreshedB.Status.Failover.Role).To(Equal(string(dr.RolePrimary)))
		Expect(refreshedB.Annotations).NotTo(HaveKey(annotationForcePromote))
		site, _ := leaseSite()
		Expect(site).To(Equal(siteB))
		content, _ := cname()
		Expect(content).To(Equal(tunnelBID + ".cfargotunnel.com"))
		expectRecordedEvent(recorderB, EventForcePromoted)

		// Site A, reconciling next, observes the steal and demotes.
		reconcileExposure(ctx, recA, expoA)
		Expect(fetchExposureNS(ctx, nsA).Status.Failover.Role).To(Equal(string(dr.RoleStandby)))
	})
})

// fetchExposureNS fetches the shared two-site Exposure ("shared-app") from the
// given namespace.
func fetchExposureNS(ctx context.Context, namespace string) *cfztv1alpha1.CloudflareExposure {
	exposure := &cfztv1alpha1.CloudflareExposure{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "shared-app"}, exposure)).To(Succeed())
	return exposure
}

func createFailoverExposureNS(ctx context.Context, namespace, name, tunnelName, hostname, group string) *cfztv1alpha1.CloudflareExposure {
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
