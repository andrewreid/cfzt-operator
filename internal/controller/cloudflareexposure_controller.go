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
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
	"github.com/andrewreid/cfzt-operator/internal/naming"
	"github.com/andrewreid/cfzt-operator/internal/origin"
)

// CloudflareExposureReconciler reconciles a CloudflareExposure object.
type CloudflareExposureReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	CloudflareClientFactory CloudflareClientFactory
	Recorder                events.EventRecorder
	HTTPRouteSourceEnabled  bool
}

// +kubebuilder:rbac:groups=cfzt.reid.ee,resources=cloudflareexposures,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cfzt.reid.ee,resources=cloudflareexposures/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cfzt.reid.ee,resources=cloudflareexposures/finalizers,verbs=update
// +kubebuilder:rbac:groups=cfzt.reid.ee,resources=cloudflaretunnels,verbs=get;list;watch
// +kubebuilder:rbac:groups=cfzt.reid.ee,resources=cloudflareaccesspolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *CloudflareExposureReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var exposure cfztv1alpha1.CloudflareExposure
	if err := r.Get(ctx, req.NamespacedName, &exposure); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !exposure.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &exposure)
	}

	var tunnel cfztv1alpha1.CloudflareTunnel
	if err := r.Get(ctx, types.NamespacedName{Name: exposure.Spec.TunnelRef.Name}, &tunnel); err != nil {
		return ctrl.Result{}, r.setExposureStatus(ctx, &exposure, exposure.Status.Cloudflare, false, ReasonOriginInvalid, fmt.Sprintf("referenced CloudflareTunnel not found: %v", err))
	}

	cfClient, err := r.cloudflareClient(ctx, &tunnel)
	if err != nil {
		return ctrl.Result{}, r.setExposureStatus(ctx, &exposure, exposure.Status.Cloudflare, false, ReasonCredentialsMissing, err.Error())
	}

	if controllerutil.AddFinalizer(&exposure, naming.Finalizer) {
		if err := r.Update(ctx, &exposure); err != nil {
			return ctrl.Result{}, err
		}
	}

	defaulted, err := r.defaultFromSourceRef(ctx, &exposure)
	if err != nil {
		return ctrl.Result{}, r.setExposureStatus(ctx, &exposure, exposure.Status.Cloudflare, false, ReasonOriginInvalid, err.Error())
	}
	if defaulted {
		return ctrl.Result{Requeue: true}, nil
	}

	if exposure.Spec.Origin == nil {
		return ctrl.Result{}, r.setExposureStatus(ctx, &exposure, exposure.Status.Cloudflare, false, ReasonOriginInvalid, "origin is required")
	}
	if exposure.Spec.Hostname == "" {
		return ctrl.Result{}, r.setExposureStatus(ctx, &exposure, exposure.Status.Cloudflare, false, ReasonOriginInvalid, "hostname is required")
	}
	if conflict, err := r.hasDuplicateHostname(ctx, &exposure); err != nil {
		return ctrl.Result{}, err
	} else if conflict {
		r.event(&exposure, corev1.EventTypeWarning, EventHostnameConflict, "Hostname %s is claimed by more than one CloudflareExposure", exposure.Spec.Hostname)
		return r.setExposureStatusAndBackoff(ctx, &exposure, exposure.Status.Cloudflare, ReasonHostnameConflict, "hostname is claimed by more than one CloudflareExposure")
	}

	status := exposure.Status.Cloudflare
	status, done, result, err := r.reconcileExposureAccess(ctx, &exposure, cfClient, status)
	if done {
		return result, err
	}

	status, done, result, err = r.reconcileExposureDNS(ctx, &exposure, &tunnel, cfClient, status)
	if done {
		return result, err
	}

	status.PublicHostnameRouteHash = routeHashForExposure(&tunnel, &exposure)
	ready := status.PublicHostnameRouteHash != "" && (!exposure.Spec.Access.Enabled || status.AccessApplicationId != "") && (!tunnel.Spec.Dns.Manage || status.DnsRecordId != "")
	if !ready {
		log.V(1).Info("CloudflareExposure waiting for tunnel route", "hostname", exposure.Spec.Hostname)
		return ctrl.Result{}, r.setExposureStatus(ctx, &exposure, status, false, ReasonAccessAppPending, "waiting for tunnel ingress route hash")
	}
	return ctrl.Result{}, r.setExposureStatus(ctx, &exposure, status, true, ReasonReconciled, "Exposure reconciled")
}

