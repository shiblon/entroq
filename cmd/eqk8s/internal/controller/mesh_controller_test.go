package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	entroqv1alpha1 "github.com/shiblon/entroq/cmd/eqk8s/api/v1alpha1"
	"github.com/shiblon/entroq/pkg/eqk8s"
)

func queue(ns, name string, patterns []entroqv1alpha1.QueuePattern) entroqv1alpha1.EntroQQueue {
	return entroqv1alpha1.EntroQQueue{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       entroqv1alpha1.EntroQQueueSpec{Queues: patterns},
	}
}

func identity(ns string, ids []entroqv1alpha1.ServiceAccountLabels) entroqv1alpha1.EntroQIdentity {
	return entroqv1alpha1.EntroQIdentity{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns},
		Spec:       entroqv1alpha1.EntroQIdentitySpec{Identities: ids},
	}
}

func TestBuildMeshInitialized(t *testing.T) {
	mesh := buildMesh(nil, nil)
	if !mesh.Initialized {
		t.Error("expected Initialized=true even with empty inputs")
	}
}

func TestBuildMeshEmptyInputs(t *testing.T) {
	mesh := buildMesh(nil, nil)
	if len(mesh.Queues) != 0 {
		t.Errorf("expected no queues, got %d", len(mesh.Queues))
	}
	if len(mesh.Identities) != 0 {
		t.Errorf("expected no identities, got %d", len(mesh.Identities))
	}
}

func TestBuildMeshQueueExact(t *testing.T) {
	queues := []entroqv1alpha1.EntroQQueue{
		queue("payments", "svc-b", []entroqv1alpha1.QueuePattern{
			{
				Pattern:   "/payments/svc-b/inbox",
				MatchType: entroqv1alpha1.MatchExact,
				AllowedCallers: []entroqv1alpha1.LabelMatcher{
					{Labels: map[string]string{"group": "frontend"}},
				},
			},
		}),
	}

	mesh := buildMesh(queues, nil)

	if len(mesh.Queues) != 1 {
		t.Fatalf("expected 1 queue policy, got %d", len(mesh.Queues))
	}
	p := mesh.Queues[0]
	if p.Pattern != "/payments/svc-b/inbox" {
		t.Errorf("wrong pattern: %q", p.Pattern)
	}
	if p.MatchType != "Exact" {
		t.Errorf("wrong matchType: %q", p.MatchType)
	}
	if len(p.AllowedCallers) != 1 {
		t.Fatalf("expected 1 allowedCallers entry, got %d", len(p.AllowedCallers))
	}
	if p.AllowedCallers[0]["group"] != "frontend" {
		t.Errorf("wrong label value: %q", p.AllowedCallers[0]["group"])
	}
}

func TestBuildMeshQueuePrefix(t *testing.T) {
	queues := []entroqv1alpha1.EntroQQueue{
		queue("ns", "svc", []entroqv1alpha1.QueuePattern{
			{
				Pattern:   "/shared/",
				MatchType: entroqv1alpha1.MatchPrefix,
				AllowedCallers: []entroqv1alpha1.LabelMatcher{
					{Labels: map[string]string{"team": "platform"}},
				},
			},
		}),
	}

	mesh := buildMesh(queues, nil)

	if len(mesh.Queues) != 1 {
		t.Fatalf("expected 1 queue policy, got %d", len(mesh.Queues))
	}
	if mesh.Queues[0].MatchType != "Prefix" {
		t.Errorf("wrong matchType: %q", mesh.Queues[0].MatchType)
	}
}

func TestBuildMeshMultipleAllowedCallers(t *testing.T) {
	queues := []entroqv1alpha1.EntroQQueue{
		queue("ns", "svc", []entroqv1alpha1.QueuePattern{
			{
				Pattern:   "/ns/svc/inbox",
				MatchType: entroqv1alpha1.MatchExact,
				AllowedCallers: []entroqv1alpha1.LabelMatcher{
					{Labels: map[string]string{"group": "frontend"}},
					{Labels: map[string]string{"group": "backend", "team": "platform"}},
				},
			},
		}),
	}

	mesh := buildMesh(queues, nil)

	callers := mesh.Queues[0].AllowedCallers
	if len(callers) != 2 {
		t.Fatalf("expected 2 allowedCallers, got %d", len(callers))
	}
	if callers[1]["team"] != "platform" {
		t.Errorf("second matcher missing team label, got %v", callers[1])
	}
}

func TestBuildMeshIdentity(t *testing.T) {
	identities := []entroqv1alpha1.EntroQIdentity{
		identity("payments", []entroqv1alpha1.ServiceAccountLabels{
			{ServiceAccount: "svc-a", Labels: map[string]string{"group": "frontend", "team": "payments"}},
			{ServiceAccount: "svc-b", Labels: map[string]string{"group": "backend"}},
		}),
	}

	mesh := buildMesh(nil, identities)

	wantA := eqk8s.OPAIdentity{Labels: map[string]string{"group": "frontend", "team": "payments"}}
	wantB := eqk8s.OPAIdentity{Labels: map[string]string{"group": "backend"}}

	keyA := "system:serviceaccount:payments:svc-a"
	keyB := "system:serviceaccount:payments:svc-b"

	if got := mesh.Identities[keyA]; got.Labels["group"] != wantA.Labels["group"] || got.Labels["team"] != wantA.Labels["team"] {
		t.Errorf("svc-a: got %v, want %v", got, wantA)
	}
	if got := mesh.Identities[keyB]; got.Labels["group"] != wantB.Labels["group"] {
		t.Errorf("svc-b: got %v, want %v", got, wantB)
	}
}

func TestBuildMeshIdentityKey(t *testing.T) {
	identities := []entroqv1alpha1.EntroQIdentity{
		identity("mynamespace", []entroqv1alpha1.ServiceAccountLabels{
			{ServiceAccount: "mysa", Labels: map[string]string{"k": "v"}},
		}),
	}

	mesh := buildMesh(nil, identities)

	want := "system:serviceaccount:mynamespace:mysa"
	if _, ok := mesh.Identities[want]; !ok {
		t.Errorf("expected identity key %q, got keys %v", want, mesh.Identities)
	}
}
