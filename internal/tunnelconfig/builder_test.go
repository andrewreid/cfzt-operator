package tunnelconfig

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/cloudflare"
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

func TestHashRuleUsesCanonicalJSON(t *testing.T) {
	rule := resultIngressRule("jellyfin.example.com", "http://jellyfin.media.svc.cluster.local:80")

	got := HashRule(rule)
	want := "sha256:4c898e712c692aed20894bb3693c45f6ca5864015b5afd47d64864de6e30678c"
	if got != want {
		t.Fatalf("HashRule = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("HashRule missing sha256 prefix: %q", got)
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

func TestTunnelConfigBuilderConcreteBeforeOverlappingWildcard(t *testing.T) {
	result, err := Build([]cfztv1alpha1.CloudflareExposure{
		exposure("wild", "*.example.com", "wild.media.svc.cluster.local", "uid-wild"),
		exposure("foo", "foo.example.com", "foo.media.svc.cluster.local", "uid-foo"),
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	hosts := ingressHostnames(result)
	wantOrder := []string{"foo.example.com", "*.example.com", ""}
	assertHostOrder(t, hosts, wantOrder)
	assertRoutesAligned(t, result)
}

func TestTunnelConfigBuilderMoreSpecificWildcardFirst(t *testing.T) {
	result, err := Build([]cfztv1alpha1.CloudflareExposure{
		exposure("broad", "*.example.com", "broad.media.svc.cluster.local", "uid-broad"),
		exposure("deep", "*.foo.example.com", "deep.media.svc.cluster.local", "uid-deep"),
		exposure("bar", "bar.foo.example.com", "bar.media.svc.cluster.local", "uid-bar"),
		exposure("foo", "foo.example.com", "foo.media.svc.cluster.local", "uid-foo"),
	})
	if err != nil {
		t.Fatalf("Build returned error: %v", err)
	}
	hosts := ingressHostnames(result)
	// (a) more labels first, (b) concrete before wildcard at equal label count:
	// bar.foo.example.com(4,concrete), *.foo.example.com(4,wild),
	// foo.example.com(3,concrete), *.example.com(3,wild), then catch-all.
	wantOrder := []string{"bar.foo.example.com", "*.foo.example.com", "foo.example.com", "*.example.com", ""}
	assertHostOrder(t, hosts, wantOrder)
	// Key correctness invariants: each concrete host precedes any wildcard that
	// covers it, and the more-specific wildcard precedes the broader one.
	assertBefore(t, hosts, "foo.example.com", "*.example.com")
	assertBefore(t, hosts, "bar.foo.example.com", "*.foo.example.com")
	assertBefore(t, hosts, "*.foo.example.com", "*.example.com")
	assertRoutesAligned(t, result)
}

func TestTunnelConfigBuilderWildcardExactDuplicateConflicts(t *testing.T) {
	_, err := Build([]cfztv1alpha1.CloudflareExposure{
		exposure("a", "*.example.com", "a.media.svc.cluster.local", "uid-a"),
		exposure("b", "*.example.com", "b.media.svc.cluster.local", "uid-b"),
	})
	if _, ok := err.(*HostnameConflictError); !ok {
		t.Fatalf("error = %T %v, want HostnameConflictError", err, err)
	}
}

func ingressHostnames(result *Result) []string {
	hosts := make([]string, 0, len(result.Config.Ingress))
	for _, rule := range result.Config.Ingress {
		hosts = append(hosts, rule.Hostname)
	}
	return hosts
}

func assertHostOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ingress hostnames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ingress hostnames = %v, want %v", got, want)
		}
	}
}

func assertBefore(t *testing.T, hosts []string, first, second string) {
	t.Helper()
	fi, si := indexOf(hosts, first), indexOf(hosts, second)
	if fi < 0 || si < 0 {
		t.Fatalf("missing host: %q@%d %q@%d in %v", first, fi, second, si, hosts)
	}
	if fi >= si {
		t.Fatalf("%q (idx %d) must precede %q (idx %d) in %v", first, fi, second, si, hosts)
	}
}

func assertRoutesAligned(t *testing.T, result *Result) {
	t.Helper()
	// Routes exclude the trailing catch-all rule; they must line up 1:1 with the
	// emitted ingress rules and carry matching hashes.
	if len(result.Routes) != len(result.Config.Ingress)-1 {
		t.Fatalf("routes = %d, ingress (minus catch-all) = %d", len(result.Routes), len(result.Config.Ingress)-1)
	}
	for i, route := range result.Routes {
		rule := result.Config.Ingress[i]
		if route.Hostname != rule.Hostname {
			t.Fatalf("route[%d] hostname %q != ingress[%d] hostname %q", i, route.Hostname, i, rule.Hostname)
		}
		if route.Hash != HashRule(rule) {
			t.Fatalf("route[%d] hash %q != HashRule(ingress[%d])", i, route.Hash, i)
		}
	}
}

func indexOf(hosts []string, host string) int {
	for i, h := range hosts {
		if h == host {
			return i
		}
	}
	return -1
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

func resultIngressRule(hostname, service string) cloudflare.IngressRule {
	return cloudflare.IngressRule{Hostname: hostname, Service: service}
}