func (r *CloudflareExposureReconciler) cloudflareClient(ctx context.Context, tunnel *cfztv1alpha1.CloudflareTunnel) (cloudflare.Client, error) {
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

func (r *CloudflareExposureReconciler) reconcileExposureAccess(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, cfClient cloudflare.Client, status cfztv1alpha1.ExposureCloudflareStatus) (cfztv1alpha1.ExposureCloudflareStatus, bool, ctrl.Result, error) {
	if exposure.Spec.Access.Enabled {
		app, created, err := r.reconcileAccess(ctx, exposure, cfClient)
		if err != nil {
			if errors.Is(err, errHostnameConflict) {
				r.event(exposure, corev1.EventTypeWarning, EventHostnameConflict, "Access application hostname conflict for %s", exposure.Spec.Hostname)
				result, statusErr := r.setExposureStatusAndBackoff(ctx, exposure, status, ReasonHostnameConflict, err.Error())
				return status, true, result, statusErr
			}
			if errors.Is(err, errForeignResource) {
				result, statusErr := r.setExposureStatusAndBackoff(ctx, exposure, status, ReasonForeignResource, err.Error())
				return status, true, result, statusErr
			}
			if errors.Is(err, errPolicyNotFound) {
				return status, true, ctrl.Result{}, r.setExposureStatus(ctx, exposure, status, false, ReasonPolicyNotFound, err.Error())
			}
			if errors.Is(err, errPolicyNotReady) {
				if statusErr := r.setExposureStatus(ctx, exposure, status, false, ReasonPolicyNotReady, err.Error()); statusErr != nil {
					return status, true, ctrl.Result{}, statusErr
				}
				return status, true, ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			return status, true, ctrl.Result{}, err
		}
		status.AccessApplicationId = app.ID
		if created {
			r.event(exposure, corev1.EventTypeNormal, EventCreatedAccessApp, "Created Cloudflare Access application %s", app.ID)
		}
		return status, false, ctrl.Result{}, nil
	}
	if err := r.deleteOwnedAccessIfPresent(ctx, exposure, cfClient); err != nil {
		return status, true, ctrl.Result{}, err
	}
	status.AccessApplicationId = ""
	return status, false, ctrl.Result{}, nil
}

func (r *CloudflareExposureReconciler) reconcileExposureDNS(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, tunnel *cfztv1alpha1.CloudflareTunnel, cfClient cloudflare.Client, status cfztv1alpha1.ExposureCloudflareStatus) (cfztv1alpha1.ExposureCloudflareStatus, bool, ctrl.Result, error) {
	if tunnel.Spec.Dns.Manage {
		record, err := r.reconcileDNS(ctx, exposure, tunnel, cfClient)
		if err != nil {
			if errors.Is(err, errHostnameConflict) {
				r.event(exposure, corev1.EventTypeWarning, EventHostnameConflict, "DNS hostname conflict for %s", exposure.Spec.Hostname)
				result, statusErr := r.setExposureStatusAndBackoff(ctx, exposure, status, ReasonHostnameConflict, err.Error())
				return status, true, result, statusErr
			}
			if errors.Is(err, errForeignResource) {
				result, statusErr := r.setExposureStatusAndBackoff(ctx, exposure, status, ReasonForeignResource, err.Error())
				return status, true, result, statusErr
			}
			return status, true, ctrl.Result{}, r.setExposureStatus(ctx, exposure, status, false, ReasonDNSWriteFailed, err.Error())
		}
		status.DnsRecordId = record.ID
		return status, false, ctrl.Result{}, nil
	}
	if err := r.deleteOwnedDNSIfPresent(ctx, exposure, cfClient); err != nil {
		return status, true, ctrl.Result{}, err
	}
	status.DnsRecordId = ""
	return status, false, ctrl.Result{}, nil
}

var (
	errHostnameConflict = errors.New("hostname conflict")
	errForeignResource  = errors.New("foreign resource")
	errPolicyNotFound   = errors.New("policy not found")
	errPolicyNotReady   = errors.New("policy not ready")
)

func (r *CloudflareExposureReconciler) reconcileAccess(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, cfClient cloudflare.Client) (*cloudflare.AccessApplication, bool, error) {
	apps, err := cfClient.AccessApplications().List(ctx, exposure.Spec.Hostname)
	if err != nil {
		return nil, false, err
	}
	policyUUID, err := r.resolveAccessPolicyUUID(ctx, exposure)
	if err != nil {
		return nil, false, err
	}
	want := cloudflare.AccessApplicationInput{
		Name:       naming.AccessAppName(exposure.Spec.DisplayName, exposure.Name),
		Domain:     exposure.Spec.Hostname,
		PolicyUUID: policyUUID,
		Tags:       ownershipTags(exposure.UID),
	}
	if err := ensureAccessTags(ctx, cfClient, want.Tags); err != nil {
		return nil, false, err
	}
	var owned *cloudflare.AccessApplication
	for _, app := range apps {
		if owner, ok := ownershipUIDFromTags(app.Tags); ok && owner != exposure.UID {
			return nil, false, fmt.Errorf("%w: Access application %s for hostname %s", errHostnameConflict, app.ID, exposure.Spec.Hostname)
		}
		if !ownedByTags(app.Tags, exposure.UID) {
			return nil, false, fmt.Errorf("%w: Access application %s for hostname %s", errHostnameConflict, app.ID, exposure.Spec.Hostname)
		}
	}
	for i := range apps {
		if ownedByTags(apps[i].Tags, exposure.UID) {
			owned = &apps[i]
			break
		}
	}
	if owned != nil {
		if owned.Name == want.Name && accessApplicationPoliciesMatch(*owned, want.PolicyUUID) && sameStringSet(owned.Tags, want.Tags) {
			copy := *owned
			return &copy, false, nil
		}
		updated, err := cfClient.AccessApplications().Update(ctx, owned.ID, want)
		return updated, false, err
	}
	app, err := cfClient.AccessApplications().Create(ctx, want)
	return app, true, err
}

func (r *CloudflareExposureReconciler) resolveAccessPolicyUUID(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure) (string, error) {
	if exposure.Spec.Access.PolicyRef.UUID != "" {
		return exposure.Spec.Access.PolicyRef.UUID, nil
	}
	name := exposure.Spec.Access.PolicyRef.Name
	if name == "" {
		return "", fmt.Errorf("%w: access.policyRef.name is empty", errPolicyNotFound)
	}
	var policy cfztv1alpha1.CloudflareAccessPolicy
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("%w: CloudflareAccessPolicy %q not found", errPolicyNotFound, name)
		}
		return "", err
	}
	ready := meta.FindStatusCondition(policy.Status.Conditions, ConditionReady)
	if !policy.DeletionTimestamp.IsZero() || policy.Status.PolicyId == "" || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != policy.Generation {
		return "", fmt.Errorf("%w: CloudflareAccessPolicy %q is not ready", errPolicyNotReady, name)
	}
	return policy.Status.PolicyId, nil
}

