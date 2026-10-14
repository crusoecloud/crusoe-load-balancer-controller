package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// TestReconcileLeavesOtherClassAlone checks that a Service of another load balancer class
// gets no finalizer and no load balancer from this controller.
func TestReconcileLeavesOtherClassAlone(t *testing.T) {
	svc := liveService(map[string]string{})
	svc.Spec.LoadBalancerClass = new("tailscale")

	reconciler := newEnsureTestReconciler(t, noAPICalls(t), svc)

	if _, err := reconciler.Reconcile(context.Background(), requestFor(svc)); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	stored := &corev1.Service{}
	if err := reconciler.Client.Get(context.Background(), client.ObjectKeyFromObject(svc), stored); err != nil {
		t.Fatalf("failed to read back the Service: %v", err)
	}

	if len(stored.Finalizers) > 0 {
		t.Errorf("expected no finalizer, got %v", stored.Finalizers)
	}
}
