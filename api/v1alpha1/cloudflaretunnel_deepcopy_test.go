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

package v1alpha1_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
)

func TestCloudflareTunnelDeepCopy(t *testing.T) {
	original := &cfztv1alpha1.CloudflareTunnel{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "cfzt.reid.ee/v1alpha1",
			Kind:       "CloudflareTunnel",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "homelab",
		},
		Spec: cfztv1alpha1.CloudflareTunnelSpec{
			CredentialsSecretRef: cfztv1alpha1.CredentialsSecretRef{
				Name: "cloudflare-credentials",
				Keys: cfztv1alpha1.CredentialsSecretKeys{
					AccountId: "accountId",
					ApiToken:  "apiToken",
				},
			},
			TunnelName: "homelab-rke2",
			Dns: cfztv1alpha1.DnsSpec{
				Manage: true,
			},
			Cloudflared: cfztv1alpha1.CloudflaredSpec{
				Namespace:   "cfzt-system",
				Image:       "cloudflare/cloudflared:2025.1.0",
				HostNetwork: false,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("10m"),
					},
				},
				NodeSelector: map[string]string{"node": "worker"},
				Tolerations: []corev1.Toleration{
					{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists},
				},
			},
		},
		Status: cfztv1alpha1.CloudflareTunnelStatus{
			TunnelId: "abc-123",
			TokenSecretRef: cfztv1alpha1.TokenSecretRef{
				Name: "homelab-token",
			},
			DnsMode: "managed",
			Routes: []cfztv1alpha1.RouteStatus{
				{
					ExposureUid:   types.UID("uid-1"),
					Namespace:     "media",
					Name:          "jellyfin",
					Hostname:      "jellyfin.reid.ee",
					Hash:          "sha256:abcdef",
					LastWrittenAt: metav1.Now(),
				},
			},
			Conditions: []metav1.Condition{
				{
					Type:   "Ready",
					Status: metav1.ConditionTrue,
					Reason: "Reconciled",
				},
			},
		},
	}

	copied := original.DeepCopy()

	if copied == original {
		t.Fatal("DeepCopy returned same pointer")
	}

	if copied.Spec.TunnelName != original.Spec.TunnelName {
		t.Errorf("TunnelName mismatch: got %q want %q", copied.Spec.TunnelName, original.Spec.TunnelName)
	}

	if copied.Spec.CredentialsSecretRef.Name != original.Spec.CredentialsSecretRef.Name {
		t.Errorf("CredentialsSecretRef.Name mismatch")
	}

	if copied.Spec.Cloudflared.Image != original.Spec.Cloudflared.Image {
		t.Errorf("Cloudflared.Image mismatch")
	}

	if len(copied.Spec.Cloudflared.NodeSelector) != len(original.Spec.Cloudflared.NodeSelector) {
		t.Errorf("NodeSelector length mismatch")
	}

	// Mutation isolation: changing the copy must not affect the original.
	copied.Spec.Cloudflared.NodeSelector["node"] = "mutated"
	if original.Spec.Cloudflared.NodeSelector["node"] == "mutated" {
		t.Error("NodeSelector map was not deep copied; mutation leaked to original")
	}

	if len(copied.Status.Routes) != 1 {
		t.Errorf("Routes length mismatch: got %d want 1", len(copied.Status.Routes))
	}

	if copied.Status.Routes[0].Hostname != "jellyfin.reid.ee" {
		t.Errorf("Route hostname mismatch")
	}

	if len(copied.Status.Conditions) != 1 {
		t.Errorf("Conditions length mismatch")
	}
}

func TestCloudflareTunnelRouteDeepCopy(t *testing.T) {
	original := &cfztv1alpha1.CloudflareTunnelRoute{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "cfzt.reid.ee/v1alpha1",
			Kind:       "CloudflareTunnelRoute",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "lan"},
		Spec: cfztv1alpha1.CloudflareTunnelRouteSpec{
			TunnelRef:        cfztv1alpha1.TunnelRouteTunnelRef{Name: "homelab"},
			Network:          "172.16.0.0/24",
			VirtualNetworkId: "00000000-0000-4000-8000-000000000001",
			Comment:          "LAN",
		},
		Status: cfztv1alpha1.CloudflareTunnelRouteStatus{
			RouteId:          "route-1",
			VirtualNetworkId: "00000000-0000-4000-8000-000000000001",
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Reconciled"},
			},
		},
	}

	copied := original.DeepCopy()
	if copied == original {
		t.Fatal("DeepCopy returned same pointer")
	}
	if copied.Spec.Network != original.Spec.Network {
		t.Errorf("Network mismatch: got %q want %q", copied.Spec.Network, original.Spec.Network)
	}
	copied.Status.Conditions[0].Reason = "Changed"
	if original.Status.Conditions[0].Reason == "Changed" {
		t.Error("Conditions slice was not deep copied; mutation leaked to original")
	}
}