func (r *CloudflareExposureReconciler) hasDuplicateHostname(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure) (bool, error) {
	others, err := listExposuresByHostname(ctx, r.Client, exposure.Spec.Hostname)
	if err != nil {
		return false, err
	}
	for _, other := range others {
		if other.UID == exposure.UID {
			continue
		}
		if other.Spec.TunnelRef.Name == exposure.Spec.TunnelRef.Name && other.Spec.Hostname == exposure.Spec.Hostname {
			return true, nil
		}
	}
	return false, nil
}

func (r *CloudflareExposureReconciler) defaultFromSourceRef(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure) (bool, error) {
	if exposure.Spec.SourceRef == nil {
		return false, nil
	}
	before := exposure.DeepCopy()
	switch exposure.Spec.SourceRef.Kind {
	case origin.ServiceKind:
		if exposure.Spec.SourceRef.ApiVersion != "v1" {
			return false, fmt.Errorf("sourceRef.apiVersion must be v1 for Service")
		}
		svc := &corev1.Service{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Spec.SourceRef.Name}, svc); err != nil {
			return false, fmt.Errorf("sourceRef Service %s/%s not readable: %w", exposure.Namespace, exposure.Spec.SourceRef.Name, err)
		}
		resolved, err := origin.FromService(exposure, svc)
		if err != nil {
			return false, err
		}
		exposure.Spec.Origin = resolved
		ensureOwnerReference(exposure, "v1", origin.ServiceKind, svc.Name, svc.UID)
	case origin.HTTPRouteKind:
		if exposure.Spec.SourceRef.ApiVersion != origin.HTTPRouteAPIVersion {
			return false, fmt.Errorf("sourceRef.apiVersion must be %s for HTTPRoute", origin.HTTPRouteAPIVersion)
		}
		if !r.HTTPRouteSourceEnabled {
			return false, errors.New("HTTPRoute CRD not found, controller disabled")
		}
		route := &unstructured.Unstructured{}
		route.SetGroupVersionKind(schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: origin.HTTPRouteKind})
		if err := r.Get(ctx, types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Spec.SourceRef.Name}, route); err != nil {
			if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				return false, fmt.Errorf("sourceRef HTTPRoute %s/%s not readable: %w", exposure.Namespace, exposure.Spec.SourceRef.Name, err)
			}
			return false, err
		}
		if exposure.Spec.Hostname == "" {
			hostname, err := origin.HostnameFromHTTPRoute(route)
			if err != nil {
				return false, err
			}
			exposure.Spec.Hostname = hostname
		}
		ensureOwnerReference(exposure, origin.HTTPRouteAPIVersion, origin.HTTPRouteKind, route.GetName(), route.GetUID())
	default:
		return false, fmt.Errorf("unsupported sourceRef kind %q", exposure.Spec.SourceRef.Kind)
	}
	if equality.Semantic.DeepEqual(before.Spec, exposure.Spec) && equality.Semantic.DeepEqual(before.OwnerReferences, exposure.OwnerReferences) {
		return false, nil
	}
	return true, r.Update(ctx, exposure)
}

