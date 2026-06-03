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

// TunnelRef identifies the CloudflareTunnel that publishes this exposure.
type TunnelRef struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// SourceRef identifies a same-namespace source object used for defaulting.
//
// +kubebuilder:validation:XValidation:rule="(self.kind == 'Service' && has(self.apiVersion) && self.apiVersion == 'v1') || (self.kind == 'HTTPRoute' && has(self.apiVersion) && self.apiVersion == 'gateway.networking.k8s.io/v1')",message="sourceRef.apiVersion must be v1 for Service and gateway.networking.k8s.io/v1 for HTTPRoute"
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

func (r AccessPolicyRef) IsZero() bool {
	return r.UUID == "" && r.Name == ""
}

// AccessApplicationPolicyBinding references one policy attached to an Access
// application. The policy ref shape stays shared with the legacy top-level
// access.policyRef field, but only nested bindings are accepted in v1alpha1.
//
// +kubebuilder:validation:XValidation:rule="(has(self.policyRef.uuid) && size(self.policyRef.uuid) > 0) != (has(self.policyRef.name) && size(self.policyRef.name) > 0)",message="policyRef requires exactly one of uuid or name"
type AccessApplicationPolicyBinding struct {
	// +kubebuilder:validation:Required
	PolicyRef AccessPolicyRef `json:"policyRef"`
}

// AccessApplicationDomain is one canonical self-hosted domain target for an
// Access application.
//
// +kubebuilder:validation:MaxLength=253
// +kubebuilder:validation:Pattern=`^[^\s?#:]+(?:/[^\s?#:]*)?$`
type AccessApplicationDomain string

