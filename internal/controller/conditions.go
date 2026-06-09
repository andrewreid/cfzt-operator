package controller

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ConditionReady       = "Ready"
	ConditionProgressing = "Progressing"

	ReasonCredentialsMissing            = "CredentialsMissing"
	ReasonTunnelCreating                = "TunnelCreating"
	ReasonTunnelNameInvalid             = "TunnelNameInvalid"
	ReasonForeignTunnel                 = "ForeignTunnel"
	ReasonTokenFetchFailed              = "TokenFetchFailed"
	ReasonWorkloadNotReady              = "WorkloadNotReady"
	ReasonOriginInvalid                 = "OriginInvalid"
	ReasonHostnameConflict              = "HostnameConflict"
	ReasonForeignResource               = "ForeignResource"
	ReasonAccessAppPending              = "AccessAppPending"
	ReasonAccessApplicationsUnsupported = "AccessApplicationsUnsupported"
	ReasonPolicyNotFound                = "PolicyNotFound"
	ReasonPolicyNotReady                = "PolicyNotReady"
	ReasonDNSWriteFailed                = "DNSWriteFailed"
	ReasonBlockedByExposures            = "BlockedByExposures"
	ReasonForeignPolicy                 = "ForeignPolicy"
	ReasonTunnelNotReady                = "TunnelNotReady"
	ReasonNetworkInvalid                = "NetworkInvalid"
	ReasonForeignRoute                  = "ForeignRoute"
	ReasonRouteWriteFailed              = "RouteWriteFailed"
	ReasonBlockedByRoutes               = "BlockedByRoutes"
	ReasonUnsupportedDrift              = "UnsupportedDrift"
	ReasonReconciled                    = "Reconciled"

	// D26 failover reasons.
	ReasonStandby                        = "Standby"
	ReasonAwaitingPromotion              = "AwaitingPromotion"
	ReasonLeaseConflict                  = "LeaseConflict"
	ReasonFailoverRequiresManagedDNS     = "FailoverRequiresManagedDNS"
	ReasonFailoverRequiresDistinctSiteID = "FailoverRequiresDistinctSiteID"
	ReasonFailoverGroupConflict          = "FailoverGroupConflict"

	EventCreatedTunnel         = "CreatedTunnel"
	EventRecoveredTunnel       = "RecoveredTunnel"
	EventRenamedTunnel         = "RenamedTunnel"
	EventUpdatedTunnelConfig   = "UpdatedTunnelConfig"
	EventDeletedTunnel         = "DeletedTunnel"
	EventCreatedAccessApp      = "CreatedAccessApp"
	EventUpdatedAccessApp      = "UpdatedAccessApp"
	EventDeletedAccessApp      = "DeletedAccessApp"
	EventCreatedDNSRecord      = "CreatedDNSRecord"
	EventUpdatedDNSRecord      = "UpdatedDNSRecord"
	EventDeletedDNSRecord      = "DeletedDNSRecord"
	EventCreatedAccessPolicy   = "CreatedAccessPolicy"
	EventRecoveredAccessPolicy = "RecoveredAccessPolicy"
	EventUpdatedAccessPolicy   = "UpdatedAccessPolicy"
	EventDeletedAccessPolicy   = "DeletedAccessPolicy"
	EventHostnameConflict      = "HostnameConflict"
	EventTokenRotated          = "TokenRotated"
	EventBlockedByExposures    = "BlockedByExposures"
	EventCreatedRoute          = "CreatedRoute"
	EventUpdatedRoute          = "UpdatedRoute"
	EventDeletedRoute          = "DeletedRoute"
	EventForeignRoute          = "ForeignRoute"
	EventBlockedByRoutes       = "BlockedByRoutes"

	// D26 failover events.
	EventPromotedToPrimary  = "PromotedToPrimary"
	EventDemotedToStandby   = "DemotedToStandby"
	EventLeaseAcquired      = "LeaseAcquired"
	EventLeaseRenewed       = "LeaseRenewed"
	EventLeaseLost          = "LeaseLost"
	EventLeaseConflict      = "LeaseConflict"
	EventSplitBrainDetected = "SplitBrainDetected"
	EventForcePromoted      = "ForcePromoted"
	// EventAwaitingManualPromotion fires when a Manual-policy Standby declines
	// to auto-promote (expired peer lease, or absent day-1 lease). Warning so
	// monitoring can alert that a deliberate force-promote is required.
	EventAwaitingManualPromotion = "AwaitingManualPromotion"
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
