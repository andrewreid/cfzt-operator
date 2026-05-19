package tunnelconfig

import (
	"crypto/sha256"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/types"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
)

const CatchAllService = "http_status:404"

type Route struct {
	ExposureUID types.UID
	Namespace   string
	Name        string
	Hostname    string
	Hash        string
}

type Result struct {
	Config cloudflare.TunnelConfiguration
	Routes []Route
}

type HostnameConflictError struct {
	Hostnames []string
}

func (e *HostnameConflictError) Error() string {
	return fmt.Sprintf("hostname conflict: %v", e.Hostnames)
}

func Build(exposures []cfztv1alpha1.CloudflareExposure) (*Result, error) {
	byHost := make(map[string][]cfztv1alpha1.CloudflareExposure)
	for _, exposure := range exposures {
		if exposure.Spec.Hostname == "" {
			continue
		}
		byHost[exposure.Spec.Hostname] = append(byHost[exposure.Spec.Hostname], exposure)
	}
	var conflicts []string
	for hostname, matches := range byHost {
		if len(matches) > 1 {
			conflicts = append(conflicts, hostname)
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return nil, &HostnameConflictError{Hostnames: conflicts}
	}

	ordered := append([]cfztv1alpha1.CloudflareExposure(nil), exposures...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Spec.Hostname == ordered[j].Spec.Hostname {
			if ordered[i].Namespace == ordered[j].Namespace {
				return ordered[i].Name < ordered[j].Name
			}
			return ordered[i].Namespace < ordered[j].Namespace
		}
		return ordered[i].Spec.Hostname < ordered[j].Spec.Hostname
	})

	result := &Result{}
	for _, exposure := range ordered {
		if exposure.Spec.Hostname == "" || exposure.Spec.Origin == nil {
			continue
		}
		service := fmt.Sprintf("%s://%s:%d", exposure.Spec.Origin.Protocol, exposure.Spec.Origin.Host, exposure.Spec.Origin.Port)
		rule := cloudflare.IngressRule{Hostname: exposure.Spec.Hostname, Service: service}
		result.Config.Ingress = append(result.Config.Ingress, rule)
		hash := HashRule(rule)
		result.Routes = append(result.Routes, Route{
			ExposureUID: exposure.UID,
			Namespace:   exposure.Namespace,
			Name:        exposure.Name,
			Hostname:    exposure.Spec.Hostname,
			Hash:        hash,
		})
	}
	result.Config.Ingress = append(result.Config.Ingress, cloudflare.IngressRule{Service: CatchAllService})
	return result, nil
}

func HashRule(rule cloudflare.IngressRule) string {
	sum := sha256.Sum256([]byte(rule.Hostname + "\x00" + rule.Service))
	return fmt.Sprintf("%x", sum)
}
