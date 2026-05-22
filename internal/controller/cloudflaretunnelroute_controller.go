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
	"net/netip"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
	"github.com/andrewreid/cfzt-operator/internal/naming"
	"github.com/andrewreid/cfzt-operator/internal/ownership"
)

// CloudflareTunnelRouteReconciler reconciles a CloudflareTunnelRoute object.
type CloudflareTunnelRouteReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	CloudflareClientFactory CloudflareClientFactory
	Recorder                record.EventRecorder
}

// +kubebuilder:rbac:groups=cfzt.reid.ee,resources=cloudflaretunnelroutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cfzt.reid.ee,resources=cloudflaretunnelroutes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cfzt.reid.ee,resources=cloudflaretunnelroutes/finalizers,verbs=update
// +kubebuilder:rbac:groups=cfzt.reid.ee,resources=cloudflaretunnels,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *CloudflareTunnelRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var route cfztv1alpha1.CloudflareTunnelRoute
	if err := r.Get(ctx, req.NamespacedName, &route); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !route.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &route)
	}

	if controllerutil.AddFinalizer(&route, naming.Finalizer) {
		if err := r.Update(ctx, &route); err != nil {
			return ctrl.Result{}, err
		}
	}

	tunnel, ok, err := r.referencedTunnel(ctx, &route, false)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ok {
		return ctrl.Result{}, r.setRouteStatus(ctx, &route, route.Status.RouteId, route.Status.VirtualNetworkId, false, ReasonTunnelNotReady, "referenced CloudflareTunnel is not ready")
	}

	network, err := canonicalNetwork(route.Spec.Network)
	if err != nil {
		return ctrl.Result{}, r.setRouteStatus(ctx, &route, route.Status.RouteId, route.Status.VirtualNetworkId, false, ReasonNetworkInvalid, err.Error())
	}

	cfClient, err := r.cloudflareClient(ctx, tunnel)
	if err != nil {
		return ctrl.Result{}, r.setRouteStatus(ctx, &route, route.Status.RouteId, route.Status.VirtualNetworkId, false, ReasonCredentialsMissing, err.Error())
	}

	desired := cloudflare.TunnelRouteInput{
		Network:          network,
		TunnelID:         tunnel.Status.TunnelId,
		VirtualNetworkID: route.Spec.VirtualNetworkId,
		Comment:          routeOwnershipComment(&route),
	}
	cfRoute, created, err := r.reconcileCloudflareRoute(ctx, &route, cfClient, desired)
	if err != nil {
		if errors.Is(err, errForeignRoute) {
			r.Recorder.Eventf(&route, corev1.EventTypeWarning, EventForeignRoute, "Cloudflare tunnel route already exists without local ownership record")
			return ctrl.Result{}, r.setRouteStatus(ctx, &route, route.Status.RouteId, route.Status.VirtualNetworkId, false, ReasonForeignRoute, "Cloudflare tunnel route already exists without local ownership record")
		}
		return ctrl.Result{}, r.setRouteStatus(ctx, &route, route.Status.RouteId, route.Status.VirtualNetworkId, false, ReasonRouteWriteFailed, err.Error())
	}
	if created {
		r.Recorder.Eventf(&route, corev1.EventTypeNormal, EventCreatedRoute, "Created Cloudflare tunnel route %s", cfRoute.ID)
	}

	log.V(1).Info("CloudflareTunnelRoute reconciled", "routeID", cfRoute.ID)
	return ctrl.Result{}, r.setRouteStatus(ctx, &route, cfRoute.ID, cfRoute.VirtualNetworkID, true, ReasonReconciled, "Cloudflare tunnel route reconciled")
}

