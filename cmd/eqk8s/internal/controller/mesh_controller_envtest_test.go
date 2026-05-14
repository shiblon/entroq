package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	entroqv1alpha1 "github.com/shiblon/entroq/cmd/eqk8s/api/v1alpha1"
	"github.com/shiblon/entroq/pkg/eqk8s"
)

// mockOPA captures PUT /v1/data/mesh requests from the reconciler.
type mockOPA struct {
	mu   sync.Mutex
	last *eqk8s.OPAMesh
	srv  *httptest.Server
}

func newMockOPA() *mockOPA {
	m := &mockOPA{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/data/mesh" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var mesh eqk8s.OPAMesh
		if err := json.Unmarshal(body, &mesh); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.last = &mesh
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	return m
}

func (m *mockOPA) Last() *eqk8s.OPAMesh {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		return nil
	}
	copy := *m.last
	return &copy
}

func (m *mockOPA) URL() string { return m.srv.URL }
func (m *mockOPA) Close()      { m.srv.Close() }

// Ordered so that BeforeAll/AfterAll scope to this container. The manager runs
// once for all specs -- starting a new manager per spec triggers Prometheus
// registration conflicts since controller metrics are global.
var _ = Describe("MeshReconciler", Ordered, func() {
	const (
		cmNamespace = "entroq-system"
		timeout     = 15 * time.Second
		interval    = 100 * time.Millisecond
	)

	var (
		opa       *mockOPA
		mgrCancel context.CancelFunc
	)

	BeforeAll(func() {
		opa = newMockOPA()

		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cmNamespace}}
		_ = k8sClient.Create(ctx, ns)

		// Metrics: BindAddress "0" disables the metrics HTTP server, avoiding
		// port conflicts when running multiple test binaries. Controller metrics
		// are still registered with the global Prometheus registry.
		mgr, err := ctrl.NewManager(cfg, ctrl.Options{
			Scheme:  scheme.Scheme,
			Metrics: metricsserver.Options{BindAddress: "0"},
		})
		Expect(err).NotTo(HaveOccurred())

		Expect((&MeshReconciler{
			Client:                 mgr.GetClient(),
			Scheme:                 mgr.GetScheme(),
			OPAClient:              eqk8s.NewOPAClient(eqk8s.WithOPAURL(opa.URL())),
			ResyncInterval:         time.Hour,
			MeshConfigMapNamespace: cmNamespace,
		}).SetupWithManager(mgr)).To(Succeed())

		mgrCtx, cancel := context.WithCancel(ctx)
		mgrCancel = cancel
		go func() { _ = mgr.Start(mgrCtx) }()

		// Wait for the informer cache to sync before running specs.
		syncCtx, syncCancel := context.WithTimeout(mgrCtx, 30*time.Second)
		defer syncCancel()
		Expect(mgr.GetCache().WaitForCacheSync(syncCtx)).To(BeTrue())
	})

	AfterAll(func() {
		mgrCancel()
		opa.Close()
	})

	It("reconciles EntroQQueue and EntroQIdentity into ConfigMap and OPA", func() {
		queue := &entroqv1alpha1.EntroQQueue{
			ObjectMeta: metav1.ObjectMeta{Name: "svc-b-inbox", Namespace: "default"},
			Spec: entroqv1alpha1.EntroQQueueSpec{
				Queues: []entroqv1alpha1.QueuePattern{{
					Pattern:   "/payments/svc-b/inbox",
					MatchType: entroqv1alpha1.MatchExact,
					AllowedCallers: []entroqv1alpha1.LabelMatcher{
						{Labels: map[string]string{"group": "frontend"}},
					},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, queue)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, queue) })

		identity := &entroqv1alpha1.EntroQIdentity{
			ObjectMeta: metav1.ObjectMeta{Name: "svc-a-identity", Namespace: "default"},
			Spec: entroqv1alpha1.EntroQIdentitySpec{
				Identities: []entroqv1alpha1.ServiceAccountLabels{
					{ServiceAccount: "svc-a", Labels: map[string]string{"group": "frontend"}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, identity)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, identity) })

		// ConfigMap should appear with correct mesh content.
		cm := &corev1.ConfigMap{}
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      meshConfigMapName,
				Namespace: cmNamespace,
			}, cm)).To(Succeed())

			var mesh eqk8s.OPAMesh
			g.Expect(json.Unmarshal([]byte(cm.Data[meshConfigMapKey]), &mesh)).To(Succeed())
			g.Expect(mesh.Initialized).To(BeTrue())
			g.Expect(mesh.Queues).To(HaveLen(1))
			g.Expect(mesh.Queues[0].Pattern).To(Equal("/payments/svc-b/inbox"))
			g.Expect(mesh.Identities).To(HaveKey("system:serviceaccount:default:svc-a"))
		}, timeout, interval).Should(Succeed())

		// OPA should also have received the document.
		Eventually(func(g Gomega) {
			mesh := opa.Last()
			g.Expect(mesh).NotTo(BeNil())
			g.Expect(mesh.Initialized).To(BeTrue())
			g.Expect(mesh.Queues).To(HaveLen(1))
			g.Expect(mesh.Identities).To(HaveKey("system:serviceaccount:default:svc-a"))
		}, timeout, interval).Should(Succeed())
	})

	It("pushes an updated document when a CRD changes", func() {
		queue := &entroqv1alpha1.EntroQQueue{
			ObjectMeta: metav1.ObjectMeta{Name: "update-test", Namespace: "default"},
			Spec: entroqv1alpha1.EntroQQueueSpec{
				Queues: []entroqv1alpha1.QueuePattern{{
					Pattern:        "/ns/svc/inbox",
					MatchType:      entroqv1alpha1.MatchExact,
					AllowedCallers: []entroqv1alpha1.LabelMatcher{{Labels: map[string]string{"role": "worker"}}},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, queue)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, queue) })

		// Wait for the initial reconcile to include this queue.
		Eventually(func(g Gomega) {
			mesh := opa.Last()
			g.Expect(mesh).NotTo(BeNil())
			found := false
			for _, q := range mesh.Queues {
				if q.Pattern == "/ns/svc/inbox" {
					found = true
					break
				}
			}
			g.Expect(found).To(BeTrue())
		}, timeout, interval).Should(Succeed())

		// Update the pattern and wait for the re-push.
		queue.Spec.Queues[0].Pattern = "/ns/svc/v2/inbox"
		Expect(k8sClient.Update(ctx, queue)).To(Succeed())

		Eventually(func(g Gomega) {
			found := false
			if mesh := opa.Last(); mesh != nil {
				for _, q := range mesh.Queues {
					if q.Pattern == "/ns/svc/v2/inbox" {
						found = true
						break
					}
				}
			}
			g.Expect(found).To(BeTrue())
		}, timeout, interval).Should(Succeed())
	})
})
