// Package v1alpha1 contains API types for the EntroQ mesh operator.
// Webhook markers live here so controller-gen finds them as package-level
// declarations alongside the CRD types they apply to.
package v1alpha1

// +kubebuilder:webhook:path=/validate-entroq-entroq-io-v1alpha1-entroqqueuename,mutating=false,failurePolicy=fail,sideEffects=None,groups=entroq.entroq.io,resources=entroqqueues,verbs=create;update,versions=v1alpha1,name=ventroqqueue.kb.io,admissionReviewVersions=v1

// +kubebuilder:webhook:path=/validate-entroq-entroq-io-v1alpha1-entroqidentity,mutating=false,failurePolicy=fail,sideEffects=None,groups=entroq.entroq.io,resources=entroqidentities,verbs=create;update,versions=v1alpha1,name=ventroqidentity.kb.io,admissionReviewVersions=v1
