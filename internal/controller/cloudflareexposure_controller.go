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
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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
	Base
	HTTPRouteSourceEnabled bool
	// SiteID is this operator process's stable failover identity (D26). It is
	// plumbed through from --site-id at boot and read by failover-enabled
	// reconcile paths (lease arbitration, role gate).
	SiteID string
	// Now and Rand are injection seams for the failover role gate so tests
	// drive lease expiry and acquire jitter deterministically. Both default
	// to wall-clock / a process-seeded source when nil.
	Now  func() time.Time
	Rand *rand.Rand
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

	cfClient, err := r.CloudflareClient(ctx, credentialsRefFromTunnel(&tunnel))
	if err != nil {
		return r.setExposureStatusAndRequeue(ctx, &exposure, exposure.Status.Cloudflare, ReasonCredentialsMissing, err.Error())
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
		r.Recorder.Eventf(&exposure, corev1.EventTypeWarning, EventHostnameConflict, "Hostname %s is claimed by more than one CloudflareExposure", exposure.Spec.Hostname)
		return r.setExposureStatusAndRequeue(ctx, &exposure, exposure.Status.Cloudflare, ReasonHostnameConflict, "hostname is claimed by more than one CloudflareExposure")
	}

	// D26 failover role gate: a failover-enabled Exposure only writes the
	// shared Access app + public DNS CNAME when this site holds the lease.
	// A Standby (or any non-Primary outcome) is fully handled here.
	var failoverRequeue time.Duration
	if exposure.Spec.Failover != nil {
		if conflict, err := r.hasFailoverGroupConflict(ctx, &exposure); err != nil {
			return ctrl.Result{}, err
		} else if conflict {
			r.Recorder.Eventf(&exposure, corev1.EventTypeWarning, EventLeaseConflict, "spec.failover.group %q is used by more than one CloudflareExposure in this cluster", exposure.Spec.Failover.Group)
			return r.setExposureStatusAndRequeue(ctx, &exposure, exposure.Status.Cloudflare, ReasonFailoverGroupConflict, "spec.failover.group is shared by more than one CloudflareExposure in this cluster")
		}
		proceed, requeue, result, done, err := r.reconcileFailoverRole(ctx, &exposure, &tunnel, cfClient)
		if done || err != nil {
			return result, err
		}
		if !proceed {
			return result, nil
		}
		failoverRequeue = requeue
	}

	status := exposure.Status.Cloudflare
	result, done, err := r.reconcileExposureAccess(ctx, &exposure, cfClient, &status)
	if done || err != nil {
		return result, err
	}

	result, done, err = r.reconcileExposureDNS(ctx, &exposure, &tunnel, cfClient, &status)
	if done || err != nil {
		return result, err
	}

	status.PublicHostnameRouteHash = routeHashForExposure(&tunnel, &exposure)
	ready := status.PublicHostnameRouteHash != "" && (!exposure.Spec.Access.Enabled || len(status.AccessApplications) == 1 && status.AccessApplications[0].AppID != "") && (!tunnel.Spec.Dns.Manage || status.DnsRecordId != "")
	if !ready {
		log.V(1).Info("CloudflareExposure waiting for tunnel route", "hostname", exposure.Spec.Hostname)
		return requeueResult(failoverRequeue), r.setExposureStatus(ctx, &exposure, status, false, ReasonAccessAppPending, "waiting for tunnel ingress route hash")
	}
	return requeueResult(failoverRequeue), r.setExposureStatus(ctx, &exposure, status, true, ReasonReconciled, "Exposure reconciled")
}