func (r *CloudflareTunnelRouteReconciler) reconcileDelete(ctx context.Context, route *cfztv1alpha1.CloudflareTunnelRoute) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(route, naming.Finalizer) {
		return ctrl.Result{}, nil
	}
	if route.Status.RouteId != "" {
		tunnel, ok, err := r.referencedTunnel(ctx, route, true)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ok {
			if statusErr := r.setRouteStatus(ctx, route, route.Status.RouteId, route.Status.VirtualNetworkId, false, ReasonTunnelNotReady, "referenced CloudflareTunnel is not ready"); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		cfClient, err := r.cloudflareClient(ctx, tunnel)
		if err != nil {
			if statusErr := r.setRouteStatus(ctx, route, route.Status.RouteId, route.Status.VirtualNetworkId, false, ReasonCredentialsMissing, err.Error()); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		cfRoute, err := cfClient.TunnelRoutes().Get(ctx, route.Status.RouteId)
		if err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
			return ctrl.Result{}, err
		}
		if err == nil && ownership.From(route.UID).MatchesComment(cfRoute.Comment) {
			if err := cfClient.TunnelRoutes().Delete(ctx, route.Status.RouteId); err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
				return ctrl.Result{}, err
			}
			r.Recorder.Eventf(route, corev1.EventTypeNormal, EventDeletedRoute, "Deleted Cloudflare tunnel route %s", route.Status.RouteId)
		}
	}
	controllerutil.RemoveFinalizer(route, naming.Finalizer)
	return ctrl.Result{}, r.Update(ctx, route)
}

func (r *CloudflareTunnelRouteReconciler) referencedTunnel(ctx context.Context, route *cfztv1alpha1.CloudflareTunnelRoute, allowDeleting bool) (*cfztv1alpha1.CloudflareTunnel, bool, error) {
	var tunnel cfztv1alpha1.CloudflareTunnel
	if err := r.Get(ctx, types.NamespacedName{Name: route.Spec.TunnelRef.Name}, &tunnel); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if !allowDeleting && !tunnel.DeletionTimestamp.IsZero() {
		return &tunnel, false, nil
	}
	if tunnel.Status.TunnelId == "" {
		return &tunnel, false, nil
	}
	return &tunnel, true, nil
}

func (r *CloudflareTunnelRouteReconciler) cloudflareClient(ctx context.Context, tunnel *cfztv1alpha1.CloudflareTunnel) (cloudflare.Client, error) {
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: tunnel.Spec.Cloudflared.Namespace, Name: tunnel.Spec.CredentialsSecretRef.Name}
	if err := r.Get(ctx, key, &secret); err != nil {
		return nil, fmt.Errorf("credentials Secret %s/%s not readable: %w", key.Namespace, key.Name, err)
	}
	accountKey := tunnel.Spec.CredentialsSecretRef.Keys.AccountId
	if accountKey == "" {
		accountKey = "accountId"
	}
	tokenKey := tunnel.Spec.CredentialsSecretRef.Keys.ApiToken
	if tokenKey == "" {
		tokenKey = "apiToken"
	}
	accountID := string(secret.Data[accountKey])
	apiToken := string(secret.Data[tokenKey])
	if accountID == "" {
		return nil, fmt.Errorf("credentials Secret %s/%s missing key %q", key.Namespace, key.Name, accountKey)
	}
	if apiToken == "" {
		return nil, fmt.Errorf("credentials Secret %s/%s missing key %q", key.Namespace, key.Name, tokenKey)
	}
	factory := r.CloudflareClientFactory
	if factory == nil {
		factory = func(accountID, apiToken string) (cloudflare.Client, error) {
			return cloudflare.New(accountID, apiToken)
		}
	}
	return factory(accountID, apiToken)
}

var errForeignRoute = errors.New("foreign route")

