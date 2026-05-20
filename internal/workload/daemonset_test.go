package workload

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/naming"
)

func TestDaemonSetSpecMatchesSpec(t *testing.T) {
	tunnel := testTunnel()
	tunnel.Spec.Cloudflared.Image = "cloudflare/cloudflared:2025.2.0"
	tunnel.Spec.Cloudflared.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{"cpu": resource.MustParse("10m")},
	}

	ds := DaemonSet(tunnel, "token-1")

	if ds.Name != naming.DaemonSetName(tunnel.Name) {
		t.Fatalf("DaemonSet name = %q", ds.Name)
	}
	if ds.Spec.UpdateStrategy.Type != appsv1.RollingUpdateDaemonSetStrategyType {
		t.Fatalf("update strategy = %q", ds.Spec.UpdateStrategy.Type)
	}
	if got := ds.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable.IntVal; got != 1 {
		t.Fatalf("maxUnavailable = %d", got)
	}
	container := ds.Spec.Template.Spec.Containers[0]
	if container.Name != "cloudflared" || container.Image != tunnel.Spec.Cloudflared.Image {
		t.Fatalf("container = %#v", container)
	}
	if got := container.Env[0].ValueFrom.SecretKeyRef.Name; got != naming.TokenSecretName(tunnel.Name) {
		t.Fatalf("token secret ref = %q", got)
	}
	if got := container.Env[0].ValueFrom.SecretKeyRef.Key; got != naming.TokenSecretKey {
		t.Fatalf("token secret key = %q", got)
	}
	if container.SecurityContext == nil || len(container.SecurityContext.Capabilities.Drop) != 1 || container.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("missing hardened security context")
	}
	if ds.Spec.Template.Spec.AutomountServiceAccountToken == nil || *ds.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatalf("automountServiceAccountToken = %#v, want false", ds.Spec.Template.Spec.AutomountServiceAccountToken)
	}
	if ds.Spec.Template.Spec.SecurityContext == nil || ds.Spec.Template.Spec.SecurityContext.SeccompProfile == nil || ds.Spec.Template.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("missing RuntimeDefault seccomp profile")
	}
	if container.ReadinessProbe.PeriodSeconds != 5 {
		t.Fatalf("readiness period = %d, want 5", container.ReadinessProbe.PeriodSeconds)
	}
	if container.ReadinessProbe.InitialDelaySeconds != 0 {
		t.Fatalf("readiness initial delay = %d, want 0", container.ReadinessProbe.InitialDelaySeconds)
	}
	if container.LivenessProbe.PeriodSeconds != 10 {
		t.Fatalf("liveness period = %d, want 10", container.LivenessProbe.PeriodSeconds)
	}
}

func TestTokenChecksumAnnotation(t *testing.T) {
	tunnel := testTunnel()

	ds := DaemonSet(tunnel, "token-1")
	got := ds.Spec.Template.Annotations[TokenChecksumAnnotation]
	if got != TokenChecksum("token-1") {
		t.Fatalf("checksum = %q, want %q", got, TokenChecksum("token-1"))
	}

	ApplyDaemonSet(ds, tunnel, "token-2")
	got = ds.Spec.Template.Annotations[TokenChecksumAnnotation]
	if got != TokenChecksum("token-2") {
		t.Fatalf("rotated checksum = %q, want %q", got, TokenChecksum("token-2"))
	}
}

func TestHostNetworkPropagates(t *testing.T) {
	tunnel := testTunnel()
	tunnel.Spec.Cloudflared.HostNetwork = true

	ds := DaemonSet(tunnel, "token")

	if !ds.Spec.Template.Spec.HostNetwork {
		t.Fatalf("hostNetwork not propagated")
	}
	if ds.Spec.Template.Spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
		t.Fatalf("dnsPolicy = %q, want %q", ds.Spec.Template.Spec.DNSPolicy, corev1.DNSClusterFirstWithHostNet)
	}
}

func testTunnel() *cfztv1alpha1.CloudflareTunnel {
	return &cfztv1alpha1.CloudflareTunnel{
		ObjectMeta: metav1.ObjectMeta{Name: "homelab"},
		Spec: cfztv1alpha1.CloudflareTunnelSpec{
			TunnelName: "homelab-rke2",
			Cloudflared: cfztv1alpha1.CloudflaredSpec{
				Namespace: "cfzt-system",
			},
		},
	}
}
