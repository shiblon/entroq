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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	entroqv1alpha1 "github.com/shiblon/entroq/cmd/eqk8s/api/v1alpha1"
	"github.com/shiblon/entroq/pkg/eqk8s"
)

// MeshReconciler watches EntroQQueue and EntroQIdentity resources and rebuilds
// the OPA mesh authorization document whenever either changes.
type MeshReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	OPAClient      *eqk8s.OPAClient
	ResyncInterval time.Duration
}

// +kubebuilder:rbac:groups=entroq.entroq.io,resources=entroqqueues,verbs=get;list;watch
// +kubebuilder:rbac:groups=entroq.entroq.io,resources=entroqqueues/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=entroq.entroq.io,resources=entroqidentities,verbs=get;list;watch
// +kubebuilder:rbac:groups=entroq.entroq.io,resources=entroqidentities/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;create;update

func (r *MeshReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("reconciling mesh OPA document", "trigger", req.NamespacedName)

	var queues entroqv1alpha1.EntroQQueueList
	if err := r.List(ctx, &queues); err != nil {
		return ctrl.Result{}, fmt.Errorf("list EntroQQueues: %w", err)
	}

	var identities entroqv1alpha1.EntroQIdentityList
	if err := r.List(ctx, &identities); err != nil {
		return ctrl.Result{}, fmt.Errorf("list EntroQIdentities: %w", err)
	}

	mesh := buildMesh(queues.Items, identities.Items)

	if err := r.writeMeshConfigMap(ctx, mesh); err != nil {
		return ctrl.Result{}, fmt.Errorf("write mesh configmap: %w", err)
	}

	if err := r.OPAClient.PushMesh(ctx, mesh); err != nil {
		return ctrl.Result{}, fmt.Errorf("push mesh document: %w", err)
	}

	log.Info("mesh OPA document updated", "queues", len(queues.Items), "identities", len(identities.Items))
	return ctrl.Result{RequeueAfter: r.ResyncInterval}, nil
}

const (
	meshConfigMapName      = "entroq-mesh"
	meshConfigMapNamespace = "entroq-system"
	meshConfigMapKey       = "mesh.json"
)

// writeMeshConfigMap writes the mesh document to a ConfigMap so OPA can load
// it on startup via a volume mount.
func (r *MeshReconciler) writeMeshConfigMap(ctx context.Context, mesh eqk8s.OPAMesh) error {
	data, err := json.Marshal(mesh)
	if err != nil {
		return fmt.Errorf("marshal mesh: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      meshConfigMapName,
			Namespace: meshConfigMapNamespace,
		},
	}

	existing := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: meshConfigMapName, Namespace: meshConfigMapNamespace}, existing)
	if errors.IsNotFound(err) {
		cm.Data = map[string]string{meshConfigMapKey: string(data)}
		return r.Create(ctx, cm)
	}
	if err != nil {
		return fmt.Errorf("get configmap: %w", err)
	}

	existing.Data = map[string]string{meshConfigMapKey: string(data)}
	return r.Update(ctx, existing)
}

// buildMesh constructs an OPAMesh from the current set of EntroQQueue and
// EntroQIdentity resources. It is pure and contains no side effects.
func buildMesh(queues []entroqv1alpha1.EntroQQueue, identities []entroqv1alpha1.EntroQIdentity) eqk8s.OPAMesh {
	mesh := eqk8s.OPAMesh{
		Initialized: true,
		Identities:  make(map[string]eqk8s.OPAIdentity),
	}

	for _, q := range queues {
		for _, qp := range q.Spec.Queues {
			var callers []map[string]string
			for _, ac := range qp.AllowedCallers {
				callers = append(callers, ac.Labels)
			}
			mesh.Queues = append(mesh.Queues, eqk8s.OPAQueuePolicy{
				Pattern:        qp.Pattern,
				MatchType:      string(qp.MatchType),
				AllowedCallers: callers,
			})
		}
	}

	for _, id := range identities {
		for _, sal := range id.Spec.Identities {
			key := "system:serviceaccount:" + id.Namespace + ":" + sal.ServiceAccount
			mesh.Identities[key] = eqk8s.OPAIdentity{Labels: sal.Labels}
		}
	}

	return mesh
}

// SetupWithManager sets up the controller with the Manager.
func (r *MeshReconciler) SetupWithManager(mgr ctrl.Manager) error {
	enqueueFixed := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			return []reconcile.Request{
				{NamespacedName: types.NamespacedName{Name: "mesh"}},
			}
		},
	)

	return ctrl.NewControllerManagedBy(mgr).
		For(&entroqv1alpha1.EntroQQueue{}).
		Watches(&entroqv1alpha1.EntroQIdentity{}, enqueueFixed).
		Named("mesh").
		Complete(r)
}
