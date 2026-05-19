/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
	"github.com/andrewreid/cfzt-operator/internal/naming"
	"github.com/andrewreid/cfzt-operator/internal/workload"
)

// CloudflareClientFactory builds a Cloudflare client from credentials.
type CloudflareClientFactory func(accountID, apiToken string) (cloudflare.Client, error)

// CloudflareTunnelReconciler reconciles a CloudflareTunnel object
type CloudflareTunnelReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	CloudflareClientFactory CloudflareClientFactory
	Recorder                record.EventRecorder
}

// +kubebuilder:rbac:groups=cfzt.reid.ee,resources=cloudflaretunnels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cfzt.reid.ee,resources=cloudflaretunnels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cfzt.reid.ee,resources=cloudflaretunnels/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *CloudflareTunnelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var tunnel cfztv1alpha1.CloudflareTunnel
	if err := r.Get(ctx, req.NamespacedName, &tunnel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !tunnel.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.reconcileDelete(ctx, &tunnel)
	}

	if controllerutil.AddFinalizer(&tunnel, naming.Finalizer) {
		if err := r.Update(ctx, &tunnel); err != nil {
			return ctrl.Result{}, err
		}
	}

	cfClient, err := r.cloudflareClient(ctx, &tunnel)
	if err != nil {
		return ctrl.Result{}, r.setTunnelStatus(ctx, &tunnel, tunnel.Status.TunnelId, naming.TokenSecretName(tunnel.Name), false, ReasonCredentialsMissing, err.Error())
	}

	cfTunnel, created, err := r.reconcileCloudflareTunnel(ctx, &tunnel, cfClient)
	if err != nil {
		if err == errForeignTunnel {
			return ctrl.Result{}, r.setTunnelStatus(ctx, &tunnel, "", naming.TokenSecretName(tunnel.Name), false, ReasonForeignTunnel, "Cloudflare tunnel name already exists without local ownership record")
		}
		return ctrl.Result{}, err
	}
	if tunnel.Status.TunnelId != cfTunnel.ID {
		if err := r.setTunnelStatus(ctx, &tunnel, cfTunnel.ID, naming.TokenSecretName(tunnel.Name), false, ReasonTunnelCreating, "Cloudflare tunnel identity reconciled"); err != nil {
			return ctrl.Result{}, err
		}
		tunnel.Status.TunnelId = cfTunnel.ID
	}
	if created {
		r.event(&tunnel, corev1.EventTypeNormal, EventCreatedTunnel, "Created Cloudflare tunnel %s", cfTunnel.ID)
	}

	token, err := cfClient.Tunnels().Token(ctx, cfTunnel.ID)
	if err != nil {
		return ctrl.Result{}, r.setTunnelStatus(ctx, &tunnel, cfTunnel.ID, naming.TokenSecretName(tunnel.Name), false, ReasonTokenFetchFailed, err.Error())
	}

	if err := r.upsertTokenSecret(ctx, &tunnel, token); err != nil {
		return ctrl.Result{}, err
	}
	rotated, err := r.upsertDaemonSet(ctx, &tunnel, token)
	if err != nil {
		return ctrl.Result{}, err
	}
	if rotated {
		r.event(&tunnel, corev1.EventTypeNormal, EventTokenRotated, "Token checksum changed for tunnel %s", tunnel.Name)
	}

	ready, err := r.daemonSetReady(ctx, &tunnel)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		return ctrl.Result{}, r.setTunnelStatus(ctx, &tunnel, cfTunnel.ID, naming.TokenSecretName(tunnel.Name), false, ReasonWorkloadNotReady, "cloudflared DaemonSet has no ready pods")
	}

	log.V(1).Info("CloudflareTunnel reconciled", "tunnelID", cfTunnel.ID)
	return ctrl.Result{}, r.setTunnelStatus(ctx, &tunnel, cfTunnel.ID, naming.TokenSecretName(tunnel.Name), true, ReasonReconciled, "Tunnel and cloudflared workload reconciled")
}