func (r *CloudflareTunnelRouteReconciler) reconcileCloudflareRoute(ctx context.Context, route *cfztv1alpha1.CloudflareTunnelRoute, cfClient cloudflare.Client, desired cloudflare.TunnelRouteInput) (*cloudflare.TunnelRoute, bool, error) {
	if route.Status.RouteId != "" {
		cfRoute, err := cfClient.TunnelRoutes().Get(ctx, route.Status.RouteId)
		if err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
			return nil, false, err
		}
		if err == nil {
			if !ownership.From(route.UID).MatchesComment(cfRoute.Comment) {
				return nil, false, errForeignRoute
			}
			if tunnelRouteMatches(*cfRoute, desired) {
				return cfRoute, false, nil
			}
			if err := r.preflightRouteTarget(ctx, cfClient, desired, cfRoute.ID); err != nil {
				return nil, false, err
			}
			updated, err := cfClient.TunnelRoutes().Edit(ctx, cfRoute.ID, desired)
			return updated, false, err
		}
	}

	existing, err := cfClient.TunnelRoutes().List(ctx, cloudflare.ListTunnelRoutesFilter{
		Network:          desired.Network,
		VirtualNetworkID: desired.VirtualNetworkID,
	})
	if err != nil {
		return nil, false, err
	}
	var owned *cloudflare.TunnelRoute
	for i := range existing {
		if ownership.From(route.UID).MatchesComment(existing[i].Comment) {
			copy := existing[i]
			owned = &copy
			continue
		}
		return nil, false, errForeignRoute
	}
	if owned != nil {
		if tunnelRouteMatches(*owned, desired) {
			return owned, false, nil
		}
		updated, err := cfClient.TunnelRoutes().Edit(ctx, owned.ID, desired)
		return updated, false, err
	}
	created, err := cfClient.TunnelRoutes().Create(ctx, desired)
	return created, true, err
}

func (r *CloudflareTunnelRouteReconciler) preflightRouteTarget(ctx context.Context, cfClient cloudflare.Client, desired cloudflare.TunnelRouteInput, allowedRouteID string) error {
	existing, err := cfClient.TunnelRoutes().List(ctx, cloudflare.ListTunnelRoutesFilter{
		Network:          desired.Network,
		VirtualNetworkID: desired.VirtualNetworkID,
	})
	if err != nil {
		return err
	}
	for _, candidate := range existing {
		if candidate.ID == allowedRouteID {
			continue
		}
		return errForeignRoute
	}
	return nil
}

func (r *CloudflareTunnelRouteReconciler) setRouteStatus(ctx context.Context, route *cfztv1alpha1.CloudflareTunnelRoute, routeID, virtualNetworkID string, ready bool, reason, message string) error {
	latest := &cfztv1alpha1.CloudflareTunnelRoute{}
	if err := r.Get(ctx, types.NamespacedName{Name: route.Name}, latest); err != nil {
		return err
	}
	before := latest.DeepCopy()
	latest.Status.RouteId = routeID
	latest.Status.VirtualNetworkId = virtualNetworkID
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

// SetupWithManager wires the controller.
func (r *CloudflareTunnelRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := indexCloudflareTunnelRouteFields(context.Background(), mgr); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&cfztv1alpha1.CloudflareTunnelRoute{}).
		Watches(&cfztv1alpha1.CloudflareTunnel{}, handler.EnqueueRequestsFromMapFunc(enqueueNamed(func(tunnel *cfztv1alpha1.CloudflareTunnel) []types.NamespacedName {
			routes, err := r.routesForTunnel(context.Background(), tunnel.Name)
			if err != nil {
				return nil
			}
			requests := make([]types.NamespacedName, 0, len(routes))
			for _, route := range routes {
				requests = append(requests, types.NamespacedName{Name: route.Name})
			}
			return requests
		}))).
		Named("cloudflaretunnelroute").
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func (r *CloudflareTunnelRouteReconciler) routesForTunnel(ctx context.Context, tunnelName string) ([]cfztv1alpha1.CloudflareTunnelRoute, error) {
	return listRoutesByTunnel(ctx, r.Client, tunnelName)
}

func canonicalNetwork(network string) (string, error) {
	prefix, err := netip.ParsePrefix(network)
	if err != nil {
		return "", fmt.Errorf("invalid CIDR %q: %w", network, err)
	}
	return prefix.Masked().String(), nil
}

func routeOwnershipComment(route *cfztv1alpha1.CloudflareTunnelRoute) string {
	prefix := ownership.From(route.UID).CompactComment()
	if route.Spec.Comment == "" {
		return prefix
	}
	return prefix + " | " + route.Spec.Comment
}

func tunnelRouteMatches(route cloudflare.TunnelRoute, desired cloudflare.TunnelRouteInput) bool {
	if route.Network != desired.Network || route.TunnelID != desired.TunnelID || route.Comment != desired.Comment {
		return false
	}
	if desired.VirtualNetworkID != "" && route.VirtualNetworkID != desired.VirtualNetworkID {
		return false
	}
	return true
}
