package controller

import (
	"context"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
)

const (
	exposureIndexTunnelRefName = "spec.tunnelRef.name"
	exposureIndexHostname      = "spec.hostname"
	exposureIndexPolicyRefName = "spec.access.policyRef.name"
)

func indexCloudflareExposureFields(ctx context.Context, mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()
	if err := indexer.IndexField(ctx, &cfztv1alpha1.CloudflareExposure{}, exposureIndexTunnelRefName, func(obj client.Object) []string {
		exposure := obj.(*cfztv1alpha1.CloudflareExposure)
		if exposure.Spec.TunnelRef.Name == "" {
			return nil
		}
		return []string{exposure.Spec.TunnelRef.Name}
	}); err != nil {
		return err
	}
	if err := indexer.IndexField(ctx, &cfztv1alpha1.CloudflareExposure{}, exposureIndexHostname, func(obj client.Object) []string {
		exposure := obj.(*cfztv1alpha1.CloudflareExposure)
		if exposure.Spec.Hostname == "" {
			return nil
		}
		return []string{exposure.Spec.Hostname}
	}); err != nil {
		return err
	}
	return indexer.IndexField(ctx, &cfztv1alpha1.CloudflareExposure{}, exposureIndexPolicyRefName, func(obj client.Object) []string {
		exposure := obj.(*cfztv1alpha1.CloudflareExposure)
		if exposure.Spec.Access.PolicyRef.Name == "" {
			return nil
		}
		return []string{exposure.Spec.Access.PolicyRef.Name}
	})
}

func listCloudflareExposuresByField(ctx context.Context, c client.Client, field, value string, keep func(cfztv1alpha1.CloudflareExposure) bool) ([]cfztv1alpha1.CloudflareExposure, error) {
	var list cfztv1alpha1.CloudflareExposureList
	if err := c.List(ctx, &list, client.MatchingFields{field: value}); err != nil {
		if !isFieldIndexUnavailable(err) {
			return nil, err
		}
		if err := c.List(ctx, &list); err != nil {
			return nil, err
		}
	}
	out := make([]cfztv1alpha1.CloudflareExposure, 0, len(list.Items))
	for _, exposure := range list.Items {
		if keep(exposure) {
			out = append(out, exposure)
		}
	}
	return out, nil
}

func isFieldIndexUnavailable(err error) bool {
	if apierrors.IsBadRequest(err) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "field label not supported") ||
		strings.Contains(msg, "Index with name") ||
		strings.Contains(msg, "does not exist")
}
