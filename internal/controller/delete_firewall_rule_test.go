package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/ory/viper"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testProjectID = "11111111-1111-1111-1111-111111111111"

// newFirewallTestReconciler returns a reconciler whose Crusoe client talks to handler and
// whose Kubernetes client is backed by svc. A successful rule deletion patches the Service to
// clear its rule annotations, so a real client is needed even on the delete path.
func newFirewallTestReconciler(t *testing.T, handler http.Handler, svc *corev1.Service) *ServiceReconciler {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	viper.Set(CrusoeProjectIDFlag, testProjectID)
	t.Cleanup(func() { viper.Set(CrusoeProjectIDFlag, "") })

	cfg := crusoeapi.NewConfiguration()
	cfg.BasePath = srv.URL
	cfg.HTTPClient = srv.Client()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to build scheme: %v", err)
	}

	return &ServiceReconciler{
		Client:       fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build(),
		Scheme:       scheme,
		CrusoeClient: crusoeapi.NewAPIClient(cfg),
	}
}

// deletingService builds a Service under deletion whose deletion was requested deletedAgo in
// the past, which is how the hold limit is exercised without injecting a clock.
func deletingService(deletedAgo time.Duration, annotations map[string]string) *corev1.Service {
	deletionTime := metav1.NewTime(time.Now().Add(-deletedAgo))

	annotations[ManageFirewallRuleKey] = ManageFirewallRuleEnabled

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "probe",
			Namespace:         "lb-udp-test",
			UID:               "22222222-2222-2222-2222-222222222222",
			DeletionTimestamp: &deletionTime,
			// A Service under deletion still carries the controller's finalizer; without one
			// the fake client refuses an object that has a deletion timestamp.
			Finalizers:  []string{"crusoe.ai/crusoe-load-balancer-controller.finalizer"},
			Annotations: annotations,
		},
	}
}

// TestDeleteFirewallRuleHoldsOnPersistentError pins the policy for a rule that cannot be
// deleted: the error is returned so the finalizer stays, and that does not stop being true
// with time. Holding is what lets cleanup finish on its own once the cause (a rotated key, an
// outage) is fixed; releasing would leave an ALLOW rule behind that nothing cleans up, and
// since the load balancer deletion shares the key and holds the same way, it would not
// unblock the Service either.
func TestDeleteFirewallRuleHoldsOnPersistentError(t *testing.T) {
	for _, deletedAgo := range []time.Duration{0, firewallRuleInFlightHoldLimit + time.Minute} {
		t.Run(deletedAgo.String(), func(t *testing.T) {
			var deleteAttempts int

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deleteAttempts++
					w.WriteHeader(http.StatusForbidden)

					return
				}

				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			})

			svc := deletingService(deletedAgo, map[string]string{
				FirewallRuleIdKey: "33333333-3333-3333-3333-333333333333",
			})

			reconciler := newFirewallTestReconciler(t, handler, svc)

			err := reconciler.deleteFirewallRule(context.Background(), svc)
			if err == nil {
				t.Fatal("a failed delete must return an error so the finalizer is kept, however long the deletion has been pending")
			}

			if errors.Is(err, errFirewallRuleOperationInFlight) {
				t.Fatalf("a delete failure must back off, not poll as if an operation were in flight: %v", err)
			}

			if deleteAttempts != 1 {
				t.Errorf("expected exactly one delete attempt per reconcile, got %d", deleteAttempts)
			}
		})
	}
}

