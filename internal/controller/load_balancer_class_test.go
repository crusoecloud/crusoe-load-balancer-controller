package controller

import (
	"context"
	"net/http"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const tailscaleClass = "tailscale"

// otherClassService is a LoadBalancer Service owned by another controller, as tailscale-operator
// owns one with loadBalancerClass: tailscale.
func otherClassService() *corev1.Service {
	svc := liveService(map[string]string{})
	svc.Spec.LoadBalancerClass = new(tailscaleClass)

	return svc
}

func TestIsWatchedService(t *testing.T) {
	takenOver := otherClassService()
	takenOver.Finalizers = []string{serviceFinalizer}

	clusterIP := liveService(map[string]string{})
	clusterIP.Spec.Type = corev1.ServiceTypeClusterIP

	tests := []struct {
		name string
		obj  client.Object
		want bool
	}{
		{"default class", liveService(map[string]string{}), true},
		{"other class", otherClassService(), false},
		{"other class taken over by an earlier version", takenOver, true},
		{"not a LoadBalancer", clusterIP, false},
		{"not a Service", &corev1.Node{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isWatchedService(tt.obj); got != tt.want {
				t.Errorf("isWatchedService() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestReconcileLeavesOtherClassAlone covers MAPS-1768: a Service of another load balancer class
// gets no finalizer, no load balancer, no annotations and no external IP from this controller.
func TestReconcileLeavesOtherClassAlone(t *testing.T) {
	svc := otherClassService()
	reconciler := newEnsureTestReconciler(t, noAPICalls(t), svc)

	result, err := reconciler.Reconcile(context.Background(), requestFor(svc))
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if !result.IsZero() {
		t.Errorf("expected no requeue, got %+v", result)
	}

	stored := &corev1.Service{}
	if err := reconciler.Client.Get(context.Background(), client.ObjectKeyFromObject(svc), stored); err != nil {
		t.Fatalf("failed to read back the Service: %v", err)
	}

	if len(stored.Finalizers) > 0 {
		t.Errorf("expected no finalizer, got %v", stored.Finalizers)
	}

	if _, recorded := stored.Annotations[loadbalancerIDLabelKey]; recorded {
		t.Error("expected no load balancer to be recorded")
	}

	if len(stored.Status.LoadBalancer.Ingress) > 0 {
		t.Errorf("expected no external IP, got %+v", stored.Status.LoadBalancer.Ingress)
	}
}

// TestReconcileReleasesTakenOverOtherClassService checks that a Service of another class which an
// earlier version took over is still cleaned up and released when deleted, rather than left
// hanging in Terminating with this controller's finalizer.
func TestReconcileReleasesTakenOverOtherClassService(t *testing.T) {
	deleted := false

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == loadBalancerPath() {
			deleted = true
			writeJSON(w, `{}`)

			return
		}

		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	svc := deletingService(0, map[string]string{loadbalancerIDLabelKey: testLoadBalancerID})
	delete(svc.Annotations, ManageFirewallRuleKey)
	svc.Spec.Type = corev1.ServiceTypeLoadBalancer
	svc.Spec.LoadBalancerClass = new(tailscaleClass)

	reconciler := newFirewallTestReconciler(t, handler, svc)

	if _, err := reconciler.Reconcile(context.Background(), requestFor(svc)); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	if !deleted {
		t.Error("expected the load balancer created by the earlier version to be deleted")
	}

	stored := &corev1.Service{}
	err := reconciler.Client.Get(context.Background(), client.ObjectKeyFromObject(svc), stored)
	if err == nil && controllerutil.ContainsFinalizer(stored, serviceFinalizer) {
		t.Error("expected the finalizer to be removed")
	}
}

// TestEnsureLoadBalancerIngressLeavesOtherClassStatus covers the loop seen in MAPS-1768:
// tailscale-operator clears the status of a Service it owns, and this controller must not put
// a Crusoe VIP back.
func TestEnsureLoadBalancerIngressLeavesOtherClassStatus(t *testing.T) {
	svc := otherClassService()
	svc.Annotations[loadbalancerIDLabelKey] = testLoadBalancerID

	reconciler := newEnsureTestReconciler(t, noAPICalls(t), svc)

	if requeue := reconciler.ensureLoadBalancerIngress(context.Background(), svc); requeue != 0 {
		t.Errorf("expected no requeue, got %s", requeue)
	}

	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		t.Errorf("expected the status to stay empty, got %+v", svc.Status.LoadBalancer.Ingress)
	}
}

// TestParseListenPortsAndBackendsSortsBackends checks that the same set of nodes always yields
// the same backends, whatever order the node list comes back in.
func TestParseListenPortsAndBackendsSortsBackends(t *testing.T) {
	svc := liveService(map[string]string{})
	reconciler := newEnsureTestReconciler(t, noAPICalls(t), svc)

	nodes := []client.Object{}
	for i, ip := range []string{"10.0.0.9", "10.0.0.10", "10.0.0.1"} {
		nodes = append(nodes, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-" + string(rune('a'+i))},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
				Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}},
			},
		})
	}

	reconciler.Client = fake.NewClientBuilder().WithScheme(reconciler.Scheme).WithObjects(nodes...).Build()

	listeners := reconciler.parseListenPortsAndBackends(context.Background(), svc.Spec.Ports, log.Log)
	if len(listeners) != 1 {
		t.Fatalf("expected one listener, got %d", len(listeners))
	}

	got := []string{}
	for _, backend := range listeners[0].Backends {
		got = append(got, backend.Ip)
	}

	if want := []string{"10.0.0.1", "10.0.0.10", "10.0.0.9"}; !slices.Equal(got, want) {
		t.Errorf("backends = %v, want %v", got, want)
	}
}
