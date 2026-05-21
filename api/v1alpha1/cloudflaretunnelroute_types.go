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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// TunnelRouteTunnelRef references the CloudflareTunnel that owns the connector.
type TunnelRouteTunnelRef struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// CloudflareTunnelRouteSpec defines the desired state of CloudflareTunnelRoute.
//
// +kubebuilder:validation:XValidation:rule="self.tunnelRef.name == oldSelf.tunnelRef.name",message="tunnelRef.name is immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.virtualNetworkId) || oldSelf.virtualNetworkId == \"\" || (has(self.virtualNetworkId) && self.virtualNetworkId != \"\")",message="virtualNetworkId cannot be cleared once set; delete and recreate the route to return to the account default virtual network"
type CloudflareTunnelRouteSpec struct {
	// +kubebuilder:validation:Required
	TunnelRef TunnelRouteTunnelRef `json:"tunnelRef"`

	// Network is a single IPv4 or IPv6 CIDR routed through the tunnel. CRD
	// validation is intentionally coarse; the controller canonicalizes with
	// net/netip.ParsePrefix and rejects invalid values missed by regex.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(([0-9]{1,3}\.){3}[0-9]{1,3}/([0-9]|[1-2][0-9]|3[0-2])|[0-9a-fA-F:]+/([0-9]|[1-9][0-9]|1[0-1][0-9]|12[0-8]))$`
	Network string `json:"network"`

	// VirtualNetworkId is optional. When empty, Cloudflare applies the account
	// default virtual network and the operator omits virtual_network_id params.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Pattern=`^$|^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`
	VirtualNetworkId string `json:"virtualNetworkId,omitempty"`

	// Comment is optional user text appended after the compact ownership tag.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=34
	Comment string `json:"comment,omitempty"`
}

// CloudflareTunnelRouteStatus defines the observed state of CloudflareTunnelRoute.
type CloudflareTunnelRouteStatus struct {
	RouteId          string `json:"routeId,omitempty"`
	VirtualNetworkId string `json:"virtualNetworkId,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=cftr
// +kubebuilder:printcolumn:name=Network,type=string,JSONPath=`.spec.network`
// +kubebuilder:printcolumn:name=Tunnel,type=string,JSONPath=`.spec.tunnelRef.name`
// +kubebuilder:printcolumn:name=RouteID,type=string,JSONPath=`.status.routeId`
// +kubebuilder:printcolumn:name=Ready,type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name=Age,type=date,JSONPath=`.metadata.creationTimestamp`

// CloudflareTunnelRoute is the Schema for the cloudflaretunnelroutes API.
type CloudflareTunnelRoute struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec CloudflareTunnelRouteSpec `json:"spec"`

	// +optional
	Status CloudflareTunnelRouteStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CloudflareTunnelRouteList contains a list of CloudflareTunnelRoute.
type CloudflareTunnelRouteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []CloudflareTunnelRoute `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CloudflareTunnelRoute{}, &CloudflareTunnelRouteList{})
}
