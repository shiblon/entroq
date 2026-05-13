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
)

// ServiceAccountLabels pairs a service account name with the mesh label claims
// it presents for authorization decisions.
// Label keys and values must follow k8s label conventions.
type ServiceAccountLabels struct {
	// serviceAccount is the name of the k8s service account.
	// +kubebuilder:validation:MinLength=1
	ServiceAccount string `json:"serviceAccount"`

	// labels is the set of mesh label claims asserted for this service account.
	// +kubebuilder:validation:MinProperties=1
	Labels map[string]string `json:"labels"`
}

// EntroQIdentitySpec defines the desired state of EntroQIdentity.
type EntroQIdentitySpec struct {
	// identities is the list of service account to label claim mappings.
	// +kubebuilder:validation:MinItems=1
	Identities []ServiceAccountLabels `json:"identities"`
}

// EntroQIdentityStatus defines the observed state of EntroQIdentity.
type EntroQIdentityStatus struct {
	// conditions represent the current state of the EntroQIdentity resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// EntroQIdentity is the Schema for the entroqidentities API.
type EntroQIdentity struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec EntroQIdentitySpec `json:"spec"`

	// +optional
	Status EntroQIdentityStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// EntroQIdentityList contains a list of EntroQIdentity.
type EntroQIdentityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []EntroQIdentity `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EntroQIdentity{}, &EntroQIdentityList{})
}
