package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"lb_controller/internal/crusoe"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/ory/viper"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testSubnetID = "66666666-6666-6666-6666-666666666666"
	testVPCID    = "77777777-7777-7777-7777-777777777777"
	testNodeIP   = "172.27.25.250"
	testNodePort = 31675
)

// liveService builds a Service that already has a load balancer and a submitted firewall rule
// operation, which is the state ensureFirewallRule resumes from on a later reconcile.
func liveService(annotations map[string]string) *corev1.Service {
	annotations[ManageFirewallRuleKey] = ManageFirewallRuleEnabled

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "probe",
			Namespace:   "lb-udp-test",
			UID:         "22222222-2222-2222-2222-222222222222",
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{
				{Port: 53, Protocol: corev1.ProtocolUDP, NodePort: testNodePort},
			},
		},
	}
}

// readyNode gives parseListenPortsAndBackends an internal IP to build backends from, without
// which GetFirewallRuleArgs finds no destination ports and skips the rule entirely.
func readyNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "np-1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: testNodeIP}},
		},
	}
}

func newEnsureTestReconciler(t *testing.T, handler http.Handler, svc *corev1.Service) *ServiceReconciler {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// The self-managed path resolves the VPC from one subnet lookup, which keeps the fake API
	// surface to a single extra route.
	viper.Set(CrusoeProjectIDFlag, testProjectID)
	viper.Set(CrusoeSubnetIDFlag, testSubnetID)
	t.Cleanup(func() {
		viper.Set(CrusoeProjectIDFlag, "")
		viper.Set(CrusoeSubnetIDFlag, "")
	})

	cfg := crusoeapi.NewConfiguration()
	cfg.BasePath = srv.URL
	cfg.HTTPClient = srv.Client()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to build scheme: %v", err)
	}

	return &ServiceReconciler{
		Client:             fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, readyNode()).Build(),
		Scheme:             scheme,
		CrusoeClient:       crusoeapi.NewAPIClient(cfg),
		LoadBalancerLookup: &crusoe.LoadBalancerLookup{BasePath: srv.URL, HTTPClient: srv.Client()},
	}
}

// TestEnsureFirewallRuleAdoptsRuleWhenOperationGone covers the recovery this change exists for:
// the operation record has aged out of the operations store, so its id can never yield a rule
// id, but the rule it created is still there under the Service's deterministic name. The
// controller must adopt that rule rather than submit a second create for the same name, and
// must drop the operation id it can no longer resolve.
func TestEnsureFirewallRuleAdoptsRuleWhenOperationGone(t *testing.T) {
	ruleName, err := GenerateLoadBalancerResourceName("lb-udp-test", "probe", "22222222-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatalf("failed to generate rule name: %v", err)
	}

	const existingRuleID = "88888888-8888-8888-8888-888888888888"

	var createAttempts int

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			createAttempts++
			w.WriteHeader(http.StatusInternalServerError)

		case r.URL.Path == "/projects/"+testProjectID+"/networking/vpc-subnets/"+testSubnetID:
			writeJSON(w, `{"id":"`+testSubnetID+`","vpc_network_id":"`+testVPCID+`","location":"us-east1-a"}`)

		case r.URL.Path == "/projects/"+testProjectID+"/networking/vpc-firewall-rules/operations/"+testOperationID:
			// Aged out of the operations store: unresolvable, not transient.
			w.WriteHeader(http.StatusNotFound)

		case r.URL.Path == "/projects/"+testProjectID+"/networking/vpc-firewall-rules":
			writeJSON(w, `{"items":[{"id":"`+existingRuleID+`","name":"`+ruleName+`"}]}`)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	svc := liveService(map[string]string{
		loadbalancerIDLabelKey:     "99999999-9999-9999-9999-999999999999",
		FirewallRuleOperationIdKey: testOperationID,
	})

	reconciler := newEnsureTestReconciler(t, handler, svc)

	requeueAfter, err := reconciler.ensureFirewallRule(context.Background(), svc)
	if err != nil {
		t.Fatalf("adopting an existing rule must succeed, got error: %v", err)
	}

	if requeueAfter != 0 {
		t.Errorf("an adopted rule is fully settled, so no further reconcile is needed, got a requeue after %s", requeueAfter)
	}

	if got := svc.Annotations[FirewallRuleIdKey]; got != existingRuleID {
		t.Errorf("expected the existing rule to be adopted as %s, got %q", existingRuleID, got)
	}

	if _, stillThere := svc.Annotations[FirewallRuleOperationIdKey]; stillThere {
		t.Error("the unresolvable operation id must be dropped once the rule is adopted")
	}

	if createAttempts != 0 {
		t.Errorf("a rule already exists under this name; no create may be issued, got %d", createAttempts)
	}
}

