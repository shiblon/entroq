// Package mesh implements EntroQ's built-in Kubernetes mesh authorization.
package mesh

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/shiblon/entroq/pkg/authz"
	"github.com/shiblon/entroq/pkg/authz/meshpolicy"
)

// Authorizer evaluates authorization requests against atomically replaceable
// mesh policy.
type Authorizer struct {
	policy atomic.Pointer[compiledPolicy]
}

var _ authz.Authorizer = (*Authorizer)(nil)

type compiledPolicy struct {
	grants map[string]subjectGrants
}

type subjectGrants struct {
	queues     []resourceGrant
	namespaces []resourceGrant
}

type resourceGrant struct {
	exact  string
	prefix string
}

// New returns an authorizer without initialized mesh policy. Kubernetes
// service accounts can access their own queue prefix before initialization;
// all cross-service and namespace access remains denied.
func New() *Authorizer {
	return new(Authorizer)
}

// Authorize implements authz.Authorizer.
func (a *Authorizer) Authorize(ctx context.Context, req *authz.Request) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if req == nil {
		return &authz.AuthzError{Errors: []string{"authorization request is missing"}}
	}

	subject := ""
	if req.Principal != nil {
		subject = req.Principal.Subject
	}
	grants := grantsFor(a.policy.Load(), subject)

	failedQueues := disallowedQueues(req.Queues, grants.queues)
	failedNamespaces := disallowedNamespaces(req.Namespaces, grants.namespaces)
	errors := decisionErrors(req.ClaimantId, subject, len(failedQueues)+len(failedNamespaces) != 0)
	if len(grants.queues)+len(grants.namespaces) != 0 &&
		len(failedQueues) == 0 && len(failedNamespaces) == 0 && len(errors) == 0 {
		return nil
	}

	return &authz.AuthzError{
		Failed:           failedQueues,
		FailedNamespaces: failedNamespaces,
		Errors:           errors,
	}
}

// ReplaceMesh validates, compiles, and atomically replaces the active mesh
// policy. A rejected document leaves the last good policy active.
func (a *Authorizer) ReplaceMesh(ctx context.Context, document meshpolicy.Document) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := document.Validate(); err != nil {
		return fmt.Errorf("validate mesh policy: %w", err)
	}

	a.policy.Store(compile(document))
	return nil
}

// Ready reports whether the first valid mesh document has been installed.
func (a *Authorizer) Ready() bool {
	return a.policy.Load() != nil
}

// Close implements authz.Authorizer. The authorizer owns no external
// resources.
func (*Authorizer) Close() error {
	return nil
}

func compile(document meshpolicy.Document) *compiledPolicy {
	policy := &compiledPolicy{grants: make(map[string]subjectGrants, len(document.Identities))}
	for subject, identity := range document.Identities {
		var grants subjectGrants
		for _, queue := range document.Queues {
			if !callerSatisfies(identity.Labels, queue.AllowedCallers) {
				continue
			}
			grants.queues = append(grants.queues, newGrant(queue.Pattern, queue.MatchType))
			if queue.MatchType == "Exact" && strings.HasSuffix(queue.Pattern, "/inbox") {
				grants.queues = append(grants.queues, resourceGrant{
					prefix: strings.TrimSuffix(queue.Pattern, "inbox") + "response/",
				})
			}
		}
		for _, namespace := range document.Namespaces {
			if callerSatisfies(identity.Labels, namespace.AllowedCallers) {
				grants.namespaces = append(
					grants.namespaces,
					newGrant(namespace.Pattern, namespace.MatchType),
				)
			}
		}
		policy.grants[subject] = grants
	}
	return policy
}

func newGrant(pattern, matchType string) resourceGrant {
	if matchType == "Exact" {
		return resourceGrant{exact: pattern}
	}
	return resourceGrant{prefix: pattern}
}

func callerSatisfies(labels map[string]string, alternatives []map[string]string) bool {
	for _, required := range alternatives {
		matched := true
		for key, value := range required {
			if labels[key] != value {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func grantsFor(policy *compiledPolicy, subject string) subjectGrants {
	var grants subjectGrants
	if policy != nil {
		grants = policy.grants[subject]
	}
	if prefix, ok := ownQueuePrefix(subject); ok {
		queues := make([]resourceGrant, 0, len(grants.queues)+1)
		queues = append(queues, grants.queues...)
		grants.queues = append(queues, resourceGrant{prefix: prefix})
	}
	return grants
}

func ownQueuePrefix(subject string) (string, bool) {
	parts := strings.Split(subject, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" {
		return "", false
	}
	return "/" + parts[2] + "/" + parts[3] + "/", true
}

func disallowedQueues(wants []*authz.Queue, grants []resourceGrant) []*authz.Queue {
	failed := make([]*authz.Queue, 0)
	for _, want := range wants {
		if want == nil {
			failed = append(failed, nil)
			continue
		}
		if want.Exact == "" && want.Prefix == "" {
			failed = append(failed, cloneQueue(want, uniqueActions(want.Actions)))
			continue
		}
		if len(want.Actions) != 0 && !matchesAny(want.Exact, want.Prefix, grants) {
			failed = append(failed, cloneQueue(want, uniqueActions(want.Actions)))
		}
	}
	return failed
}

func disallowedNamespaces(wants []*authz.Namespace, grants []resourceGrant) []*authz.Namespace {
	failed := make([]*authz.Namespace, 0)
	for _, want := range wants {
		if want == nil {
			failed = append(failed, nil)
			continue
		}
		if want.Exact == "" && want.Prefix == "" {
			failed = append(failed, cloneNamespace(want, uniqueActions(want.Actions)))
			continue
		}
		if len(want.Actions) != 0 && !matchesAny(want.Exact, want.Prefix, grants) {
			failed = append(failed, cloneNamespace(want, uniqueActions(want.Actions)))
		}
	}
	return failed
}

func matchesAny(exact, prefix string, grants []resourceGrant) bool {
	for _, grant := range grants {
		switch {
		case exact != "" && grant.exact != "" && exact == grant.exact:
			return true
		case exact != "" && grant.prefix != "" && strings.HasPrefix(exact, grant.prefix):
			return true
		case prefix != "" && grant.prefix != "" && strings.HasPrefix(prefix, grant.prefix):
			return true
		}
	}
	return false
}

func uniqueActions(actions []authz.Action) []authz.Action {
	result := make([]authz.Action, 0, len(actions))
	seen := make(map[authz.Action]struct{}, len(actions))
	for _, action := range actions {
		if _, ok := seen[action]; ok {
			continue
		}
		seen[action] = struct{}{}
		result = append(result, action)
	}
	return result
}

func cloneQueue(queue *authz.Queue, actions []authz.Action) *authz.Queue {
	return &authz.Queue{Exact: queue.Exact, Prefix: queue.Prefix, Actions: actions}
}

func cloneNamespace(namespace *authz.Namespace, actions []authz.Action) *authz.Namespace {
	return &authz.Namespace{Exact: namespace.Exact, Prefix: namespace.Prefix, Actions: actions}
}

func decisionErrors(claimantID, subject string, resourceFailure bool) []string {
	var errors []string
	if resourceFailure && subject != "" {
		errors = append(errors, "User: "+subject)
	}
	if claimantID != "" && subject != "" && !strings.HasPrefix(claimantID, subject+"#") {
		errors = append(errors, "claimant_id does not match authenticated user")
	}
	return errors
}
