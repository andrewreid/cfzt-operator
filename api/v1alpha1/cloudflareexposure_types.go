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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TunnelRef identifies the CloudflareTunnel that publishes this exposure.
type TunnelRef struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// SourceRef identifies a same-namespace source object used for defaulting.
type SourceRef struct {
	// +kubebuilder:validation:Optional
	ApiVersion string `json:"apiVersion,omitempty"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Service;HTTPRoute
	// +kubebuilder:validation:MinLength=1
	Kind string `json:"kind"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// OriginSpec describes the origin cloudflared should proxy to.
type OriginSpec struct {
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=http;https
	Protocol string `json:"protocol,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
}

// AccessPolicyRef points at an existing Cloudflare Access policy.
type AccessPolicyRef struct {
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`
	UUID string `json:"uuid,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name,omitempty"`
}

// AccessSpec controls Cloudflare Access protection.
type AccessSpec struct {
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`

	// +kubebuilder:validation:Optional
	PolicyRef AccessPolicyRef `json:"policyRef,omitempty"`
}

// CloudflareExposureSpec defines the desired state of CloudflareExposure.
//
// +kubebuilder:validation:XValidation:rule="(has(self.sourceRef) && self.sourceRef.kind == 'Service') || (has(self.origin) && has(self.origin.protocol) && has(self.origin.host) && has(self.origin.port))",message="origin protocol, host, and port are required unless sourceRef.kind is Service"
// +kubebuilder:validation:XValidation:rule="has(self.hostname) || (has(self.sourceRef) && self.sourceRef.kind == 'HTTPRoute')",message="hostname is required unless sourceRef.kind is HTTPRoute"
// +kubebuilder:validation:XValidation:rule="!has(self.access) || !self.access.enabled || (has(self.access.policyRef) && ((has(self.access.policyRef.uuid) && size(self.access.policyRef.uuid) > 0) != (has(self.access.policyRef.name) && size(self.access.policyRef.name) > 0)))",message="access.policyRef requires exactly one of uuid or name when access.enabled is true"
type CloudflareExposureSpec struct {
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=120
	DisplayName string `json:"displayName,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`
	Hostname string `json:"hostname,omitempty"`

	// +kubebuilder:validation:Required
	TunnelRef TunnelRef `json:"tunnelRef"`

	// +kubebuilder:validation:Optional
	SourceRef *SourceRef `json:"sourceRef,omitempty"`

	// +kubebuilder:validation:Optional
	Origin *OriginSpec `json:"origin,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:default={}
	Access AccessSpec `json:"access,omitempty"`
}

// ExposureCloudflareStatus records Cloudflare-side resources for one exposure.
type ExposureCloudflareStatus struct {
	AccessApplicationId     string `json:"accessApplicationId,omitempty"`
	PublicHostnameRouteHash string `json:"publicHostnameRouteHash,omitempty"`
	DnsRecordId             string `json:"dnsRecordId,omitempty"`
}

// CloudflareExposureStatus defines the observed state of CloudflareExposure.
type CloudflareExposureStatus struct {
	Cloudflare        ExposureCloudflareStatus `json:"cloudflare,omitempty"`
	ObservedTunnelUid types.UID                `json:"observedTunnelUid,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=cfe
// +kubebuilder:printcolumn:name=Hostname,type=string,JSONPath=`.spec.hostname`
// +kubebuilder:printcolumn:name=Tunnel,type=string,JSONPath=`.spec.tunnelRef.name`
// +kubebuilder:printcolumn:name=Access,type=boolean,JSONPath=`.spec.access.enabled`
// +kubebuilder:printcolumn:name=Ready,type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name=Age,type=date,JSONPath=`.metadata.creationTimestamp`

// CloudflareExposure is the Schema for the cloudflareexposures API.
type CloudflareExposure struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec CloudflareExposureSpec `json:"spec"`

	// +optional
	Status CloudflareExposureStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CloudflareExposureList contains a list of CloudflareExposure.
type CloudflareExposureList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []CloudflareExposure `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CloudflareExposure{}, &CloudflareExposureList{})
}