func ensureOwnerReference(exposure *cfztv1alpha1.CloudflareExposure, apiVersion, kind, name string, uid types.UID) {
	blockOwnerDeletion := false
	ref := metav1.OwnerReference{
		APIVersion:         apiVersion,
		Kind:               kind,
		Name:               name,
		UID:                uid,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}
	refs := exposure.OwnerReferences
	for i := range refs {
		if refs[i].UID == uid {
			refs[i] = ref
			exposure.OwnerReferences = refs
			return
		}
	}
	exposure.OwnerReferences = append(refs, ref)
}

func (r *CloudflareExposureReconciler) reconcileDNS(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, tunnel *cfztv1alpha1.CloudflareTunnel, cfClient cloudflare.Client) (*cloudflare.DNSRecord, error) {
	zone, err := cfClient.Zones().Resolve(ctx, exposure.Spec.Hostname)
	if err != nil {
		return nil, err
	}
	want := cloudflare.DNSRecordInput{
		ZoneID:  zone.ID,
		Name:    exposure.Spec.Hostname,
		Type:    "CNAME",
		Content: tunnel.Status.TunnelId + ".cfargotunnel.com",
		Proxied: true,
		Comment: naming.OwnershipTag(exposure.UID),
	}
	records, err := cfClient.DNSRecords().List(ctx, zone.ID, exposure.Spec.Hostname, "CNAME")
	if err != nil {
		return nil, err
	}
	var owned *cloudflare.DNSRecord
	for _, record := range records {
		if owner, ok := naming.ParseOwnershipTag(record.Comment); ok && owner != exposure.UID {
			return nil, fmt.Errorf("%w: DNS record %s for hostname %s", errHostnameConflict, record.ID, exposure.Spec.Hostname)
		}
		if !ownedByComment(record.Comment, exposure.UID) {
			return nil, fmt.Errorf("%w: DNS record %s for hostname %s", errHostnameConflict, record.ID, exposure.Spec.Hostname)
		}
	}
	for i := range records {
		if ownedByComment(records[i].Comment, exposure.UID) {
			owned = &records[i]
			break
		}
	}
	if owned != nil {
		if owned.Content == want.Content && owned.Proxied == want.Proxied && owned.Comment == want.Comment {
			copy := *owned
			return &copy, nil
		}
		return cfClient.DNSRecords().Update(ctx, owned.ID, want)
	}
	return cfClient.DNSRecords().Create(ctx, want)
}

