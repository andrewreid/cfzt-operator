package controller

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
)

const (
	exposureIndexTunnelRefName = "spec.tunnelRef.name"
	exposureIndexHostname      = "spec.hostname"
	exposureIndexPolicyRefName = "spec.access.applications.policies.policyRef.name"
	exposureIndexFailoverGroup = "spec.failover.group"
	tunnelRouteIndexTunnelRef  = "spec.tunnelRef.name"
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
	if err := indexer.IndexField(ctx, &cfztv1alpha1.CloudflareExposure{}, exposureIndexPolicyRefName, func(obj client.Object) []string {
		exposure := obj.(*cfztv1alpha1.CloudflareExposure)
		return exposurePolicyRefNames(exposure)
	}); err != nil {
		return err
	}
	return indexer.IndexField(ctx, &cfztv1alpha1.CloudflareExposure{}, exposureIndexFailoverGroup, func(obj client.Object) []string {
		exposure := obj.(*cfztv1alpha1.CloudflareExposure)
		if exposure.Spec.Failover == nil || exposure.Spec.Failover.Group == "" {
			return nil
		}
		return []string{exposure.Spec.Failover.Group}
	})
}

func indexCloudflareTunnelRouteFields(ctx context.Context, mgr ctrl.Manager) error {
	return mgr.GetFieldIndexer().IndexField(ctx, &cfztv1alpha1.CloudflareTunnelRoute{}, tunnelRouteIndexTunnelRef, func(obj client.Object) []string {
		route := obj.(*cfztv1alpha1.CloudflareTunnelRoute)
		if route.Spec.TunnelRef.Name == "" {
			return nil
		}
		return []string{route.Spec.TunnelRef.Name}
	})
}

func listCloudflareTunnelRoutesByField(ctx context.Context, c client.Client, field, value string) ([]cfztv1alpha1.CloudflareTunnelRoute, error) {
	var list cfztv1alpha1.CloudflareTunnelRouteList
	if err := c.List(ctx, &list, client.MatchingFields{field: value}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func listCloudflareExposuresByField(ctx context.Context, c client.Client, field, value string) ([]cfztv1alpha1.CloudflareExposure, error) {
	var list cfztv1alpha1.CloudflareExposureList
	if err := c.List(ctx, &list, client.MatchingFields{field: value}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func listExposuresByTunnel(ctx context.Context, c client.Client, tunnelName string) ([]cfztv1alpha1.CloudflareExposure, error) {
	return listCloudflareExposuresByField(ctx, c, exposureIndexTunnelRefName, tunnelName)
}

func listExposuresByHostname(ctx context.Context, c client.Client, hostname string) ([]cfztv1alpha1.CloudflareExposure, error) {
	return listCloudflareExposuresByField(ctx, c, exposureIndexHostname, hostname)
}

func listExposuresByPolicy(ctx context.Context, c client.Client, policyName string) ([]cfztv1alpha1.CloudflareExposure, error) {
	return listCloudflareExposuresByField(ctx, c, exposureIndexPolicyRefName, policyName)
}

func listExposuresByFailoverGroup(ctx context.Context, c client.Client, group string) ([]cfztv1alpha1.CloudflareExposure, error) {
	return listCloudflareExposuresByField(ctx, c, exposureIndexFailoverGroup, group)
}

func listRoutesByTunnel(ctx context.Context, c client.Client, tunnelName string) ([]cfztv1alpha1.CloudflareTunnelRoute, error) {
	return listCloudflareTunnelRoutesByField(ctx, c, tunnelRouteIndexTunnelRef, tunnelName)
}

func exposurePolicyRefNames(exposure *cfztv1alpha1.CloudflareExposure) []string {
	names := make(map[string]struct{})
	for _, app := range exposure.Spec.Access.Applications {
		for _, policy := range app.Policies {
			if policy.PolicyRef.Name == "" {
				continue
			}
			names[policy.PolicyRef.Name] = struct{}{}
		}
	}
	if len(names) == 0 {
		return nil
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	return out
}
