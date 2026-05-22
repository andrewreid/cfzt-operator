package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
)

type CredentialsRef = cloudflare.CredentialsRef

type EventRecorder interface {
	Eventf(obj runtime.Object, eventtype, reason, message string, args ...any)
}

type eventRecorderAdapter struct {
	recorder events.EventRecorder
}

func NewEventRecorder(recorder events.EventRecorder) EventRecorder {
	return eventRecorderAdapter{recorder: recorder}
}

func (r eventRecorderAdapter) Eventf(obj runtime.Object, eventtype, reason, message string, args ...any) {
	r.recorder.Eventf(obj, nil, eventtype, reason, reason, message, args...)
}

// Base holds common reconciler dependencies.
type Base struct {
	client.Client
	Scheme              *runtime.Scheme
	Recorder            EventRecorder
	NewCloudflareClient func(ctx context.Context, ref CredentialsRef) (cloudflare.Client, error)
}

func (b *Base) CloudflareClient(ctx context.Context, ref CredentialsRef) (cloudflare.Client, error) {
	if b.NewCloudflareClient != nil {
		return b.NewCloudflareClient(ctx, ref)
	}
	accountID, apiToken, err := cloudflare.Load(ctx, b.Client, ref)
	if err != nil {
		return nil, err
	}
	return cloudflare.New(accountID, apiToken)
}

func (b *Base) SetReady(ctx context.Context, obj client.Object, conditions *[]metav1.Condition, generation int64, ready bool, reason, message string) error {
	return b.setReady(ctx, obj, conditions, generation, ready, reason, message, nil)
}

func (b *Base) setReady(ctx context.Context, obj client.Object, conditions *[]metav1.Condition, generation int64, ready bool, reason, message string, mutate func()) error {
	before := obj.DeepCopyObject().(client.Object)
	if mutate != nil {
		mutate()
	}
	if ready {
		setCondition(conditions, ConditionReady, metav1.ConditionTrue, reason, message, generation)
		setCondition(conditions, ConditionProgressing, metav1.ConditionFalse, reason, message, generation)
	} else {
		setCondition(conditions, ConditionReady, metav1.ConditionFalse, reason, message, generation)
		setCondition(conditions, ConditionProgressing, metav1.ConditionTrue, reason, message, generation)
	}
	if equality.Semantic.DeepEqual(before, obj) {
		return nil
	}
	return b.Status().Update(ctx, obj)
}

func credentialsRefFromTunnel(tunnel *cfztv1alpha1.CloudflareTunnel) CredentialsRef {
	return CredentialsRef{
		Namespace:    tunnel.Spec.Cloudflared.Namespace,
		Name:         tunnel.Spec.CredentialsSecretRef.Name,
		AccountIDKey: tunnel.Spec.CredentialsSecretRef.Keys.AccountId,
		APITokenKey:  tunnel.Spec.CredentialsSecretRef.Keys.ApiToken,
	}
}

func credentialsRefFromAccessPolicy(policy *cfztv1alpha1.CloudflareAccessPolicy) CredentialsRef {
	return CredentialsRef{
		Namespace:    policy.Spec.CredentialsSecretRef.Namespace,
		Name:         policy.Spec.CredentialsSecretRef.Name,
		AccountIDKey: policy.Spec.CredentialsSecretRef.Keys.AccountId,
		APITokenKey:  policy.Spec.CredentialsSecretRef.Keys.ApiToken,
	}
}
