package webhook

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	entroqv1alpha1 "github.com/shiblon/entroq/cmd/eqk8s/api/v1alpha1"
)

// EntroQQueueValidator validates EntroQQueue resources on create and update.
// The OpenAPI schema already enforces structural constraints (non-empty lists,
// non-empty strings). This validator adds semantic checks OpenAPI cannot express:
// patterns must start with "/", no empty segments or traversal, Prefix patterns
// must end with "/", Exact patterns must not end with "/".
type EntroQQueueValidator struct{}

var _ admission.Validator[*entroqv1alpha1.EntroQQueue] = &EntroQQueueValidator{}

func (v *EntroQQueueValidator) ValidateCreate(_ context.Context, q *entroqv1alpha1.EntroQQueue) (admission.Warnings, error) {
	return nil, validateEntroQQueue(q)
}

func (v *EntroQQueueValidator) ValidateUpdate(_ context.Context, _, newQ *entroqv1alpha1.EntroQQueue) (admission.Warnings, error) {
	return nil, validateEntroQQueue(newQ)
}

func (v *EntroQQueueValidator) ValidateDelete(_ context.Context, _ *entroqv1alpha1.EntroQQueue) (admission.Warnings, error) {
	return nil, nil
}

func validateEntroQQueue(q *entroqv1alpha1.EntroQQueue) error {
	var errs []string
	for i, qp := range q.Spec.Queues {
		if err := validatePattern(qp.Pattern, qp.MatchType); err != nil {
			errs = append(errs, fmt.Sprintf("queues[%d].pattern: %v", i, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid EntroQQueue %s/%s: %s", q.Namespace, q.Name, strings.Join(errs, "; "))
	}
	return nil
}

func validatePattern(pattern string, matchType entroqv1alpha1.MatchType) error {
	if !strings.HasPrefix(pattern, "/") {
		return fmt.Errorf("%q must start with /", pattern)
	}
	if strings.Contains(pattern, "//") {
		return fmt.Errorf("%q must not contain empty segments (//)", pattern)
	}
	for _, seg := range strings.Split(strings.Trim(pattern, "/"), "/") {
		if seg == ".." {
			return fmt.Errorf("%q must not contain path traversal (..)", pattern)
		}
	}
	switch matchType {
	case entroqv1alpha1.MatchPrefix:
		if !strings.HasSuffix(pattern, "/") {
			return fmt.Errorf("%q with matchType Prefix must end with /", pattern)
		}
	case entroqv1alpha1.MatchExact:
		if pattern != "/" && strings.HasSuffix(pattern, "/") {
			return fmt.Errorf("%q with matchType Exact must not end with /", pattern)
		}
	}
	return nil
}

// Ensure EntroQQueueValidator satisfies runtime.Object indirectly via the
// Validator interface so the compiler catches registration mismatches.
var _ runtime.Object = (*entroqv1alpha1.EntroQQueue)(nil)