func (r *CloudflareExposureReconciler) reconcileExposureAccess(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, cfClient cloudflare.Client, status *cfztv1alpha1.ExposureCloudflareStatus) (ctrl.Result, bool, error) {
	if exposure.Spec.Access.Enabled {
		app, statusEntry, action, err := r.reconcileAccess(ctx, exposure, cfClient)
		if err != nil {
			if errors.Is(err, errHostnameConflict) {
				r.Recorder.Eventf(exposure, corev1.EventTypeWarning, EventHostnameConflict, "Access application hostname conflict for %s", exposure.Spec.Hostname)
				result, statusErr := r.setExposureStatusAndRequeue(ctx, exposure, *status, ReasonHostnameConflict, err.Error())
				return result, true, statusErr
			}
			if errors.Is(err, errForeignResource) {
				result, statusErr := r.setExposureStatusAndRequeue(ctx, exposure, *status, ReasonForeignResource, err.Error())
				return result, true, statusErr
			}
			if errors.Is(err, errAccessApplicationsUnsupported) {
				return ctrl.Result{}, true, r.setExposureStatus(ctx, exposure, *status, false, ReasonAccessApplicationsUnsupported, err.Error())
			}
			if errors.Is(err, errPolicyNotFound) {
				return ctrl.Result{}, true, r.setExposureStatus(ctx, exposure, *status, false, ReasonPolicyNotFound, err.Error())
			}
			if errors.Is(err, errPolicyNotReady) {
				if statusErr := r.setExposureStatus(ctx, exposure, *status, false, ReasonPolicyNotReady, err.Error()); statusErr != nil {
					return ctrl.Result{}, true, statusErr
				}
				return ctrl.Result{RequeueAfter: 30 * time.Second}, true, nil
			}
			return ctrl.Result{}, true, err
		}
		status.AccessApplications = []cfztv1alpha1.ExposureAccessApplicationStatus{statusEntry}
		switch action {
		case accessApplicationReconcileCreated:
			r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventCreatedAccessApp, "Created Cloudflare Access application %s", app.ID)
		case accessApplicationReconcileUpdated:
			r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventUpdatedAccessApp, "Updated Cloudflare Access application %s", app.ID)
		}
		return ctrl.Result{}, false, nil
	}
	deleted, err := r.deleteOwnedAccessIfPresent(ctx, exposure, cfClient)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if deleted {
		if len(status.AccessApplications) > 0 {
			r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventDeletedAccessApp, "Deleted Cloudflare Access application %s for %s", status.AccessApplications[0].AppID, exposure.Spec.Hostname)
		}
	}
	status.AccessApplications = nil
	return ctrl.Result{}, false, nil
}

func (r *CloudflareExposureReconciler) reconcileExposureDNS(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, tunnel *cfztv1alpha1.CloudflareTunnel, cfClient cloudflare.Client, status *cfztv1alpha1.ExposureCloudflareStatus) (ctrl.Result, bool, error) {
	if tunnel.Spec.Dns.Manage {
		record, action, err := r.reconcileDNS(ctx, exposure, tunnel, cfClient)
		if err != nil {
			if errors.Is(err, errHostnameConflict) {
				r.Recorder.Eventf(exposure, corev1.EventTypeWarning, EventHostnameConflict, "DNS hostname conflict for %s", exposure.Spec.Hostname)
				result, statusErr := r.setExposureStatusAndRequeue(ctx, exposure, *status, ReasonHostnameConflict, err.Error())
				return result, true, statusErr
			}
			if errors.Is(err, errForeignResource) {
				result, statusErr := r.setExposureStatusAndRequeue(ctx, exposure, *status, ReasonForeignResource, err.Error())
				return result, true, statusErr
			}
			return ctrl.Result{}, true, r.setExposureStatusAndBackoff(ctx, exposure, *status, err.Error())
		}
		status.DnsRecordId = record.ID
		switch action {
		case dnsReconcileCreated:
			r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventCreatedDNSRecord, "Created Cloudflare DNS CNAME record %s for %s -> %s", record.ID, exposure.Spec.Hostname, record.Content)
		case dnsReconcileUpdated:
			r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventUpdatedDNSRecord, "Updated Cloudflare DNS CNAME record %s for %s -> %s", record.ID, exposure.Spec.Hostname, record.Content)
		}
		return ctrl.Result{}, false, nil
	}
	deleted, err := r.deleteOwnedDNSIfPresent(ctx, exposure, cfClient)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if deleted {
		r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventDeletedDNSRecord, "Deleted Cloudflare DNS CNAME record %s for %s", status.DnsRecordId, exposure.Spec.Hostname)
	}
	status.DnsRecordId = ""
	return ctrl.Result{}, false, nil
}

type dnsReconcileAction string

const (
	dnsReconcileUnchanged dnsReconcileAction = "unchanged"
	dnsReconcileCreated   dnsReconcileAction = "created"
	dnsReconcileUpdated   dnsReconcileAction = "updated"
)

