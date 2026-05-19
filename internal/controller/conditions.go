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
	ReasonDNSWriteFailed     = "DNSWriteFailed"
	ReasonBlockedByExposures = "BlockedByExposures"
	ReasonReconciled         = "Reconciled"

	EventCreatedTunnel      = "CreatedTunnel"
	EventCreatedAccessApp   = "CreatedAccessApp"
	EventHostnameConflict   = "HostnameConflict"
	EventForeignTunnel      = "ForeignTunnel"
	EventReconcileFailed    = "ReconcileFailed"
	EventTokenRotated       = "TokenRotated"
	EventBlockedByExposures = "BlockedByExposures"
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