func (r *CloudflareTunnelReconciler) reconcileDelete(ctx context.Context, tunnel *cfztv1alpha1.CloudflareTunnel) error {
	if !controllerutil.ContainsFinalizer(tunnel, naming.Finalizer) {
		return nil
	}
	if tunnel.Status.TunnelId != "" {
		cfClient, err := r.cloudflareClient(ctx, tunnel)
		if err != nil {
			setCondition(&tunnel.Status.Conditions, ConditionReady, metav1.ConditionFalse, ReasonCredentialsMissing, err.Error(), tunnel.Generation)
			_ = r.Status().Update(ctx, tunnel)
			return nil
		}
		err = cfClient.Tunnels().Delete(ctx, tunnel.Status.TunnelId)
		if err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
			return err
		}
	}
	if err := r.deleteNamespaced(ctx, &corev1.Secret{}, tunnel.Spec.Cloudflared.Namespace, naming.TokenSecretName(tunnel.Name)); err != nil {
		return err
	}
	if err := r.deleteNamespaced(ctx, &appsv1.DaemonSet{}, tunnel.Spec.Cloudflared.Namespace, naming.DaemonSetName(tunnel.Name)); err != nil {
		return err
	}
	controllerutil.RemoveFinalizer(tunnel, naming.Finalizer)
	return r.Update(ctx, tunnel)
}

func (r *CloudflareTunnelReconciler) deleteNamespaced(ctx context.Context, obj client.Object, namespace, name string) error {
	obj.SetNamespace(namespace)
	obj.SetName(name)
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *CloudflareTunnelReconciler) cloudflareClient(ctx context.Context, tunnel *cfztv1alpha1.CloudflareTunnel) (cloudflare.Client, error) {
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: tunnel.Spec.Cloudflared.Namespace, Name: tunnel.Spec.CredentialsSecretRef.Name}
	if err := r.Get(ctx, key, &secret); err != nil {
		return nil, fmt.Errorf("credentials Secret %s/%s not readable: %w", key.Namespace, key.Name, err)
	}
	accountID := string(secret.Data[tunnel.Spec.CredentialsSecretRef.Keys.AccountId])
	apiToken := string(secret.Data[tunnel.Spec.CredentialsSecretRef.Keys.ApiToken])
	if accountID == "" {
		return nil, fmt.Errorf("credentials Secret %s/%s missing key %q", key.Namespace, key.Name, tunnel.Spec.CredentialsSecretRef.Keys.AccountId)
	}
	if apiToken == "" {
		return nil, fmt.Errorf("credentials Secret %s/%s missing key %q", key.Namespace, key.Name, tunnel.Spec.CredentialsSecretRef.Keys.ApiToken)
	}
	factory := r.CloudflareClientFactory
	if factory == nil {
		factory = func(accountID, apiToken string) (cloudflare.Client, error) {
			return cloudflare.New(accountID, apiToken)
		}
	}
	return factory(accountID, apiToken)
}

var errForeignTunnel = fmt.Errorf("foreign tunnel")

func (r *CloudflareTunnelReconciler) reconcileCloudflareTunnel(ctx context.Context, tunnel *cfztv1alpha1.CloudflareTunnel, cfClient cloudflare.Client) (*cloudflare.Tunnel, bool, error) {
	if tunnel.Status.TunnelId != "" {
		cfTunnel, err := cfClient.Tunnels().Get(ctx, tunnel.Status.TunnelId)
		if err != nil {
			return nil, false, err
		}
		if cfTunnel.Name != tunnel.Spec.TunnelName {
			return nil, false, fmt.Errorf("tracked Cloudflare tunnel %s has name %q, want %q", cfTunnel.ID, cfTunnel.Name, tunnel.Spec.TunnelName)
		}
		return cfTunnel, false, nil
	}

	existing, err := cfClient.Tunnels().List(ctx, cloudflare.ListTunnelsFilter{Name: tunnel.Spec.TunnelName})
	if err != nil {
		return nil, false, err
	}
	if len(existing) > 0 {
		return nil, false, errForeignTunnel
	}
	cfTunnel, err := cfClient.Tunnels().Create(ctx, cloudflare.CreateTunnelInput{Name: tunnel.Spec.TunnelName, ConfigSrc: "cloudflare"})
	if err != nil {
		return nil, false, err
	}
	return cfTunnel, true, nil
}

func (r *CloudflareTunnelReconciler) upsertTokenSecret(ctx context.Context, tunnel *cfztv1alpha1.CloudflareTunnel, token string) error {
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: tunnel.Spec.Cloudflared.Namespace, Name: naming.TokenSecretName(tunnel.Name)}
	if err := r.Get(ctx, key, &secret); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		secret = *workload.TokenSecret(tunnel, token)
		if err := controllerutil.SetControllerReference(tunnel, &secret, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, &secret)
	}
	before := secret.DeepCopy()
	workload.ApplyTokenSecret(&secret, tunnel, token)
	if err := controllerutil.SetControllerReference(tunnel, &secret, r.Scheme); err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(before.Labels, secret.Labels) &&
		equality.Semantic.DeepEqual(before.OwnerReferences, secret.OwnerReferences) &&
		equality.Semantic.DeepEqual(before.Type, secret.Type) &&
		equality.Semantic.DeepEqual(before.Data, secret.Data) {
		return nil
	}
	return r.Update(ctx, &secret)
}

