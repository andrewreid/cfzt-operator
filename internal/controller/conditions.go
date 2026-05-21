package controller

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ConditionReady       = "Ready"
	ConditionProgressing = "Progressing"

	ReasonCredentialsMissing = "CredentialsMissing"
	ReasonTunnelCreating     = "TunnelCreating"
	ReasonForeignTunnel      = "ForeignTunnel"
	ReasonTokenFetchFailed   = "TokenFetchFailed"
	ReasonWorkloadNotReady   = "WorkloadNotReady"
	ReasonOriginInvalid      = "OriginInvalid"
	ReasonHostnameConflict   = "HostnameConflict"
	ReasonForeignResource    = "ForeignResource"
	ReasonAccessAppPending   = "AccessAppPending"
	ReasonPolicyNotFound     = "PolicyNotFound"
	ReasonPolicyNotReady     = "PolicyNotReady"
	ReasonDNSWriteFailed     = "DNSWriteFailed"
	ReasonBlockedByExposures = "BlockedByExposures"
	ReasonForeignPolicy      = "ForeignPolicy"
	ReasonTunnelNotReady     = "TunnelNotReady"
	ReasonNetworkInvalid     = "NetworkInvalid"
	ReasonForeignRoute       = "ForeignRoute"
	ReasonRouteWriteFailed   = "RouteWriteFailed"
	ReasonBlockedByRoutes    = "BlockedByRoutes"
	ReasonUnsupportedDrift   = "UnsupportedDrift"
	ReasonReconciled         = "Reconciled"

	EventCreatedTunnel       = "CreatedTunnel"
	EventCreatedAccessApp    = "CreatedAccessApp"
	EventCreatedAccessPolicy = "CreatedAccessPolicy"
	EventUpdatedAccessPolicy = "UpdatedAccessPolicy"
	EventHostnameConflict    = "HostnameConflict"
	EventForeignTunnel       = "ForeignTunnel"
	EventReconcileFailed     = "ReconcileFailed"
	EventTokenRotated        = "TokenRotated"
	EventBlockedByExposures  = "BlockedByExposures"
	EventCreatedRoute        = "CreatedRoute"
	EventDeletedRoute        = "DeletedRoute"
	EventForeignRoute        = "ForeignRoute"
	EventBlockedByRoutes     = "BlockedByRoutes"
)

func setCondition(conditions *[]metav1.Condition, conditionType string, status metav1.ConditionStatus, reason, message string, observedGeneration int64) {
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: observedGeneration,
	})
}