func (r *CloudflareExposureReconciler) reconcileDelete(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(exposure, naming.Finalizer) {
		return ctrl.Result{}, nil
	}
	if exposure.Status.Cloudflare.AccessApplicationId == "" && exposure.Status.Cloudflare.DnsRecordId == "" {
		controllerutil.RemoveFinalizer(exposure, naming.Finalizer)
		return ctrl.Result{}, r.Update(ctx, exposure)
	}
	var tunnel cfztv1alpha1.CloudflareTunnel
	if err := r.Get(ctx, types.NamespacedName{Name: exposure.Spec.TunnelRef.Name}, &tunnel); err != nil {
		return r.setExposureStatusAndRequeue(ctx, exposure, exposure.Status.Cloudflare, ReasonCredentialsMissing, fmt.Sprintf("cleanup needs referenced CloudflareTunnel credentials: %v", err))
	}
	cfClient, err := r.cloudflareClient(ctx, &tunnel)
	if err != nil {
		return r.setExposureStatusAndRequeue(ctx, exposure, exposure.Status.Cloudflare, ReasonCredentialsMissing, err.Error())
	}
	if err := r.deleteOwnedDNSIfPresent(ctx, exposure, cfClient); err != nil {
		if errors.Is(err, errForeignResource) {
			return ctrl.Result{}, r.setExposureStatus(ctx, exposure, exposure.Status.Cloudflare, false, ReasonForeignResource, "DNS record is not owned by this CloudflareExposure")
		}
		return ctrl.Result{}, err
	}
	if err := r.deleteOwnedAccessIfPresent(ctx, exposure, cfClient); err != nil {
		if errors.Is(err, errForeignResource) {
			return ctrl.Result{}, r.setExposureStatus(ctx, exposure, exposure.Status.Cloudflare, false, ReasonForeignResource, "Access application is not owned by this CloudflareExposure")
		}
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(exposure, naming.Finalizer)
	return ctrl.Result{}, r.Update(ctx, exposure)
}

func (r *CloudflareExposureReconciler) deleteOwnedAccessIfPresent(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, cfClient cloudflare.Client) error {
	if exposure.Status.Cloudflare.AccessApplicationId == "" {
		return nil
	}
	apps, err := cfClient.AccessApplications().List(ctx, exposure.Spec.Hostname)
	if err != nil {
		return err
	}
	for _, app := range apps {
		if app.ID != exposure.Status.Cloudflare.AccessApplicationId {
			continue
		}
		if !ownedByTags(app.Tags, exposure.UID) {
			return errForeignResource
		}
		if err := cfClient.AccessApplications().Delete(ctx, app.ID); err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
			return err
		}
	}
	for _, tag := range sourceUIDTags(exposure.UID) {
		if err := cfClient.AccessTags().Delete(ctx, tag); err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
			return err
		}
	}
	return nil
}