var (
	errHostnameConflict              = errors.New("hostname conflict")
	errForeignResource               = errors.New("foreign resource")
	errAccessApplicationsUnsupported = errors.New("access applications unsupported")
	errPolicyNotFound                = errors.New("policy not found")
	errPolicyNotReady                = errors.New("policy not ready")
)

type accessApplicationReconcileAction string

const (
	accessApplicationReconcileUnchanged accessApplicationReconcileAction = "unchanged"
	accessApplicationReconcileCreated   accessApplicationReconcileAction = "created"
	accessApplicationReconcileUpdated   accessApplicationReconcileAction = "updated"
)

func (r *CloudflareExposureReconciler) reconcileAccess(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, cfClient cloudflare.Client) (*cloudflare.AccessApplication, cfztv1alpha1.ExposureAccessApplicationStatus, accessApplicationReconcileAction, error) {
	desired, err := r.desiredAccessApplication(ctx, exposure)
	if err != nil {
		return nil, cfztv1alpha1.ExposureAccessApplicationStatus{}, accessApplicationReconcileUnchanged, err
	}
	apps, err := cfClient.AccessApplications().List(ctx, exposure.Spec.Hostname)
	if err != nil {
		return nil, cfztv1alpha1.ExposureAccessApplicationStatus{}, accessApplicationReconcileUnchanged, err
	}
	owner := exposureOwner(exposure)
	var owned []*cloudflare.AccessApplication
	for i := range apps {
		app := &apps[i]
		if !owner.MatchesTags(app.Tags) {
			return nil, cfztv1alpha1.ExposureAccessApplicationStatus{}, accessApplicationReconcileUnchanged, fmt.Errorf("%w: Access application %s for hostname %s", errHostnameConflict, app.ID, exposure.Spec.Hostname)
		}
		owned = append(owned, app)
	}
	if len(owned) > 1 {
		return nil, cfztv1alpha1.ExposureAccessApplicationStatus{}, accessApplicationReconcileUnchanged, fmt.Errorf("%w: found %d Cloudflare Access applications for hostname %s; slice 8a only supports one root app", errAccessApplicationsUnsupported, len(owned), exposure.Spec.Hostname)
	}
	if len(owned) == 1 {
		ownedApp := owned[0]
		if accessApplicationMatchesDesired(*ownedApp, desired) {
			copy := *ownedApp
			return &copy, accessApplicationStatusFrom(exposure.Spec.Access.Applications[0].Name, &copy), accessApplicationReconcileUnchanged, nil
		}
		updated, err := cfClient.AccessApplications().Update(ctx, ownedApp.ID, desired)
		if err != nil {
			return nil, cfztv1alpha1.ExposureAccessApplicationStatus{}, accessApplicationReconcileUnchanged, err
		}
		return updated, accessApplicationStatusFrom(exposure.Spec.Access.Applications[0].Name, updated), accessApplicationReconcileUpdated, nil
	}
	app, err := cfClient.AccessApplications().Create(ctx, desired)
	if err != nil {
		return nil, cfztv1alpha1.ExposureAccessApplicationStatus{}, accessApplicationReconcileUnchanged, err
	}
	return app, accessApplicationStatusFrom(exposure.Spec.Access.Applications[0].Name, app), accessApplicationReconcileCreated, nil
}

func (r *CloudflareExposureReconciler) desiredAccessApplication(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure) (cloudflare.AccessApplicationInput, error) {
	if len(exposure.Spec.Access.Applications) != 1 {
		return cloudflare.AccessApplicationInput{}, fmt.Errorf("%w: access.applications[] must contain exactly one entry in slice 8a", errAccessApplicationsUnsupported)
	}
	appSpec := exposure.Spec.Access.Applications[0]
	policyUUIDs, err := r.resolveAccessPolicyUUIDs(ctx, exposure, appSpec.Policies)
	if err != nil {
		return cloudflare.AccessApplicationInput{}, err
	}
	return cloudflare.AccessApplicationInput{
		Name:        desiredAccessApplicationName(exposure, appSpec.Name),
		Domains:     accessApplicationDomainsAsStrings(appSpec.Domains),
		PolicyUUIDs: policyUUIDs,
		Tags:        exposureOwner(exposure).Tags(),
	}, nil
}