// TestDeleteFirewallRuleHoldsWhileOperationInFlight covers deleting a Service in the seconds
// between submitting the rule's create operation and that operation settling. Releasing then
// would orphan the rule that is about to appear.
func TestDeleteFirewallRuleHoldsWhileOperationInFlight(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"operation_id":"44444444-4444-4444-4444-444444444444","state":"IN_PROGRESS","started_at":"","completed_at":"","metadata":null}`))

			return
		}

		t.Errorf("nothing may be deleted while the create operation is in flight, got %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	svc := deletingService(0, map[string]string{
		FirewallRuleOperationIdKey: testOperationID,
	})

	reconciler := newFirewallTestReconciler(t, handler, svc)

	err := reconciler.deleteFirewallRule(context.Background(), svc)
	if !errors.Is(err, errFirewallRuleOperationInFlight) {
		t.Fatalf("expected an in-flight error so the caller polls, got %v", err)
	}
}

const (
	testOperationID = "44444444-4444-4444-4444-444444444444"
	testFoundRuleID = "55555555-5555-5555-5555-555555555555"
)

// byNameHandler serves an operation lookup with operationStatus and operationBody, lists a
// single rule carrying the Service's deterministic name, and records the path of any delete.
func byNameHandler(t *testing.T, operationStatus int, operationBody string, deleted *string) http.Handler {
	t.Helper()

	ruleName, err := GenerateLoadBalancerResourceName("lb-udp-test", "probe", "22222222-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatalf("failed to generate rule name: %v", err)
	}

	operationPath := "/projects/" + testProjectID + "/networking/vpc-firewall-rules/operations/" + testOperationID

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == operationPath:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(operationStatus)
			_, _ = w.Write([]byte(operationBody))

		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"items":[{"id":"` + testFoundRuleID + `","name":"` + ruleName + `"}]}`))

		case r.Method == http.MethodDelete:
			*deleted = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
}

func assertRuleDeletedByName(t *testing.T, deleted string) {
	t.Helper()

	if want := "/projects/" + testProjectID + "/networking/vpc-firewall-rules/" + testFoundRuleID; deleted != want {
		t.Errorf("expected the rule found by name to be deleted at %s, got %q", want, deleted)
	}
}

// TestDeleteFirewallRuleResolvesByNamePastLimit checks that an in-flight operation stops being
// waited on once the hold limit has passed, and that the code then tries the rule's
// deterministic name rather than giving up: by then the create may well have landed, and that
// rule is exactly the one nothing else will clean up.
func TestDeleteFirewallRuleResolvesByNamePastLimit(t *testing.T) {
	var deleted string

	inFlight := `{"operation_id":"` + testOperationID + `","state":"IN_PROGRESS","started_at":"","completed_at":"","metadata":null}`
	handler := byNameHandler(t, http.StatusOK, inFlight, &deleted)

	svc := deletingService(firewallRuleInFlightHoldLimit+time.Minute, map[string]string{
		FirewallRuleOperationIdKey: testOperationID,
	})

	reconciler := newFirewallTestReconciler(t, handler, svc)

	if err := reconciler.deleteFirewallRule(context.Background(), svc); err != nil {
		t.Fatalf("past the hold limit an in-flight operation must yield to the name lookup, got error: %v", err)
	}

	assertRuleDeletedByName(t, deleted)
}

// TestDeleteFirewallRuleResolvesByNameWhenOperationGone covers the aged-out operation record:
// a 404 is unresolvable, so the code goes straight to the name lookup with no hold at all.
func TestDeleteFirewallRuleResolvesByNameWhenOperationGone(t *testing.T) {
	var deleted string

	handler := byNameHandler(t, http.StatusNotFound, `{}`, &deleted)

	svc := deletingService(0, map[string]string{
		FirewallRuleOperationIdKey: testOperationID,
	})

	reconciler := newFirewallTestReconciler(t, handler, svc)

	if err := reconciler.deleteFirewallRule(context.Background(), svc); err != nil {
		t.Fatalf("a vanished operation must resolve through the rule's name, got error: %v", err)
	}

	assertRuleDeletedByName(t, deleted)
}

// TestDeleteFirewallRuleHoldsOnTransientOperationError covers the other way the delete path
// can fail: the rule id is not recorded yet and the operation behind it cannot be read. A 500
// says nothing about whether the rule exists, so the deletion must be retried rather than
// resolved by name, which could conclude "no rule" about a rule that is simply not listed yet.
func TestDeleteFirewallRuleHoldsOnTransientOperationError(t *testing.T) {
	var sawDelete bool

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			sawDelete = true
		}

		w.WriteHeader(http.StatusInternalServerError)
	})

	svc := deletingService(0, map[string]string{
		FirewallRuleOperationIdKey: testOperationID,
	})

	reconciler := newFirewallTestReconciler(t, handler, svc)

	err := reconciler.deleteFirewallRule(context.Background(), svc)
	if err == nil {
		t.Fatal("an unreadable operation must return an error so the deletion is retried")
	}

	if errors.Is(err, errFirewallRuleOperationInFlight) {
		t.Fatalf("a transient read failure must back off, not poll as in-flight: %v", err)
	}

	if sawDelete {
		t.Error("nothing may be deleted while the operation's outcome is unknown")
	}
}
