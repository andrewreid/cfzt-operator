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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// CredentialsSecretKeys configures which keys within the referenced Secret hold credentials.
type CredentialsSecretKeys struct {
	// +kubebuilder:validation:Optional
	// +kubebuilder:default="accountId"
	// +kubebuilder:validation:MaxLength=253
	AccountId string `json:"accountId,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:default="apiToken"
	// +kubebuilder:validation:MaxLength=253
	ApiToken string `json:"apiToken,omitempty"`
}

// CredentialsSecretRef identifies the Secret holding Cloudflare credentials in
// the cloudflared namespace.
type CredentialsSecretRef struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:default={}
	Keys CredentialsSecretKeys `json:"keys,omitempty"`
}

// DnsSpec controls DNS management behaviour.
type DnsSpec struct {
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=true
	Manage bool `json:"manage"`
}

// CloudflaredSpec controls the cloudflared DaemonSet deployed by the operator.
//
// +kubebuilder:validation:XValidation:rule="!has(self.image) || !self.image.endsWith(':latest')",message="cloudflared.image must not use the :latest tag"
type CloudflaredSpec struct {
	// +kubebuilder:validation:Optional
	// +kubebuilder:default="cfzt-system"
	// +kubebuilder:validation:MaxLength=253
	Namespace string `json:"namespace,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9./-]+(:[a-zA-Z0-9._-]+)?$`
	Image string `json:"image,omitempty"`

	// +kubebuilder:validation:Optional
	HostNetwork bool `json:"hostNetwork,omitempty"`

	// +kubebuilder:validation:Optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// +kubebuilder:validation:Optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// +kubebuilder:validation:Optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// +kubebuilder:validation:Optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
}

// CloudflareTunnelSpec defines the desired state of CloudflareTunnel.
//
// +kubebuilder:validation:XValidation:rule="self.tunnelName == oldSelf.tunnelName",message="tunnelName is immutable"
type CloudflareTunnelSpec struct {
	// +kubebuilder:validation:Required
	CredentialsSecretRef CredentialsSecretRef `json:"credentialsSecretRef"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=120
	TunnelName string `json:"tunnelName"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:default={}
	Dns DnsSpec `json:"dns,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:default={}
	Cloudflared CloudflaredSpec `json:"cloudflared,omitempty"`
}

// TokenSecretRef holds the name of the Secret that stores the tunnel token.
type TokenSecretRef struct {
	Name string `json:"name"`
}

// RouteStatus records the last-written ingress rule for one CloudflareExposure.
type RouteStatus struct {
	ExposureUid   types.UID   `json:"exposureUid"`
	Namespace     string      `json:"namespace"`
	Name          string      `json:"name"`
	Hostname      string      `json:"hostname"`
	Hash          string      `json:"hash"`
	LastWrittenAt metav1.Time `json:"lastWrittenAt"`
}

// CloudflareTunnelStatus defines the observed state of CloudflareTunnel.
type CloudflareTunnelStatus struct {
	TunnelId       string         `json:"tunnelId,omitempty"`
	TokenSecretRef TokenSecretRef `json:"tokenSecretRef,omitempty"`
	DnsMode        string         `json:"dnsMode,omitempty"`
	Routes         []RouteStatus  `json:"routes,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=cft
// +kubebuilder:printcolumn:name=Tunnel,type=string,JSONPath=`.spec.tunnelName`
// +kubebuilder:printcolumn:name=TunnelID,type=string,JSONPath=`.status.tunnelId`
// +kubebuilder:printcolumn:name=Ready,type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name=Age,type=date,JSONPath=`.metadata.creationTimestamp`

// CloudflareTunnel is the Schema for the cloudflaretunnels API.
type CloudflareTunnel struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec CloudflareTunnelSpec `json:"spec"`

	// +optional
	Status CloudflareTunnelStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CloudflareTunnelList contains a list of CloudflareTunnel.
type CloudflareTunnelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []CloudflareTunnel `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CloudflareTunnel{}, &CloudflareTunnelList{})
}