func (r *CloudflareExposureReconciler) resolveAccessPolicyUUIDs(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, policies []cfztv1alpha1.AccessApplicationPolicyBinding) ([]string, error) {
	uuids := make([]string, 0, len(policies))
	for _, policy := range policies {
		uuid, err := r.resolveAccessPolicyUUID(ctx, policy.PolicyRef)
		if err != nil {
			return nil, err
		}
		uuids = append(uuids, uuid)
	}
	return uuids, nil
}

func (r *CloudflareExposureReconciler) resolveAccessPolicyUUID(ctx context.Context, ref cfztv1alpha1.AccessPolicyRef) (string, error) {
	if ref.UUID != "" {
		return ref.UUID, nil
	}
	name := ref.Name
	if name == "" {
		return "", fmt.Errorf("%w: access.applications[].policies[].policyRef.name is empty", errPolicyNotFound)
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

// hasFailoverGroupConflict reports whether another CloudflareExposure anywhere
// in this cluster shares this Exposure's spec.failover.group. The lease name
// (_cfzt-lease.<hash8(group)>.<zone>) carries no namespace or hostname, and all
// Exposures in a cluster share one operator process / --site-id, so two members
// of the same group in one cluster would resolve to the same lease record and
// each read lease.Site == its own site-id — both treating it as self-owned with
// no mutual exclusion. The invariant is therefore one group member per cluster
// (D26); both contending Exposures back off without touching Cloudflare. This is
// cluster-wide, not per-namespace: the legitimate cross-cluster pair lives in
// separate apiservers, so each operator lists only its own cluster and never
// trips.
func (r *CloudflareExposureReconciler) hasFailoverGroupConflict(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure) (bool, error) {
	if exposure.Spec.Failover == nil {
		return false, nil
	}
	others, err := listExposuresByFailoverGroup(ctx, r.Client, exposure.Spec.Failover.Group)
	if err != nil {
		return false, err
	}
	for _, other := range others {
		if other.UID != exposure.UID && other.DeletionTimestamp.IsZero() {
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

func (r *CloudflareExposureReconciler) reconcileDNS(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, tunnel *cfztv1alpha1.CloudflareTunnel, cfClient cloudflare.Client) (*cloudflare.DNSRecord, dnsReconcileAction, error) {
	zone, err := cfClient.Zones().Resolve(ctx, exposure.Spec.Hostname)
	if err != nil {
		return nil, dnsReconcileUnchanged, err
	}
	owner := exposureOwner(exposure)
	want := cloudflare.DNSRecordInput{
		ZoneID:  zone.ID,
		Name:    exposure.Spec.Hostname,
		Type:    "CNAME",
		Content: tunnel.Status.TunnelId + ".cfargotunnel.com",
		Proxied: true,
		Comment: owner.Comment(),
	}
	records, err := cfClient.DNSRecords().List(ctx, zone.ID, exposure.Spec.Hostname, "CNAME")
	if err != nil {
		return nil, dnsReconcileUnchanged, err
	}
	var owned *cloudflare.DNSRecord
	for _, record := range records {
		if !owner.MatchesComment(record.Comment) {
			return nil, dnsReconcileUnchanged, fmt.Errorf("%w: DNS record %s for hostname %s", errHostnameConflict, record.ID, exposure.Spec.Hostname)
		}
	}
	for i := range records {
		if owner.MatchesComment(records[i].Comment) {
			owned = &records[i]
			break
		}
	}
	if owned != nil {
		if owned.Content == want.Content && owned.Proxied == want.Proxied && owned.Comment == want.Comment {
			copy := *owned
			return &copy, dnsReconcileUnchanged, nil
		}
		record, err := cfClient.DNSRecords().Update(ctx, owned.ID, want)
		return record, dnsReconcileUpdated, err
	}
	record, err := cfClient.DNSRecords().Create(ctx, want)
	return record, dnsReconcileCreated, err
}

func (r *CloudflareExposureReconciler) reconcileDelete(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(exposure, naming.Finalizer) {
		return ctrl.Result{}, nil
	}
	failover := exposure.Spec.Failover != nil
	// Non-failover Exposures with nothing written can drop the finalizer
	// without fetching credentials. Failover Exposures always take the
	// cleanup path so a Primary's lease record is removed before the CR goes.
	if !failover && len(exposure.Status.Cloudflare.AccessApplications) == 0 && exposure.Status.Cloudflare.DnsRecordId == "" {
		controllerutil.RemoveFinalizer(exposure, naming.Finalizer)
		return ctrl.Result{}, r.Update(ctx, exposure)
	}
	var tunnel cfztv1alpha1.CloudflareTunnel
	if err := r.Get(ctx, types.NamespacedName{Name: exposure.Spec.TunnelRef.Name}, &tunnel); err != nil {
		return r.setExposureStatusAndRequeue(ctx, exposure, exposure.Status.Cloudflare, ReasonCredentialsMissing, fmt.Sprintf("cleanup needs referenced CloudflareTunnel credentials: %v", err))
	}
	cfClient, err := r.CloudflareClient(ctx, credentialsRefFromTunnel(&tunnel))
	if err != nil {
		return r.setExposureStatusAndRequeue(ctx, exposure, exposure.Status.Cloudflare, ReasonCredentialsMissing, err.Error())
	}
	if failover {
		// Prove CURRENT lease ownership from the live record, not the persisted
		// status.failover.role: a returned primary whose peer has since acquired
		// the lease must not tear down the surface the peer is now serving.
		holdsLease, err := r.holdsLiveLease(ctx, exposure, cfClient)
		if err != nil {
			return ctrl.Result{}, err
		}
		// Remove this site's own lease record (no-op if a peer owns it).
		if err := r.deleteOwnedLeaseIfPresent(ctx, exposure, cfClient); err != nil {
			return ctrl.Result{}, err
		}
		if !holdsLease {
			controllerutil.RemoveFinalizer(exposure, naming.Finalizer)
			return ctrl.Result{}, r.Update(ctx, exposure)
		}
	}
	deletedDNS, err := r.deleteOwnedDNSIfPresent(ctx, exposure, cfClient)
	if err != nil {
		if errors.Is(err, errForeignResource) {
			return r.setExposureStatusAndRequeue(ctx, exposure, exposure.Status.Cloudflare, ReasonForeignResource, "DNS record is not owned by this CloudflareExposure")
		}
		return ctrl.Result{}, err
	}
	if deletedDNS {
		r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventDeletedDNSRecord, "Deleted Cloudflare DNS CNAME record %s for %s", exposure.Status.Cloudflare.DnsRecordId, exposure.Spec.Hostname)
	}
	deletedAccess, err := r.deleteOwnedAccessIfPresent(ctx, exposure, cfClient)
	if err != nil {
		if errors.Is(err, errForeignResource) {
			return r.setExposureStatusAndRequeue(ctx, exposure, exposure.Status.Cloudflare, ReasonForeignResource, "Access application is not owned by this CloudflareExposure")
		}
		return ctrl.Result{}, err
	}
	if deletedAccess {
		if len(exposure.Status.Cloudflare.AccessApplications) > 0 {
			r.Recorder.Eventf(exposure, corev1.EventTypeNormal, EventDeletedAccessApp, "Deleted Cloudflare Access application %s for %s", exposure.Status.Cloudflare.AccessApplications[0].AppID, exposure.Spec.Hostname)
		}
	}
	controllerutil.RemoveFinalizer(exposure, naming.Finalizer)
	return ctrl.Result{}, r.Update(ctx, exposure)
}

func (r *CloudflareExposureReconciler) deleteOwnedAccessIfPresent(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, cfClient cloudflare.Client) (bool, error) {
	if len(exposure.Status.Cloudflare.AccessApplications) == 0 {
		return false, nil
	}
	apps, err := cfClient.AccessApplications().List(ctx, exposure.Spec.Hostname)
	if err != nil {
		return false, err
	}
	owner := exposureOwner(exposure)
	byID := make(map[string]cloudflare.AccessApplication, len(apps))
	for _, app := range apps {
		byID[app.ID] = app
	}
	deleted := false
	for _, tracked := range exposure.Status.Cloudflare.AccessApplications {
		if tracked.AppID == "" {
			continue
		}
		app, ok := byID[tracked.AppID]
		if !ok {
			continue
		}
		if !owner.MatchesTags(app.Tags) {
			return false, errForeignResource
		}
		if err := cfClient.AccessApplications().Delete(ctx, app.ID); err != nil {
			if errors.Is(err, cloudflare.ErrNotFound) {
				continue
			}
			return false, err
		}
		deleted = true
	}
	for _, tag := range owner.Tags()[1:] {
		if err := cfClient.AccessTags().Delete(ctx, tag); err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
			return false, err
		}
	}
	return deleted, nil
}

func (r *CloudflareExposureReconciler) deleteOwnedDNSIfPresent(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, cfClient cloudflare.Client) (bool, error) {
	if exposure.Status.Cloudflare.DnsRecordId == "" {
		return false, nil
	}
	zone, err := cfClient.Zones().Resolve(ctx, exposure.Spec.Hostname)
	if err != nil {
		if errors.Is(err, cloudflare.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	records, err := cfClient.DNSRecords().List(ctx, zone.ID, exposure.Spec.Hostname, "CNAME")
	if err != nil {
		return false, err
	}
	owner := exposureOwner(exposure)
	for _, record := range records {
		if record.ID != exposure.Status.Cloudflare.DnsRecordId {
			continue
		}
		if !owner.MatchesComment(record.Comment) {
			return false, errForeignResource
		}
		if err := cfClient.DNSRecords().Delete(ctx, zone.ID, record.ID); err != nil {
			if errors.Is(err, cloudflare.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func (r *CloudflareExposureReconciler) setExposureStatus(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, cfStatus cfztv1alpha1.ExposureCloudflareStatus, ready bool, reason, message string) error {
	latest := &cfztv1alpha1.CloudflareExposure{}
	key := types.NamespacedName{Namespace: exposure.Namespace, Name: exposure.Name}
	if err := r.Get(ctx, key, latest); err != nil {
		return err
	}
	return r.setReady(ctx, latest, &latest.Status.Conditions, latest.Generation, ready, reason, message, func() {
		latest.Status.Cloudflare = cfStatus
	})
}

// setExposureStatusAndBackoff records a DNS/Cloudflare write failure and
// returns it as an error so controller-runtime applies exponential backoff
// (requeue policy: transient CF failures back off, waiting states requeue 30s).
func (r *CloudflareExposureReconciler) setExposureStatusAndBackoff(ctx context.Context, exposure *cfztv1alpha1.CloudflareExposure, cfStatus cfztv1alpha1.ExposureCloudflareStatus, message string) error {
	if err := r.setExposureStatus(ctx, exposure, cfStatus, false, ReasonDNSWriteFailed, message); err != nil {
		return err
	}
	return fmt.Errorf("%s: %s", ReasonDNSWriteFailed, message)
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

func desiredAccessApplicationName(exposure *cfztv1alpha1.CloudflareExposure, applicationName string) string {
	baseName := exposure.Spec.DisplayName
	if baseName == "" {
		baseName = exposure.Name
	}
	return baseName + "-" + applicationName + "-cfzt"
}

func accessApplicationMatchesDesired(app cloudflare.AccessApplication, want cloudflare.AccessApplicationInput) bool {
	return app.Name == want.Name &&
		app.Domain == accessApplicationPrimaryDomain(want.Domains) &&
		equality.Semantic.DeepEqual(app.Domains, want.Domains) &&
		equality.Semantic.DeepEqual(app.PolicyUUIDs, want.PolicyUUIDs) &&
		sameStringSet(app.Tags, want.Tags)
}

func accessApplicationStatusFrom(name string, app *cloudflare.AccessApplication) cfztv1alpha1.ExposureAccessApplicationStatus {
	if app == nil {
		return cfztv1alpha1.ExposureAccessApplicationStatus{}
	}
	return cfztv1alpha1.ExposureAccessApplicationStatus{
		Name:                name,
		AppID:               app.ID,
		CanonicalDomainHash: hashStringsSorted(app.Domains),
		PolicyHash:          hashStringsOrdered(app.PolicyUUIDs),
	}
}

func hashStringsSorted(values []string) string {
	return hashStrings(sortedCopy(values))
}

func hashStringsOrdered(values []string) string {
	return hashStrings(values)
}

func hashStrings(values []string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return fmt.Sprintf("sha256:%x", sum[:])
}

func accessApplicationDomainsAsStrings(domains []cfztv1alpha1.AccessApplicationDomain) []string {
	out := make([]string, len(domains))
	for i := range domains {
		out[i] = string(domains[i])
	}
	return out
}

func accessApplicationPrimaryDomain(domains []string) string {
	if len(domains) == 0 {
		return ""
	}
	return domains[0]
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
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