func (r *CloudflareExposureReconciler) deleteOwnedDNSIfPresent(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, cfClient cloudflare.Client) error {
	if exposure.Status.Cloudflare.DnsRecordId == "" {
		return nil
	}
	zone, err := cfClient.Zones().Resolve(ctx, exposure.Spec.Hostname)
	if err != nil {
		if errors.Is(err, cloudflare.ErrNotFound) {
			return nil
		}
		return err
	}
	records, err := cfClient.DNSRecords().List(ctx, zone.ID, exposure.Spec.Hostname, "CNAME")
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.ID != exposure.Status.Cloudflare.DnsRecordId {
			continue
		}
		if !ownedByComment(record.Comment, exposure.UID) {
			return errForeignResource
		}
		if err := cfClient.DNSRecords().Delete(ctx, zone.ID, record.ID); err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
			return err
		}
	}
	return nil
}

func (r *CloudflareExposureReconciler) setExposureStatus(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, cfStatus cfztv1alpha1.ExposureCloudflareStatus, ready bool, reason, message string) error {
	latest := &cfztv1alpha1.CloudflareExposure{}
	key := types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Name}
	if err := r.Get(ctx, key, latest); err != nil {
		return err
	}
	before := latest.DeepCopy()
	latest.Status.Cloudflare = cfStatus
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