func (r *CloudflareTunnelReconciler) upsertDaemonSet(ctx context.Context, tunnel *cfztv1alpha1.CloudflareTunnel, token string) (bool, error) {
	var ds appsv1.DaemonSet
	key := types.NamespacedName{Namespace: tunnel.Spec.Cloudflared.Namespace, Name: naming.DaemonSetName(tunnel.Name)}
	checksum := workload.TokenChecksum(token)
	if err := r.Get(ctx, key, &ds); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, err
		}
		ds = *workload.DaemonSet(tunnel, token)
		if err := controllerutil.SetControllerReference(tunnel, &ds, r.Scheme); err != nil {
			return false, err
		}
		return false, r.Create(ctx, &ds)
	}
	oldChecksum := ds.Spec.Template.Annotations[workload.TokenChecksumAnnotation]
	before := ds.DeepCopy()
	workload.ApplyDaemonSet(&ds, tunnel, token)
	if err := controllerutil.SetControllerReference(tunnel, &ds, r.Scheme); err != nil {
		return false, err
	}
	if equality.Semantic.DeepEqual(before.Labels, ds.Labels) &&
		equality.Semantic.DeepEqual(before.OwnerReferences, ds.OwnerReferences) &&
		equality.Semantic.DeepEqual(before.Spec, ds.Spec) {
		return false, nil
	}
	return oldChecksum != "" && oldChecksum != checksum, r.Update(ctx, &ds)
}

func (r *CloudflareTunnelReconciler) daemonSetReady(ctx context.Context, tunnel *cfztv1alpha1.CloudflareTunnel) (bool, error) {
	var ds appsv1.DaemonSet
	key := types.NamespacedName{Namespace: tunnel.Spec.Cloudflared.Namespace, Name: naming.DaemonSetName(tunnel.Name)}
	if err := r.Get(ctx, key, &ds); err != nil {
		return false, err
	}
	return ds.Status.NumberReady > 0, nil
}

func (r *CloudflareTunnelReconciler) setTunnelStatus(ctx context.Context, tunnel *cfztv1alpha1.CloudflareTunnel, tunnelID, tokenSecretName string, ready bool, reason, message string) error {
	latest := &cfztv1alpha1.CloudflareTunnel{}
	if err := r.Get(ctx, types.NamespacedName{Name: tunnel.Name}, latest); err != nil {
		return err
	}
	before := latest.DeepCopy()
	latest.Status.TunnelId = tunnelID
	latest.Status.TokenSecretRef.Name = tokenSecretName
	if latest.Spec.Dns.Manage {
		latest.Status.DnsMode = "managed"
	} else {
		latest.Status.DnsMode = "external"
	}
	if ready {
		setCondition(&latest.Status.Conditions, ConditionReady, metav1.ConditionTrue, reason, message, latest.Generation)
		setCondition(&latest.Status.Conditions, ConditionProgressing, metav1.ConditionFalse, reason, message, latest.Generation)
	} else {
		setCondition(&latest.Status.Conditions, ConditionReady, metav1.ConditionFalse, reason, message, latest.Generation)
		setCondition(&latest.Status.Conditions, ConditionProgressing, metav1.ConditionTrue, reason, message, latest.Generation)
	}
	if equality.Semantic.DeepEqual(before.Status, latest.Status) {
		return nil
	}
	return r.Status().Update(ctx, latest)
}

func (r *CloudflareTunnelReconciler) event(tunnel *cfztv1alpha1.CloudflareTunnel, eventType, reason, messageFmt string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(tunnel, eventType, reason, messageFmt, args...)
}

// SetupWithManager sets up the controller with the Manager.
func (r *CloudflareTunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// D19: MaxConcurrentReconciles=1 — Tunnel controller is the single writer of
	// the tunnel-config doc per D11. Combined with leader election (D12) this
	// guarantees one writer per tunnel-config doc per cluster.
	return ctrl.NewControllerManagedBy(mgr).
		For(&cfztv1alpha1.CloudflareTunnel{}).
		Owns(&corev1.Secret{}).
		Owns(&appsv1.DaemonSet{}).
		Named("cloudflaretunnel").
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
