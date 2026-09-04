// Package meshpolicy defines the authorization data produced by the EntroQ
// Kubernetes operator and consumed by policy engines.
package meshpolicy

import (
	"errors"
	"fmt"
	"strings"

	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

// Document is the complete, atomically replaceable mesh authorization state.
type Document struct {
	// Initialized distinguishes a valid empty policy from policy that has not
	// been supplied yet.
	Initialized bool `json:"initialized"`

	// Queues and Namespaces contain resource policies derived from EntroQQueue
	// resources.
	Queues     []QueuePolicy     `json:"queues"`
	Namespaces []NamespacePolicy `json:"namespaces"`

	// Identities maps authenticated subjects to the labels asserted for them by
	// EntroQIdentity resources.
	Identities map[string]Identity `json:"identities"`
}

// QueuePolicy describes a queue pattern and the callers permitted to access it.
type QueuePolicy struct {
	Pattern        string              `json:"pattern"`
	MatchType      string              `json:"matchType"`
	AllowedCallers []map[string]string `json:"allowedCallers"`
}

// NamespacePolicy describes a document-namespace pattern and the callers
// permitted to access it.
type NamespacePolicy struct {
	Pattern        string              `json:"pattern"`
	MatchType      string              `json:"matchType"`
	AllowedCallers []map[string]string `json:"allowedCallers"`
}

// Identity holds the labels asserted for an authenticated subject.
type Identity struct {
	Labels map[string]string `json:"labels"`
}

// Validate rejects documents that cannot safely become active policy.
func (d Document) Validate() error {
	if !d.Initialized {
		return errors.New("mesh document is not initialized")
	}
	for i, policy := range d.Queues {
		if err := validatePolicy(policy.Pattern, policy.MatchType, policy.AllowedCallers); err != nil {
			return fmt.Errorf("queue policy %d: %w", i, err)
		}
	}
	for i, policy := range d.Namespaces {
		if err := validatePolicy(policy.Pattern, policy.MatchType, policy.AllowedCallers); err != nil {
			return fmt.Errorf("namespace policy %d: %w", i, err)
		}
	}
	for subject, identity := range d.Identities {
		if subject == "" {
			return errors.New("identity subject is empty")
		}
		if len(identity.Labels) == 0 {
			return fmt.Errorf("identity %q has no labels", subject)
		}
		if err := validateLabels(identity.Labels); err != nil {
			return fmt.Errorf("identity %q: %w", subject, err)
		}
	}
	return nil
}

func validatePolicy(pattern, matchType string, allowedCallers []map[string]string) error {
	if pattern == "" {
		return errors.New("pattern is empty")
	}
	if matchType != "Exact" && matchType != "Prefix" {
		return fmt.Errorf("unknown match type %q", matchType)
	}
	if len(allowedCallers) == 0 {
		return errors.New("allowed callers is empty")
	}
	for i, matcher := range allowedCallers {
		if len(matcher) == 0 {
			return fmt.Errorf("allowed caller %d has no labels", i)
		}
		if err := validateLabels(matcher); err != nil {
			return fmt.Errorf("allowed caller %d: %w", i, err)
		}
	}
	return nil
}

func validateLabels(labels map[string]string) error {
	for key, value := range labels {
		if problems := k8svalidation.IsQualifiedName(key); len(problems) != 0 {
			return fmt.Errorf("invalid label key %q: %s", key, strings.Join(problems, "; "))
		}
		if problems := k8svalidation.IsValidLabelValue(value); len(problems) != 0 {
			return fmt.Errorf("invalid value for label %q: %s", key, strings.Join(problems, "; "))
		}
	}
	return nil
}
