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
	ReasonReconciled         = "Reconciled"

	EventCreatedTunnel = "CreatedTunnel"
	EventTokenRotated  = "TokenRotated"
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
