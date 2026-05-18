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

// MatchType specifies how a queue pattern is matched against a queue path.
// +kubebuilder:validation:Enum=Exact;Prefix
type MatchType string

const (
	MatchExact  MatchType = "Exact"
	MatchPrefix MatchType = "Prefix"
)

// LabelMatcher specifies a set of required label key/value pairs.
// A caller satisfies this matcher only if it carries all specified labels (AND semantics).
// Label keys and values must follow k8s label conventions.
type LabelMatcher struct {
	// Labels is the set of key/value pairs that must all be present on the caller.
	// +kubebuilder:validation:MinProperties=1
	Labels map[string]string `json:"labels"`
}

// QueuePattern pairs a queue path pattern with its access policy.
type QueuePattern struct {
	// pattern is the queue path to match, interpreted according to matchType.
	// +kubebuilder:validation:MinLength=1
	Pattern string `json:"pattern"`

	// matchType controls how pattern is applied: Exact or Prefix.
	// Defaults to Exact.
	// +kubebuilder:default=Exact
	MatchType MatchType `json:"matchType,omitempty"`

	// allowedCallers lists the label matchers that grant access to this queue.
	// A caller is permitted if it satisfies at least one matcher (OR semantics).
	// +kubebuilder:validation:MinItems=1
	AllowedCallers []LabelMatcher `json:"allowedCallers"`
}

// EntroQQueueSpec defines the desired state of EntroQQueue.
type EntroQQueueSpec struct {
	// queues is the list of queue patterns and their associated access policies.
	// +kubebuilder:validation:MinItems=1
	Queues []QueuePattern `json:"queues"`
}

// EntroQQueueStatus defines the observed or current state of EntroQQueue.
type EntroQQueueStatus struct {
	// conditions represent the current state of the EntroQQueue resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// EntroQQueue is the Schema for the entroqqueues API.
type EntroQQueue struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec EntroQQueueSpec `json:"spec"`

	// +optional
	Status EntroQQueueStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// EntroQQueueList contains a list of EntroQQueue specs, this is the top-level structure.
type EntroQQueueList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []EntroQQueue `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EntroQQueue{}, &EntroQQueueList{})
}