func (r *CloudflareExposureReconciler) setExposureStatusAndBackoff(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, cfStatus cfztv1alpha1.ExposureCloudflareStatus, reason, message string) (ctrl.Result, error) {
	if err := r.setExposureStatus(ctx, exposure, cfStatus, false, reason, message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, fmt.Errorf("%s: %s", reason, message)
}

func (r *CloudflareExposureReconciler) setExposureStatusAndRequeue(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, cfStatus cfztv1alpha1.ExposureCloudflareStatus, reason, message string) (ctrl.Result, error) {
	if err := r.setExposureStatus(ctx, exposure, cfStatus, false, reason, message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func routeHashForExposure(tunnel *cfztv1alpha1.CloudflareTunnel, exposure *cfztv1alpha1.CloudflareExposure) string {
	for _, route := range tunnel.Status.Routes {
		if route.ExposureUid == exposure.UID && route.Namespace == exposure.Namespace && route.Name == exposure.Name && route.Hostname == exposure.Spec.Hostname {
			return route.Hash
		}
	}
	return ""
}

func ownershipTags(uid types.UID) []string {
	return append([]string{accessManagedByTag}, sourceUIDTags(uid)...)
}

const (
	accessManagedByTag         = "managed-by=cfzt-operator"
	accessSourceUIDChunkPrefix = "source-uid-"
	accessTagMaxLength         = 35
)

func ensureAccessTags(ctx context.Context, cfClient cloudflare.Client, tags []string) error {
	for _, tag := range tags {
		if err := cfClient.AccessTags().Ensure(ctx, tag); err != nil {
			return err
		}
	}
	return nil
}

func ownedByTags(tags []string, uid types.UID) bool {
	found, ok := ownershipUIDFromTags(tags)
	return ok && found == uid
}

func ownershipUIDFromTags(tags []string) (types.UID, bool) {
	hasManagedBy := false
	chunks := map[int]string{}
	for _, tag := range tags {
		if tag == accessManagedByTag {
			hasManagedBy = true
			continue
		}
		if rest, ok := strings.CutPrefix(tag, accessSourceUIDChunkPrefix); ok {
			idxText, value, ok := strings.Cut(rest, "=")
			if !ok || value == "" {
				continue
			}
			idx, err := strconv.Atoi(idxText)
			if err != nil || idx < 0 {
				continue
			}
			chunks[idx] = value
		}
	}
	if !hasManagedBy {
		return "", false
	}
	if len(chunks) == 0 {
		return "", false
	}
	var b strings.Builder
	for i := 0; i < len(chunks); i++ {
		chunk, ok := chunks[i]
		if !ok {
			return "", false
		}
		b.WriteString(chunk)
	}
	return types.UID(b.String()), true
}

func sourceUIDTags(uid types.UID) []string {
	value := string(uid)
	var tags []string
	for idx := 0; value != ""; idx++ {
		prefix := fmt.Sprintf("%s%d=", accessSourceUIDChunkPrefix, idx)
		chunkSize := accessTagMaxLength - len(prefix)
		if chunkSize <= 0 {
			panic("access source UID tag prefix exceeds Cloudflare tag length limit")
		}
		if chunkSize > len(value) {
			chunkSize = len(value)
		}
		tags = append(tags, prefix+value[:chunkSize])
		value = value[chunkSize:]
	}
	return tags
}

func ownedByComment(comment string, uid types.UID) bool {
	prefix, _, _ := strings.Cut(comment, " | ")
	found, ok := naming.ParseOwnershipTag(prefix)
	return ok && found == uid
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

func accessApplicationPoliciesMatch(app cloudflare.AccessApplication, wantPolicyUUID string) bool {
	if len(app.PolicyUUIDs) > 0 {
		return sameStringSet(app.PolicyUUIDs, []string{wantPolicyUUID})
	}
	return app.PolicyUUID == wantPolicyUUID
}

func (r *CloudflareExposureReconciler) event(exposure *cfztv1alpha1.CloudflareExposure, eventType, reason, messageFmt string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(exposure, nil, eventType, reason, reason, messageFmt, args...)
}

// SetupWithManager sets up the controller with the Manager.
func (r *CloudflareExposureReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := indexCloudflareExposureFields(context.Background(), mgr); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&cfztv1alpha1.CloudflareExposure{}).
		Watches(&cfztv1alpha1.CloudflareTunnel{}, handler.EnqueueRequestsFromMapFunc(enqueueNamed(func(tunnel *cfztv1alpha1.CloudflareTunnel) []types.NamespacedName {
			exposures, err := r.exposuresForTunnel(context.Background(), tunnel.Name)
			if err != nil {
				return nil
			}
			requests := make([]types.NamespacedName, 0, len(exposures))
			for _, exposure := range exposures {
				requests = append(requests, types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Name})
			}
			return requests
		}))).
		Watches(&cfztv1alpha1.CloudflareAccessPolicy{}, handler.EnqueueRequestsFromMapFunc(enqueueNamed(func(policy *cfztv1alpha1.CloudflareAccessPolicy) []types.NamespacedName {
			exposures, err := r.exposuresForPolicy(context.Background(), policy.Name)
			if err != nil {
				return nil
			}
			requests := make([]types.NamespacedName, 0, len(exposures))
			for _, exposure := range exposures {
				requests = append(requests, types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Name})
			}
			return requests
		}))).
		Named("cloudflareexposure").
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func (r *CloudflareExposureReconciler) exposuresForTunnel(ctx context.Context, tunnelName string) ([]cfztv1alpha1.CloudflareExposure, error) {
	return listExposuresByTunnel(ctx, r.Client, tunnelName)
}

func (r *CloudflareExposureReconciler) exposuresForPolicy(ctx context.Context, policyName string) ([]cfztv1alpha1.CloudflareExposure, error) {
	return listExposuresByPolicy(ctx, r.Client, policyName)
}
