package webhook

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	entroqv1alpha1 "github.com/shiblon/entroq/cmd/eqk8s/api/v1alpha1"
)

// EntroQIdentityValidator validates EntroQIdentity resources on create and update.
// Structural constraints are enforced by the OpenAPI schema. This validator
// exists to ensure failurePolicy=fail applies -- if the operator is unreachable,
// identity changes are rejected rather than silently accepted.
type EntroQIdentityValidator struct{}

var _ admission.Validator[*entroqv1alpha1.EntroQIdentity] = &EntroQIdentityValidator{}

func (v *EntroQIdentityValidator) ValidateCreate(_ context.Context, _ *entroqv1alpha1.EntroQIdentity) (admission.Warnings, error) {
	return nil, nil
}

func (v *EntroQIdentityValidator) ValidateUpdate(_ context.Context, _, _ *entroqv1alpha1.EntroQIdentity) (admission.Warnings, error) {
	return nil, nil
}

func (v *EntroQIdentityValidator) ValidateDelete(_ context.Context, _ *entroqv1alpha1.EntroQIdentity) (admission.Warnings, error) {
	return nil, nil
}