// AccessApplicationTarget describes one Cloudflare Access self-hosted
// application owned by a CloudflareExposure.
type AccessApplicationTarget struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Domains is the ordered list of self-hosted domain targets for this Access
	// application. The first entry is the Cloudflare primary domain.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=3
	// +listType=atomic
	Domains []AccessApplicationDomain `json:"domains"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=3
	Policies []AccessApplicationPolicyBinding `json:"policies"`
}

// AccessSpec controls Cloudflare Access protection.
type AccessSpec struct {
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`

	// +kubebuilder:validation:Optional
	PolicyRef *AccessPolicyRef `json:"policyRef,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=3
	// +listType=map
	// +listMapKey=name
	Applications []AccessApplicationTarget `json:"applications,omitempty"`
}

// FailoverSpec opts a CloudflareExposure into D26 active-passive multi-cluster
// DR. Two Exposures applied to two clusters with matching spec.failover.group
// and distinct --site-id cooperate over one hostname via a Cloudflare DNS TXT
// lease record; exactly one cluster is Primary and writes the shared CNAME +
// Access app, the other warms its tunnel and waits.
type FailoverSpec struct {
	// Group is the cross-cluster identity for this logical exposure. It is
	// explicitly user-supplied (not derived from hostname) so renaming a
	// hostname does not silently break the failover relationship. RFC 1123
	// label, min 3 / max 63 chars.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Group string `json:"group"`

	// LeaseSeconds is the TTL of the DNS TXT lease record. The Primary
	// renews at leaseSeconds/2. Default 60s; min 30s (Cloudflare DNS TTL
	// floor + safe renewer headroom); max 600s (caps split-brain window).
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=600
	LeaseSeconds int32 `json:"leaseSeconds,omitempty"`
}

// CloudflareExposureSpec defines the desired state of CloudflareExposure.
//
// +kubebuilder:validation:XValidation:rule="(has(self.sourceRef) && self.sourceRef.kind == 'Service') || (has(self.origin) && has(self.origin.protocol) && has(self.origin.host) && has(self.origin.port))",message="origin protocol, host, and port are required unless sourceRef.kind is Service"
// +kubebuilder:validation:XValidation:rule="has(self.hostname) || (has(self.sourceRef) && self.sourceRef.kind == 'HTTPRoute' && (!has(self.access) || !self.access.enabled))",message="hostname is required unless sourceRef.kind is HTTPRoute and access is disabled"
// +kubebuilder:validation:XValidation:rule="!has(self.access) || !self.access.enabled || (has(self.hostname) && size(self.hostname) > 0 && has(self.access.applications) && size(self.access.applications) > 0)",message="access.enabled=true requires spec.hostname and access.applications[]"
// +kubebuilder:validation:XValidation:rule="!has(self.access) || !has(self.access.policyRef)",message="access.policyRef was removed in v1alpha1; use access.applications[]"
// +kubebuilder:validation:XValidation:rule="!has(self.access) || !self.access.enabled || self.access.applications.all(app, app.domains.all(domain, domain == self.hostname || domain.startsWith(self.hostname + '/')))",message="access.applications[].domains must equal spec.hostname or start with spec.hostname + \"/\""
// +kubebuilder:validation:XValidation:rule="has(oldSelf.hostname) ? (has(self.hostname) && self.hostname == oldSelf.hostname) : (!has(self.hostname) || (has(self.sourceRef) && self.sourceRef.kind == 'HTTPRoute'))",message="hostname is immutable except when initially derived from an HTTPRoute sourceRef"
// +kubebuilder:validation:XValidation:rule="self.tunnelRef.name == oldSelf.tunnelRef.name",message="tunnelRef.name is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.sourceRef) == has(oldSelf.sourceRef) && (!has(self.sourceRef) || self.sourceRef == oldSelf.sourceRef)",message="sourceRef is immutable"
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

	// +kubebuilder:validation:Optional
	Failover *FailoverSpec `json:"failover,omitempty"`
}

// ExposureCloudflareStatus records Cloudflare-side resources for one exposure.
type ExposureCloudflareStatus struct {
	// +listType=map
	// +listMapKey=name
	// +optional
	AccessApplications []ExposureAccessApplicationStatus `json:"accessApplications,omitempty"`

	PublicHostnameRouteHash string `json:"publicHostnameRouteHash,omitempty"`
	DnsRecordId             string `json:"dnsRecordId,omitempty"`
}

// ExposureAccessApplicationStatus records one reconciled Access application for
// a CloudflareExposure.
type ExposureAccessApplicationStatus struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +kubebuilder:validation:Optional
	AppID string `json:"appId,omitempty"`

	// +kubebuilder:validation:Optional
	CanonicalDomainHash string `json:"canonicalDomainHash,omitempty"`

	// +kubebuilder:validation:Optional
	PolicyHash string `json:"policyHash,omitempty"`
}

// ExposureFailoverStatus records this site's view of the D26 failover lease
// for one Exposure. Empty when spec.failover is unset.
type ExposureFailoverStatus struct {
	// Role is this site's current failover role.
	// +kubebuilder:validation:Enum=Unknown;Standby;Primary
	Role string `json:"role,omitempty"`

	// SiteID is this operator process's --site-id, recorded on every
	// status write so external operators can correlate role + identity.
	SiteID string `json:"siteId,omitempty"`

	// LeaseOwner is the site-id last observed holding the lease (may be
	// this site or a peer).
	LeaseOwner string `json:"leaseOwner,omitempty"`

	// LeaseExpiresAt is the lease expiry read from / written to the
	// lease TXT record.
	LeaseExpiresAt *metav1.Time `json:"leaseExpiresAt,omitempty"`

	// LeaseRenewedAt is the last successful renewal by this site as
	// Primary; empty on Standby.
	LeaseRenewedAt *metav1.Time `json:"leaseRenewedAt,omitempty"`

	// LastRoleTransitionAt records the most recent Role change observed
	// by this site.
	LastRoleTransitionAt *metav1.Time `json:"lastRoleTransitionAt,omitempty"`

	// ObservedPrimaryTunnelID is the Cloudflare tunnel ID the current
	// Primary published in the lease record (split-brain diagnostic).
	ObservedPrimaryTunnelID string `json:"observedPrimaryTunnelId,omitempty"`

	// LastForcePromoteToken is the most recent cfzt.reid.ee/force-promote
	// annotation value this site has honored. A force-promote fires only
	// when the annotation token differs from this, so a GitOps re-apply of
	// the same token does not replay the emergency promotion (D26).
	LastForcePromoteToken string `json:"lastForcePromoteToken,omitempty"`
}

// CloudflareExposureStatus defines the observed state of CloudflareExposure.
type CloudflareExposureStatus struct {
	Cloudflare ExposureCloudflareStatus `json:"cloudflare,omitempty"`

	// Failover is this site's view of the D26 failover lease. Empty
	// (zero-valued) when spec.failover is unset.
	Failover ExposureFailoverStatus `json:"failover,omitempty"`

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
// +kubebuilder:printcolumn:name=Role,type=string,JSONPath=`.status.failover.role`
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
