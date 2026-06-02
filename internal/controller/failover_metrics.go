package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// D26 failover observability. Metrics are labelled by the Exposure's
// namespace/name and its failover group so an operator can chart which site
// holds the lease over time and how often promotions / renewals happen.
var (
	// failoverRoleGauge encodes this site's current role per Exposure:
	// 0=Unknown, 1=Standby, 2=Primary. A gauge (not a per-role series) keeps
	// the cardinality bounded and makes "is exactly one site Primary" a
	// trivial sum across clusters scraping into the same store.
	failoverRoleGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cfzt_failover_role",
		Help: "Current DR failover role for a CloudflareExposure (0=Unknown, 1=Standby, 2=Primary).",
	}, []string{"namespace", "name", "group"})

	failoverLeaseRenewTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cfzt_failover_lease_renew_total",
		Help: "Total successful DR failover lease renewals by this site.",
	}, []string{"namespace", "name", "group"})

	failoverPromotionTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cfzt_failover_promotion_total",
		Help: "Total DR failover promotions to Primary by this site.",
	}, []string{"namespace", "name", "group"})
)

func init() {
	ctrlmetrics.Registry.MustRegister(failoverRoleGauge, failoverLeaseRenewTotal, failoverPromotionTotal)
}

func failoverRoleValue(role string) float64 {
	switch role {
	case "Primary":
		return 2
	case "Standby":
		return 1
	default:
		return 0
	}
}
