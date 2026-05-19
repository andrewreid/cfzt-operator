package origin

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	HTTPRouteKind       = "HTTPRoute"
	HTTPRouteAPIVersion = "gateway.networking.k8s.io/v1"
)

// HostnameFromHTTPRoute returns the single hostname from a Gateway API HTTPRoute.
func HostnameFromHTTPRoute(route *unstructured.Unstructured) (string, error) {
	hostnames, ok, err := unstructured.NestedStringSlice(route.Object, "spec", "hostnames")
	if err != nil {
		return "", err
	}
	if !ok || len(hostnames) != 1 {
		return "", fmt.Errorf("HTTPRoute %s/%s has %d hostnames; spec.hostname is required unless exactly one HTTPRoute hostname is set", route.GetNamespace(), route.GetName(), len(hostnames))
	}
	return hostnames[0], nil
}
