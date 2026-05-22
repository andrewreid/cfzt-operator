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

// AccessPolicyCredentialsSecretRef identifies the Secret holding Cloudflare
// credentials for a cluster-scoped CloudflareAccessPolicy. Distinct from
// CredentialsSecretRef because the policy CR is cluster-scoped and therefore
// must carry the Secret namespace explicitly.
type AccessPolicyCredentialsSecretRef struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Namespace string `json:"namespace"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:default={}
	Keys CredentialsSecretKeys `json:"keys,omitempty"`
}

// AccessRule is a discriminated union; exactly one field must be set per item.
//
// +kubebuilder:validation:XValidation:rule="[has(self.email), has(self.emailDomain), has(self.ip), has(self.everyone) && self.everyone, has(self.serviceToken), has(self.geoCountryCode)].filter(b, b).size() == 1",message="each rule item must set exactly one of email, emailDomain, ip, everyone, serviceToken, geoCountryCode"
type AccessRule struct {
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=320
	Email string `json:"email,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=253
	EmailDomain string `json:"emailDomain,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=43
	IP string `json:"ip,omitempty"`

	// +kubebuilder:validation:Optional
	Everyone bool `json:"everyone,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=36
	ServiceToken string `json:"serviceToken,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=2
	GeoCountryCode string `json:"geoCountryCode,omitempty"`
}

// AccessRules groups the three matching lists. Semantics:
//   - include: any-of match
//   - exclude: none-of match
//   - require: all-of match
type AccessRules struct {
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:default={}
	Include []AccessRule `json:"include"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:default={}
	Exclude []AccessRule `json:"exclude"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:default={}
	Require []AccessRule `json:"require"`
}

// PurposeJustification configures Cloudflare Access purpose justification.
type PurposeJustification struct {
	// +kubebuilder:validation:Optional
	// +kubebuilder:default=false
	Required bool `json:"required"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=1000
	Prompt string `json:"prompt,omitempty"`
}

// CloudflareAccessPolicySpec defines the desired state of CloudflareAccessPolicy.
//
// +kubebuilder:validation:XValidation:rule="size(self.rules.include) + size(self.rules.exclude) + size(self.rules.require) >= 1",message="rules must contain at least one item across include, exclude, or require"
// +kubebuilder:validation:XValidation:rule="has(self.policyName) == has(oldSelf.policyName) && (!has(self.policyName) || self.policyName == oldSelf.policyName)",message="policyName is immutable"
type CloudflareAccessPolicySpec struct {
	// +kubebuilder:validation:Required
	CredentialsSecretRef AccessPolicyCredentialsSecretRef `json:"credentialsSecretRef"`

	// PolicyName is the base name for the Cloudflare Access policy. The
	// controller always appends "-cfzt"; when empty the base defaults to
	// metadata.name.
	//
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=120
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._ -]+$`
	PolicyName string `json:"policyName,omitempty"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=allow;deny;bypass;non_identity
	Decision string `json:"decision"`

	// +kubebuilder:validation:Required
	Rules AccessRules `json:"rules"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(s|m|h|d|w|mo|y)$`
	SessionDuration string `json:"sessionDuration,omitempty"`

	// +kubebuilder:validation:Optional
	// +kubebuilder:default={}
	PurposeJustification PurposeJustification `json:"purposeJustification,omitempty"`
}

// ReferencedExposure identifies one CloudflareExposure that references this policy.
type ReferencedExposure struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Uid       types.UID `json:"uid"`
}

// CloudflareAccessPolicyStatus defines the observed state of CloudflareAccessPolicy.
type CloudflareAccessPolicyStatus struct {
	PolicyId          string `json:"policyId,omitempty"`
	ObservedRulesHash string `json:"observedRulesHash,omitempty"`

	// +listType=map
	// +listMapKey=uid
	// +optional
	ReferencedBy []ReferencedExposure `json:"referencedBy,omitempty"`

	ReferencedByCount int32 `json:"referencedByCount,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=cfap
// +kubebuilder:printcolumn:name=Decision,type=string,JSONPath=`.spec.decision`
// +kubebuilder:printcolumn:name=PolicyID,type=string,JSONPath=`.status.policyId`
// +kubebuilder:printcolumn:name=RefBy,type=integer,JSONPath=`.status.referencedByCount`
// +kubebuilder:printcolumn:name=Ready,type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name=Age,type=date,JSONPath=`.metadata.creationTimestamp`

// CloudflareAccessPolicy is the Schema for the cloudflareaccesspolicies API.
type CloudflareAccessPolicy struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec CloudflareAccessPolicySpec `json:"spec"`

	// +optional
	Status CloudflareAccessPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CloudflareAccessPolicyList contains a list of CloudflareAccessPolicy.
type CloudflareAccessPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []CloudflareAccessPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CloudflareAccessPolicy{}, &CloudflareAccessPolicyList{})
}
