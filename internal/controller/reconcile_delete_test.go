package controller

import (
	"context"
	"net/http"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const testFinalizer = "crusoe.ai/crusoe-load-balancer-controller.finalizer"

func requestFor(svc *corev1.Service) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)}
}

// TestReconcileKeepsFinalizerWhileCleanupPending covers Reconcile honouring a non-zero result
// from handleDelete. While the firewall rule's create operation is still in flight, removing
// the finalizer would orphan the rule that is about to appear. Reconcile used to discard
// handleDelete's result and remove the finalizer whenever there was no error.
func TestReconcileKeepsFinalizerWhileCleanupPending(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, `{"operation_id":"`+testOperationID+`","state":"IN_PROGRESS","started_at":"","completed_at":"","metadata":null}`)

			return
		}

		t.Errorf("nothing may be deleted while the rule's create operation is in flight, got %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	svc := deletingService(0, map[string]string{
		loadbalancerIDLabelKey:     testLoadBalancerID,
		FirewallRuleOperationIdKey: testOperationID,
	})

	reconciler := newFirewallTestReconciler(t, handler, svc)

	result, err := reconciler.Reconcile(context.Background(), requestFor(svc))
	if err != nil {
		t.Fatalf("waiting on an in-flight operation is not an error, got: %v", err)
	}

	if result.RequeueAfter != firewallRuleRequeueInterval {
		t.Errorf("expected a requeue after %s to check the operation again, got %+v", firewallRuleRequeueInterval, result)
	}

	stored := &corev1.Service{}
	if err := reconciler.Client.Get(context.Background(), client.ObjectKeyFromObject(svc), stored); err != nil {
		t.Fatalf("the Service must still exist while cleanup is pending, got: %v", err)
	}

	if !controllerutil.ContainsFinalizer(stored, testFinalizer) {
		t.Error("the finalizer must be kept while cleanup is pending")
	}
}

// TestReconcileReleasesServiceWithoutLoadBalancer covers deleting a Service that never got a
// load balancer, such as one rejected for mixing TCP and UDP ports. There is nothing to delete:
// the firewall lookup by name finds no rule, no load balancer delete may be sent (its id would
// be empty), and the finalizer must come off so the Service does not hang in Terminating.
func TestReconcileReleasesServiceWithoutLoadBalancer(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/projects/"+testProjectID+"/networking/vpc-firewall-rules" {
			writeJSON(w, `{"items":[]}`)

			return
		}

		t.Errorf("a Service without a load balancer needs no other API call, got %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	// Firewall-managed, but neither a rule nor a load balancer was ever recorded.
	svc := deletingService(0, map[string]string{})

	reconciler := newFirewallTestReconciler(t, handler, svc)

	result, err := reconciler.Reconcile(context.Background(), requestFor(svc))
	if err != nil || !result.IsZero() {
		t.Fatalf("expected the Service to be released without a requeue, got %+v, %v", result, err)
	}

	err = reconciler.Client.Get(context.Background(), client.ObjectKeyFromObject(svc), &corev1.Service{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("with the finalizer removed the Service must be gone, got: %v", err)
	}
}
