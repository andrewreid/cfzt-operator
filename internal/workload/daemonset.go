package workload

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	cfztv1alpha1 "github.com/andrewreid/cfzt-operator/api/v1alpha1"
	"github.com/andrewreid/cfzt-operator/internal/naming"
)

const (
	// DefaultCloudflaredImage is pinned by the operator and must not be :latest.
	DefaultCloudflaredImage = "cloudflare/cloudflared:2025.1.0"
)

// Labels returns stable labels for tunnel-owned Kubernetes workload resources.
func Labels(tunnelName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "cloudflared",
		"app.kubernetes.io/component":  "connector",
		"app.kubernetes.io/managed-by": "cfzt-operator",
		"cfzt.reid.ee/tunnel":          tunnelName,
	}
}

// DaemonSet returns the desired cloudflared DaemonSet for a tunnel.
func DaemonSet(tunnel *cfztv1alpha1.CloudflareTunnel, token string) *appsv1.DaemonSet {
	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.DaemonSetName(tunnel.Name),
			Namespace: tunnel.Spec.Cloudflared.Namespace,
			Labels:    Labels(tunnel.Name),
		},
	}
	ApplyDaemonSet(ds, tunnel, token)
	return ds
}

// ApplyDaemonSet copies mutable desired DaemonSet fields onto an existing object.
func ApplyDaemonSet(ds *appsv1.DaemonSet, tunnel *cfztv1alpha1.CloudflareTunnel, token string) {
	labels := Labels(tunnel.Name)
	image := tunnel.Spec.Cloudflared.Image
	if image == "" {
		image = DefaultCloudflaredImage
	}
	maxUnavailable := intstr.FromInt32(1)
	runAsNonRoot := true
	readOnlyRootFilesystem := true
	allowPrivilegeEscalation := false
	runAsUser := int64(65532)
	runAsGroup := int64(65532)
	automountServiceAccountToken := false

	ds.Labels = labels
	ds.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
	ds.Spec.UpdateStrategy = appsv1.DaemonSetUpdateStrategy{
		Type: appsv1.RollingUpdateDaemonSetStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDaemonSet{
			MaxUnavailable: &maxUnavailable,
		},
	}
	ds.Spec.Template = corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: labels,
			Annotations: map[string]string{
				TokenChecksumAnnotation: TokenChecksum(token),
			},
		},
		Spec: corev1.PodSpec{
			HostNetwork:                  tunnel.Spec.Cloudflared.HostNetwork,
			DNSPolicy:                    dnsPolicy(tunnel.Spec.Cloudflared.HostNetwork),
			NodeSelector:                 tunnel.Spec.Cloudflared.NodeSelector,
			Tolerations:                  tunnel.Spec.Cloudflared.Tolerations,
			Affinity:                     tunnel.Spec.Cloudflared.Affinity,
			AutomountServiceAccountToken: &automountServiceAccountToken,
			SecurityContext:              &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers: []corev1.Container{
				{
					Name:  "cloudflared",
					Image: image,
					Args: []string{
						"tunnel",
						"--no-autoupdate",
						"--metrics",
						"0.0.0.0:2000",
						"run",
					},
					Env: []corev1.EnvVar{
						{
							Name: "TUNNEL_TOKEN",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: naming.TokenSecretName(tunnel.Name),
									},
									Key: naming.TokenSecretKey,
								},
							},
						},
					},
					Ports: []corev1.ContainerPort{
						{Name: "metrics", ContainerPort: 2000},
					},
					LivenessProbe:  httpProbe("/ready", 10),
					ReadinessProbe: httpProbe("/ready", 5),
					Resources:      tunnel.Spec.Cloudflared.Resources,
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: &allowPrivilegeEscalation,
						ReadOnlyRootFilesystem:   &readOnlyRootFilesystem,
						RunAsNonRoot:             &runAsNonRoot,
						RunAsUser:                &runAsUser,
						RunAsGroup:               &runAsGroup,
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
					},
				},
			},
		},
	}
}

func dnsPolicy(hostNetwork bool) corev1.DNSPolicy {
	if hostNetwork {
		return corev1.DNSClusterFirstWithHostNet
	}
	return corev1.DNSClusterFirst
}

func httpProbe(path string, periodSeconds int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: path,
				Port: intstr.FromInt32(2000),
			},
		},
		PeriodSeconds:    periodSeconds,
		TimeoutSeconds:   1,
		FailureThreshold: 3,
	}
}