// TestEnsureFirewallRuleWaitsBeforeRetryingFailedCreate pins the cooldown after a rule's create
// operation fails. Inside it nothing may be submitted, the failed operation's id must stay on
// the Service, and the pass must come back when the cooldown ends. After it, exactly one new
// create is submitted. Resubmitting on every pass used to turn a create that always fails into
// a new one every ~10 seconds, forever.
func TestEnsureFirewallRuleWaitsBeforeRetryingFailedCreate(t *testing.T) {
	const newOperationID = "dddddddd-dddd-dddd-dddd-dddddddddddd"

	tests := []struct {
		name        string
		failedAgo   time.Duration
		wantCreates int
	}{
		{name: "inside the cooldown", failedAgo: 10 * time.Second, wantCreates: 0},
		{name: "after the cooldown", failedAgo: firewallRuleCreateRetryDelay + time.Minute, wantCreates: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failedAt := time.Now().Add(-tt.failedAgo).UTC().Format(time.RFC3339Nano)

			var creates int

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/projects/"+testProjectID+"/networking/vpc-subnets/"+testSubnetID:
					writeJSON(w, `{"id":"`+testSubnetID+`","vpc_network_id":"`+testVPCID+`","location":"us-east1-a"}`)

				case r.URL.Path == "/projects/"+testProjectID+"/networking/vpc-firewall-rules/operations/"+testOperationID:
					writeJSON(w, `{"operation_id":"`+testOperationID+`","state":"FAILED","started_at":"`+failedAt+`",`+
						`"completed_at":"`+failedAt+`","metadata":null,"result":{"error":"quota exceeded"}}`)

				case r.Method == http.MethodPost && r.URL.Path == "/projects/"+testProjectID+"/networking/vpc-firewall-rules":
					creates++
					writeJSON(w, `{"operation":{"operation_id":"`+newOperationID+`","state":"IN_PROGRESS",`+
						`"started_at":"","completed_at":"","metadata":null}}`)

				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			})

			svc := liveService(map[string]string{
				loadbalancerIDLabelKey:     "99999999-9999-9999-9999-999999999999",
				FirewallRuleOperationIdKey: testOperationID,
			})

			reconciler := newEnsureTestReconciler(t, handler, svc)

			requeueAfter, err := reconciler.ensureFirewallRule(context.Background(), svc)
			if err != nil {
				t.Fatalf("a failed create operation is handled by waiting, not an error, got: %v", err)
			}

			if creates != tt.wantCreates {
				t.Errorf("expected %d create(s), got %d", tt.wantCreates, creates)
			}

			if requeueAfter <= 0 || requeueAfter > firewallRuleCreateRetryDelay {
				t.Errorf("expected a requeue within the retry delay %s, got %s", firewallRuleCreateRetryDelay, requeueAfter)
			}

			wantOperationID := testOperationID
			if tt.wantCreates > 0 {
				wantOperationID = newOperationID
			}

			if got := svc.Annotations[FirewallRuleOperationIdKey]; got != wantOperationID {
				t.Errorf("expected operation id %s on the Service, got %q", wantOperationID, got)
			}
		})
	}
}

// TestEnsureFirewallRuleDistinguishesSkipFromUnknown pins the difference between "nothing to
// do" and "could not find out". A failed VPC lookup must come back as an error so the pass is
// retried, and must leave a pending operation id alone; it used to be treated as a skip, which
// left the Service without its firewall rule until an unrelated event came along. A missing
// project id is a genuine skip: no error, no requeue, no API call.
func TestEnsureFirewallRuleDistinguishesSkipFromUnknown(t *testing.T) {
	t.Run("VPC lookup fails", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/projects/"+testProjectID+"/networking/vpc-subnets/"+testSubnetID {
				w.WriteHeader(http.StatusServiceUnavailable)

				return
			}

			t.Errorf("nothing may be read or created without the VPC, got %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		})

		svc := liveService(map[string]string{
			loadbalancerIDLabelKey:     testLoadBalancerID,
			FirewallRuleOperationIdKey: testOperationID,
		})

		reconciler := newEnsureTestReconciler(t, handler, svc)

		if _, err := reconciler.ensureFirewallRule(context.Background(), svc); err == nil {
			t.Fatal("a failed VPC lookup must be returned as an error so the pass is retried")
		}

		if got := svc.Annotations[FirewallRuleOperationIdKey]; got != testOperationID {
			t.Errorf("the pending operation id must be left alone, got %q", got)
		}
	})

	t.Run("project id not set", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("a deliberate skip makes no API call, got %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		})

		svc := liveService(map[string]string{loadbalancerIDLabelKey: testLoadBalancerID})

		reconciler := newEnsureTestReconciler(t, handler, svc)
		viper.Set(CrusoeProjectIDFlag, "")

		requeueAfter, err := reconciler.ensureFirewallRule(context.Background(), svc)
		if err != nil || requeueAfter != 0 {
			t.Errorf("a missing project id is a skip: want no error and no requeue, got %s, %v", requeueAfter, err)
		}
	})
}

// TestFirewallRuleRetryWait covers the timing edge cases: the wait counts from completion (or
// start, if completion is missing), never blocks a retry for want of a usable timestamp, and
// cannot be stretched by a server clock running ahead.
func TestFirewallRuleRetryWait(t *testing.T) {
	now := time.Now()
	stamp := func(offset time.Duration) string { return now.Add(offset).UTC().Format(time.RFC3339Nano) }

	tests := []struct {
		name string
		op   crusoeapi.Operation
		want time.Duration
	}{
		{name: "just failed", op: crusoeapi.Operation{CompletedAt: stamp(0)}, want: firewallRuleCreateRetryDelay},
		{name: "cooldown passed", op: crusoeapi.Operation{CompletedAt: stamp(-firewallRuleCreateRetryDelay - time.Second)}, want: 0},
		{name: "start time used when completion is missing", op: crusoeapi.Operation{StartedAt: stamp(-time.Minute)}, want: firewallRuleCreateRetryDelay - time.Minute},
		{name: "no usable timestamp", op: crusoeapi.Operation{CompletedAt: "not a time"}, want: 0},
		{name: "server clock ahead", op: crusoeapi.Operation{CompletedAt: stamp(time.Hour)}, want: firewallRuleCreateRetryDelay},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firewallRuleRetryWait(&tt.op, now); got != tt.want {
				t.Errorf("firewallRuleRetryWait() = %s, want %s", got, tt.want)
			}
		})
	}
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}
