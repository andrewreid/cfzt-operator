package tunnelconfig

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
)

func TestTunnelConfigBuilderDeterministic(t *testing.T) {
	result, err := Build([]cfztv1alpha1.CloudflareExposure{
		exposure("b", "z.example.com", "z.media.svc.cluster.local", "uid-z"),
		exposure("a", "a.example.com", "a.media.svc.cluster.local", "uid-a"),
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if got, want := len(result.Config.Ingress), 3; got != want {
		t.Fatalf("ingress count = %d, want %d", got, want)
	}
	if got := result.Config.Ingress[0].Hostname; got != "a.example.com" {
		t.Fatalf("first hostname = %q", got)
	}
	if got := result.Config.Ingress[2].Service; got != CatchAllService {
		t.Fatalf("catch-all service = %q", got)
	}
	if result.Routes[0].Hash == "" || result.Routes[0].Hash != HashRule(result.Config.Ingress[0]) {
		t.Fatalf("route hash not stable")
	}
}

func TestTunnelConfigBuilderHostnameCollision(t *testing.T) {
	_, err := Build([]cfztv1alpha1.CloudflareExposure{
		exposure("a", "same.example.com", "a.media.svc.cluster.local", "uid-a"),
		exposure("b", "same.example.com", "b.media.svc.cluster.local", "uid-b"),
	})
	if _, ok := err.(*HostnameConflictError); !ok {
		t.Fatalf("error = %T %v, want HostnameConflictError", err, err)
	}
}

func TestTunnelConfigBuilderEmptyExposureList(t *testing.T) {
	result, err := Build(nil)
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if got, want := len(result.Config.Ingress), 1; got != want {
		t.Fatalf("ingress count = %d, want %d", got, want)
	}
	if got := result.Config.Ingress[0].Service; got != CatchAllService {
		t.Fatalf("catch-all service = %q", got)
	}
}

func TestTunnelConfigBuilderSkipsUnresolvedSourceRef(t *testing.T) {
	unresolved := exposure("route", "", "route.media.svc.cluster.local", "uid-route")
	result, err := Build([]cfztv1alpha1.CloudflareExposure{unresolved})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	if got, want := len(result.Config.Ingress), 1; got != want {
		t.Fatalf("ingress count = %d, want %d", got, want)
	}
	if result.Config.Ingress[0].Hostname != "" || result.Config.Ingress[0].Service != CatchAllService {
		t.Fatalf("unresolved sourceRef produced ingress rule: %#v", result.Config.Ingress[0])
	}
	if len(result.Routes) != 0 {
		t.Fatalf("routes = %#v, want none", result.Routes)
	}
}

func exposure(name, hostname, host, uid string) cfztv1alpha1.CloudflareExposure {
	return cfztv1alpha1.CloudflareExposure{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "media", UID: types.UID(uid)},
		Spec: cfztv1alpha1.CloudflareExposureSpec{
			Hostname:  hostname,
			TunnelRef: cfztv1alpha1.TunnelRef{Name: "homelab"},
			Origin:    &cfztv1alpha1.OriginSpec{Protocol: "http", Host: host, Port: 80},
		},
	}
}
