package origin

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
)

const (
	DefaultProtocol = "http"
	ServiceKind     = "Service"
)

// FromService fills missing origin fields from a single-port Service.
func FromService(exposure *cfztv1alpha1.CloudflareExposure, svc *corev1.Service) (*cfztv1alpha1.OriginSpec, error) {
	current := exposure.Spec.Origin
	protocol := DefaultProtocol
	if current != nil && current.Protocol != "" {
		protocol = current.Protocol
	}
	host := serviceDNSName(svc)
	if current != nil && current.Host != "" {
		host = current.Host
	}
	port := int32(0)
	if current != nil {
		port = current.Port
	}
	if port == 0 {
		if len(svc.Spec.Ports) != 1 {
			return nil, fmt.Errorf("service %s/%s has %d ports; origin.port is required when a Service has zero or multiple ports", svc.Namespace, svc.Name, len(svc.Spec.Ports))
		}
		port = svc.Spec.Ports[0].Port
	}
	return &cfztv1alpha1.OriginSpec{Protocol: protocol, Host: host, Port: port}, nil
}

func serviceDNSName(svc *corev1.Service) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace)
}
