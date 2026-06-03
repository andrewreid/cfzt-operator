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
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
)

// CloudflareAccessPolicyReconciler reconciles a CloudflareAccessPolicy object.
type CloudflareAccessPolicyReconciler struct {
	Base
}

// +kubebuilder:rbac:groups=cfzt.reid.ee,resources=cloudflareaccesspolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cfzt.reid.ee,resources=cloudflareaccesspolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cfzt.reid.ee,resources=cloudflareaccesspolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *CloudflareAccessPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var policy cfztv1alpha1.CloudflareAccessPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !policy.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &policy)
	}

	if controllerutil.AddFinalizer(&policy, naming.Finalizer) {
		if err := r.Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	references, err := r.referencedBy(ctx, policy.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	cfClient, err := r.CloudflareClient(ctx, credentialsRefFromAccessPolicy(&policy))
	if err != nil {
		if statusErr := r.setPolicyStatus(ctx, &policy, policy.Status.PolicyId, policy.Status.ObservedRulesHash, references, false, ReasonCredentialsMissing, err.Error()); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	policyName := desiredPolicyName(&policy)
	desiredHash, err := accessPolicyRulesHash(&policy)
	if err != nil {
		return ctrl.Result{}, err
	}

	if policy.Status.PolicyId == "" {
		existing, err := cfClient.AccessPolicies().List(ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
		for _, p := range existing {
			if p.Name == policyName {
				cfPolicy, err := cfClient.AccessPolicies().Get(ctx, p.ID)
				if err != nil {
					if errors.Is(err, cloudflare.ErrUnsupportedAccessRule) {
						return ctrl.Result{}, r.setPolicyStatus(ctx, &policy, "", "", references, false, ReasonForeignPolicy, fmt.Sprintf("Cloudflare Access policy %q already exists without local ownership record and has unsupported rules", policyName))
					}
					return ctrl.Result{}, err
				}
				if accessPolicyMatches(cfPolicy, &policy, policyName) {
					if err := r.setPolicyStatus(ctx, &policy, cfPolicy.ID, desiredHash, references, true, ReasonReconciled, "Cloudflare Access policy identity recovered"); err != nil {
						return ctrl.Result{}, err
					}
					r.Recorder.Eventf(&policy, corev1.EventTypeNormal, EventRecoveredAccessPolicy, "Recovered Cloudflare Access policy %s from exact name and rules match", cfPolicy.ID)
					return ctrl.Result{}, nil
				}
				return ctrl.Result{}, r.setPolicyStatus(ctx, &policy, "", "", references, false, ReasonForeignPolicy, fmt.Sprintf("Cloudflare Access policy %q already exists without local ownership record", policyName))
			}
		}
		created, err := cfClient.AccessPolicies().Create(ctx, buildAccessPolicyInput(&policy, policyName))
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.setPolicyStatus(ctx, &policy, created.ID, desiredHash, references, true, ReasonReconciled, "Cloudflare Access policy reconciled"); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(&policy, corev1.EventTypeNormal, EventCreatedAccessPolicy, "Created Cloudflare Access policy %s", created.ID)
		log.V(1).Info("CloudflareAccessPolicy created", "policyID", created.ID)
		return ctrl.Result{}, nil
	}

	cfPolicy, err := cfClient.AccessPolicies().Get(ctx, policy.Status.PolicyId)
	if errors.Is(err, cloudflare.ErrNotFound) {
		// Status ID points at a deleted CF policy; clear and requeue.
		if statusErr := r.setPolicyStatus(ctx, &policy, "", "", references, false, ReasonReconciled, "tracked Cloudflare Access policy no longer exists, recreating"); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if err != nil {
		if errors.Is(err, cloudflare.ErrUnsupportedAccessRule) {
			return ctrl.Result{}, r.setPolicyStatus(ctx, &policy, policy.Status.PolicyId, policy.Status.ObservedRulesHash, references, false, ReasonUnsupportedDrift, err.Error())
		}
		return ctrl.Result{}, err
	}
	if cfPolicy.Name != policyName {
		return ctrl.Result{}, r.setPolicyStatus(ctx, &policy, policy.Status.PolicyId, policy.Status.ObservedRulesHash, references, false, ReasonForeignPolicy, fmt.Sprintf("tracked Cloudflare Access policy %s has name %q, want %q", cfPolicy.ID, cfPolicy.Name, policyName))
	}

	if policy.Status.ObservedRulesHash != desiredHash || !accessPolicyMatches(cfPolicy, &policy, policyName) {
		updated, err := cfClient.AccessPolicies().Update(ctx, policy.Status.PolicyId, buildAccessPolicyInput(&policy, policyName))
		if err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(&policy, corev1.EventTypeNormal, EventUpdatedAccessPolicy, "Updated Cloudflare Access policy %s", updated.ID)
		log.V(1).Info("CloudflareAccessPolicy updated", "policyID", updated.ID)
		return ctrl.Result{}, r.setPolicyStatus(ctx, &policy, policy.Status.PolicyId, desiredHash, references, true, ReasonReconciled, "Cloudflare Access policy reconciled")
	}

	log.V(1).Info("CloudflareAccessPolicy reconciled", "policyID", policy.Status.PolicyId)
	return ctrl.Result{}, r.setPolicyStatus(ctx, &policy, policy.Status.PolicyId, desiredHash, references, true, ReasonReconciled, "Cloudflare Access policy reconciled")
}

func (r *CloudflareAccessPolicyReconciler) reconcileDelete(ctx context.Context, policy *cfztv1alpha1.CloudflareAccessPolicy) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(policy, naming.Finalizer) {
		return ctrl.Result{}, nil
	}
	references, err := r.referencedBy(ctx, policy.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(references) > 0 {
		policy.Status.ReferencedBy = references
		policy.Status.ReferencedByCount = int32(len(references))
		setCondition(&policy.Status.Conditions, ConditionReady, metav1.ConditionFalse, ReasonBlockedByExposures, "CloudflareAccessPolicy still has referencing CloudflareExposures", policy.Generation)
		setCondition(&policy.Status.Conditions, ConditionProgressing, metav1.ConditionTrue, ReasonBlockedByExposures, "waiting for CloudflareExposures to stop referencing this policy", policy.Generation)
		if err := r.Status().Update(ctx, policy); err != nil {
			return ctrl.Result{}, err
		}
		r.Recorder.Eventf(policy, corev1.EventTypeWarning, EventBlockedByExposures, "Deletion blocked by %d CloudflareExposure resources", len(references))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if policy.Status.PolicyId != "" {
		cfClient, err := r.CloudflareClient(ctx, credentialsRefFromAccessPolicy(policy))
		if err != nil {
			if statusErr := r.setPolicyStatus(ctx, policy, policy.Status.PolicyId, policy.Status.ObservedRulesHash, references, false, ReasonCredentialsMissing, err.Error()); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		cfPolicy, err := cfClient.AccessPolicies().GetMetadata(ctx, policy.Status.PolicyId)
		if err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
			return ctrl.Result{}, err
		}
		if err == nil && cfPolicy.Name != desiredPolicyName(policy) {
			setCondition(&policy.Status.Conditions, ConditionReady, metav1.ConditionFalse, ReasonForeignPolicy, "tracked Cloudflare Access policy name does not match desired policy name", policy.Generation)
			setCondition(&policy.Status.Conditions, ConditionProgressing, metav1.ConditionTrue, ReasonForeignPolicy, "deletion blocked to avoid deleting a foreign Cloudflare Access policy", policy.Generation)
			return ctrl.Result{}, r.Status().Update(ctx, policy)
		}
		err = cfClient.AccessPolicies().Delete(ctx, policy.Status.PolicyId)
		if err != nil && !errors.Is(err, cloudflare.ErrNotFound) {
			return ctrl.Result{}, err
		}
		if err == nil {
			r.Recorder.Eventf(policy, corev1.EventTypeNormal, EventDeletedAccessPolicy, "Deleted Cloudflare Access policy %s", policy.Status.PolicyId)
		}
	}
	controllerutil.RemoveFinalizer(policy, naming.Finalizer)
	return ctrl.Result{}, r.Update(ctx, policy)
}

func (r *CloudflareAccessPolicyReconciler) setPolicyStatus(ctx context.Context, policy *cfztv1alpha1.CloudflareAccessPolicy, policyID, observedRulesHash string, referencedBy []cfztv1alpha1.ReferencedExposure, ready bool, reason, message string) error {
	latest := &cfztv1alpha1.CloudflareAccessPolicy{}
	if err := r.Get(ctx, types.NamespacedName{Name: policy.Name}, latest); err != nil {
		return err
	}
	return r.setReady(ctx, latest, &latest.Status.Conditions, latest.Generation, ready, reason, message, func() {
		latest.Status.PolicyId = policyID
		latest.Status.ObservedRulesHash = observedRulesHash
		if referencedBy != nil {
			latest.Status.ReferencedBy = referencedBy
			latest.Status.ReferencedByCount = int32(len(referencedBy))
		}
	})
}

// SetupWithManager wires the controller.
func (r *CloudflareAccessPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// MaxConcurrentReconciles=1 per AGENTS.md ## Code Rules (mirror Tunnel + Exposure).
	return ctrl.NewControllerManagedBy(mgr).
		For(&cfztv1alpha1.CloudflareAccessPolicy{}).
		Watches(&cfztv1alpha1.CloudflareExposure{}, handler.EnqueueRequestsFromMapFunc(enqueueNamed(func(exposure *cfztv1alpha1.CloudflareExposure) []types.NamespacedName {
			names := exposurePolicyRefNames(exposure)
			requests := make([]types.NamespacedName, 0, len(names))
			for _, name := range names {
				requests = append(requests, types.NamespacedName{Name: name})
			}
			return requests
		}))).
		Named("cloudflareaccesspolicy").
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

func desiredPolicyName(policy *cfztv1alpha1.CloudflareAccessPolicy) string {
	base := policy.Name
	if policy.Spec.PolicyName != "" {
		base = policy.Spec.PolicyName
	}
	return base + "-cfzt"
}

func buildAccessPolicyInput(policy *cfztv1alpha1.CloudflareAccessPolicy, name string) cloudflare.AccessPolicyInput {
	return cloudflare.AccessPolicyInput{
		Name:                         name,
		Decision:                     policy.Spec.Decision,
		Include:                      translateRules(policy.Spec.Rules.Include),
		Exclude:                      translateRules(policy.Spec.Rules.Exclude),
		Require:                      translateRules(policy.Spec.Rules.Require),
		SessionDuration:              policy.Spec.SessionDuration,
		PurposeJustificationRequired: policy.Spec.PurposeJustification.Required,
		PurposeJustificationPrompt:   policy.Spec.PurposeJustification.Prompt,
	}
}

func accessPolicyMatches(cfPolicy *cloudflare.AccessPolicy, policy *cfztv1alpha1.CloudflareAccessPolicy, name string) bool {
	if cfPolicy.Name != name ||
		cfPolicy.Decision != policy.Spec.Decision ||
		cfPolicy.SessionDuration != policy.Spec.SessionDuration ||
		cfPolicy.PurposeJustificationRequired != policy.Spec.PurposeJustification.Required ||
		cfPolicy.PurposeJustificationPrompt != policy.Spec.PurposeJustification.Prompt {
		return false
	}
	return cloudflareRulesEqual(cfPolicy.Include, translateRules(policy.Spec.Rules.Include)) &&
		cloudflareRulesEqual(cfPolicy.Exclude, translateRules(policy.Spec.Rules.Exclude)) &&
		cloudflareRulesEqual(cfPolicy.Require, translateRules(policy.Spec.Rules.Require))
}

func cloudflareRulesEqual(a, b []cloudflare.AccessRule) bool {
	ac := canonicalize(a)
	bc := canonicalize(b)
	return equality.Semantic.DeepEqual(ac, bc)
}

func (r *CloudflareAccessPolicyReconciler) referencedBy(ctx context.Context, policyName string) ([]cfztv1alpha1.ReferencedExposure, error) {
	exposures, err := listExposuresByPolicy(ctx, r.Client, policyName)
	if err != nil {
		return nil, err
	}
	refs := make([]cfztv1alpha1.ReferencedExposure, 0)
	for _, exposure := range exposures {
		refs = append(refs, cfztv1alpha1.ReferencedExposure{
			Namespace: exposure.Namespace,
			Name:      exposure.Name,
			Uid:       exposure.UID,
		})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Namespace != refs[j].Namespace {
			return refs[i].Namespace < refs[j].Namespace
		}
		return refs[i].Name < refs[j].Name
	})
	return refs, nil
}
