package workload

import (
	"crypto/sha256"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/naming"
)

// TokenChecksumAnnotation rolls cloudflared pods when the tunnel token changes.
const TokenChecksumAnnotation = "cfzt.reid.ee/token-checksum"

// TokenChecksum returns the stable SHA-256 hex digest of the tunnel token.
func TokenChecksum(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum)
}

// TokenSecret returns the operator-managed Secret storing the tunnel token.
func TokenSecret(tunnel *cfztv1alpha1.CloudflareTunnel, token string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.TokenSecretName(tunnel.Name),
			Namespace: tunnel.Spec.Cloudflared.Namespace,
			Labels:    Labels(tunnel.Name),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			naming.TokenSecretKey: []byte(token),
		},
	}
}

// ApplyTokenSecret copies mutable desired Secret fields onto an existing object.
func ApplyTokenSecret(dst *corev1.Secret, tunnel *cfztv1alpha1.CloudflareTunnel, token string) {
	dst.Labels = Labels(tunnel.Name)
	dst.Type = corev1.SecretTypeOpaque
	if dst.Data == nil {
		dst.Data = map[string][]byte{}
	}
	dst.Data[naming.TokenSecretKey] = []byte(token)
	dst.StringData = nil
}
