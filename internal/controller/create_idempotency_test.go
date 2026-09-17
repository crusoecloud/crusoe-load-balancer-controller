package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	testLoadBalancerID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	testVIP            = "203.0.113.10"
)

// loadBalancerOnlyService is liveService without firewall management, so only the load
// balancer paths run and the fake API needs no firewall routes.
func loadBalancerOnlyService(annotations map[string]string) *corev1.Service {
	svc := liveService(annotations)
	delete(svc.Annotations, ManageFirewallRuleKey)

	return svc
}

func loadBalancerCollectionPath() string {
	return "/projects/" + testProjectID + "/networking/load-balancers"
}

func loadBalancerPath() string {
	return loadBalancerCollectionPath() + "/" + testLoadBalancerID
}

// TestHandleCreateWaitsWhenCacheIsStale covers the stale-cache race: the reconcile was handed
// a cached Service without the load balancer id, but the API server already has it. Nothing
// may be created; the reconcile must come back once the cache has caught up.
func TestHandleCreateWaitsWhenCacheIsStale(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no Crusoe API call may be made while the cache is stale, got %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	cached := loadBalancerOnlyService(map[string]string{})
	reconciler := newEnsureTestReconciler(t, handler, cached)

	fresh := cached.DeepCopy()
	fresh.Annotations[loadbalancerIDLabelKey] = testLoadBalancerID
	reconciler.APIReader = fake.NewClientBuilder().WithScheme(reconciler.Scheme).WithObjects(fresh).Build()

	result, err := reconciler.handleCreate(context.Background(), cached)
	if err != nil {
		t.Fatalf("a stale cache is not an error, got: %v", err)
	}

	if result.RequeueAfter == 0 {
		t.Errorf("expected a requeue so the cache can catch up, got %+v", result)
	}
}

// TestHandleCreateRecordsIDBeforeExternalIP pins the write order. If publishing the external
// IP fails, the load balancer id must already be on the Service, so the retry updates this
// load balancer instead of creating a second one.
func TestHandleCreateRecordsIDBeforeExternalIP(t *testing.T) {
	var creates int

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/projects/"+testProjectID+"/networking/vpc-subnets/"+testSubnetID:
			writeJSON(w, `{"id":"`+testSubnetID+`","vpc_network_id":"`+testVPCID+`","location":"us-east1-a"}`)

		case r.Method == http.MethodGet && r.URL.Path == loadBalancerCollectionPath():
			// The adoption check finds nothing, so a load balancer is created.
			writeJSON(w, `{"items":[]}`)

		case r.Method == http.MethodPost && r.URL.Path == loadBalancerCollectionPath():
			creates++
			// Already settled, so WaitForOperation returns without polling.
			writeJSON(w, `{"operation":{"operation_id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","state":"SUCCEEDED",`+
				`"started_at":"","completed_at":"","metadata":null,`+
				`"result":{"id":"`+testLoadBalancerID+`","vip":"`+testVIP+`"}}}`)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	svc := loadBalancerOnlyService(map[string]string{})
	reconciler := newEnsureTestReconciler(t, handler, svc)

	errStatusWrite := errors.New("status write failed")
	reconciler.Client = fake.NewClientBuilder().
		WithScheme(reconciler.Scheme).
		WithObjects(svc, readyNode()).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
				return errStatusWrite
			},
		}).
		Build()

	if _, err := reconciler.handleCreate(context.Background(), svc); !errors.Is(err, errStatusWrite) {
		t.Fatalf("expected the failed status write to be returned, got: %v", err)
	}

	stored := &corev1.Service{}
	if err := reconciler.Client.Get(context.Background(), client.ObjectKeyFromObject(svc), stored); err != nil {
		t.Fatalf("failed to read back the Service: %v", err)
	}

	if got := stored.Annotations[loadbalancerIDLabelKey]; got != testLoadBalancerID {
		t.Errorf("the load balancer id must be recorded before the external IP is published, got %q", got)
	}

	if creates != 1 {
		t.Errorf("expected exactly one load balancer create, got %d", creates)
	}
}

// TestHandleCreateAdoptsExistingLoadBalancer covers the failure the fresh read cannot catch: a
// load balancer was created, but its id never reached the Service, so the API server has no id
// either. The load balancer carrying the Service's name must be adopted, with its own protocol
// and VIP, and no second one created. A load balancer whose name only starts with the
// Service's must be ignored.
func TestHandleCreateAdoptsExistingLoadBalancer(t *testing.T) {
	svc := loadBalancerOnlyService(map[string]string{})

	lbName, err := GenerateLoadBalancerResourceName(svc.Namespace, svc.Name, string(svc.UID))
	if err != nil {
		t.Fatalf("failed to generate load balancer name: %v", err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/projects/"+testProjectID+"/networking/vpc-subnets/"+testSubnetID:
			writeJSON(w, `{"id":"`+testSubnetID+`","vpc_network_id":"`+testVPCID+`","location":"us-east1-a"}`)

		case r.Method == http.MethodGet && r.URL.Path == loadBalancerCollectionPath():
			if got := r.URL.Query().Get("name"); got != lbName {
				t.Errorf("expected the lookup to filter by name %q, got %q", lbName, got)
			}

			writeJSON(w, `{"items":[`+
				`{"id":"cccccccc-cccc-cccc-cccc-cccccccccccc","name":"`+lbName+`-other","vip":"198.51.100.1"},`+
				`{"id":"`+testLoadBalancerID+`","name":"`+lbName+`","vip":"`+testVIP+`","protocol":"`+LoadBalancerProtocolUDP+`"}]}`)

		default:
			t.Errorf("nothing may be created when the load balancer already exists, got %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	reconciler := newEnsureTestReconciler(t, handler, svc)
	reconciler.Client = fake.NewClientBuilder().
		WithScheme(reconciler.Scheme).
		WithObjects(svc, readyNode()).
		WithStatusSubresource(&corev1.Service{}).
		Build()

	if _, err := reconciler.handleCreate(context.Background(), svc); err != nil {
		t.Fatalf("adopting an existing load balancer must succeed, got: %v", err)
	}

	stored := &corev1.Service{}
	if err := reconciler.Client.Get(context.Background(), client.ObjectKeyFromObject(svc), stored); err != nil {
		t.Fatalf("failed to read back the Service: %v", err)
	}

	if got := stored.Annotations[loadbalancerIDLabelKey]; got != testLoadBalancerID {
		t.Errorf("expected the existing load balancer %s to be adopted, got %q", testLoadBalancerID, got)
	}

	if got := stored.Annotations[AnnotationLoadBalancerProtocol]; got != LoadBalancerProtocolUDP {
		t.Errorf("expected the adopted load balancer's protocol to be recorded, got %q", got)
	}

	if ingress := stored.Status.LoadBalancer.Ingress; len(ingress) != 1 || ingress[0].IP != testVIP {
		t.Errorf("expected the adopted load balancer's VIP %s as the external IP, got %+v", testVIP, ingress)
	}
}

// TestHandleUpdateRestoresMissingExternalIP covers the recovery for
// TestHandleCreateRecordsIDBeforeExternalIP: a Service that records a load balancer but has no
// external IP gets the VIP back from the API.
func TestHandleUpdateRestoresMissingExternalIP(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == loadBalancerPath():
			writeJSON(w, `{"id":"`+testLoadBalancerID+`","vip":"`+testVIP+`"}`)

		case r.Method == http.MethodPatch && r.URL.Path == loadBalancerPath():
			writeJSON(w, `{}`)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	svc := loadBalancerOnlyService(map[string]string{loadbalancerIDLabelKey: testLoadBalancerID})
	reconciler := newEnsureTestReconciler(t, handler, svc)

	// Status writes need the status subresource registered on the fake client.
	reconciler.Client = fake.NewClientBuilder().
		WithScheme(reconciler.Scheme).
		WithObjects(svc, readyNode()).
		WithStatusSubresource(&corev1.Service{}).
		Build()

	if _, err := reconciler.handleUpdate(context.Background(), svc); err != nil {
		t.Fatalf("handleUpdate failed: %v", err)
	}

	stored := &corev1.Service{}
	if err := reconciler.Client.Get(context.Background(), client.ObjectKeyFromObject(svc), stored); err != nil {
		t.Fatalf("failed to read back the Service: %v", err)
	}

	if ingress := stored.Status.LoadBalancer.Ingress; len(ingress) != 1 || ingress[0].IP != testVIP {
		t.Errorf("expected the external IP %s to be restored, got %+v", testVIP, ingress)
	}
}

// TestHandleUpdateLeavesExistingExternalIPAlone checks that the restore costs nothing in the
// normal case: a Service that already has its external IP triggers no load balancer read.
func TestHandleUpdateLeavesExistingExternalIPAlone(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == loadBalancerPath() {
			writeJSON(w, `{}`)

			return
		}

		t.Errorf("only the listener update may be sent, got %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	svc := loadBalancerOnlyService(map[string]string{loadbalancerIDLabelKey: testLoadBalancerID})
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: testVIP}}

	reconciler := newEnsureTestReconciler(t, handler, svc)

	if _, err := reconciler.handleUpdate(context.Background(), svc); err != nil {
		t.Fatalf("handleUpdate failed: %v", err)
	}
}

// listenersInUpdate returns how many listeners a load balancer update request carries.
func listenersInUpdate(t *testing.T, r *http.Request) int {
	t.Helper()

	var body struct {
		ListenPortsAndBackends []json.RawMessage `json:"listen_ports_and_backends"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("failed to decode the listener update: %v", err)
	}

	return len(body.ListenPortsAndBackends)
}

// TestHandleUpdateLeavesLegacyServiceAsOnMain pins the upgrade guarantee. A Service whose load
// balancer was created before UDP support carries no protocol annotation, and must get exactly
// what the previous controller gave it: every port as a listener, every port in the firewall
// rule, and nothing written to the Service. The mixed TCP and UDP case matters most: narrowing
// its firewall rule on upgrade broke UDP sent straight to the nodes, the obvious workaround for
// anyone who hit the original bug.
func TestHandleUpdateLeavesLegacyServiceAsOnMain(t *testing.T) {
	listeners := -1

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/projects/"+testProjectID+"/networking/vpc-subnets/"+testSubnetID:
			writeJSON(w, `{"id":"`+testSubnetID+`","vpc_network_id":"`+testVPCID+`","location":"us-east1-a"}`)

		case r.Method == http.MethodPatch && r.URL.Path == loadBalancerPath():
			listeners = listenersInUpdate(t, r)
			writeJSON(w, `{}`)

		default:
			// In particular no read of the load balancer: nothing about it is looked up or
			// recorded on upgrade.
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	svc := loadBalancerOnlyService(map[string]string{loadbalancerIDLabelKey: testLoadBalancerID})
	svc.Spec.Ports = []corev1.ServicePort{
		{Port: 53, NodePort: 30053, Protocol: corev1.ProtocolTCP},
		{Port: 54, NodePort: 30054, Protocol: corev1.ProtocolUDP},
	}
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: testVIP}}

	reconciler := newEnsureTestReconciler(t, handler, svc)

	if _, err := reconciler.handleUpdate(context.Background(), svc); err != nil {
		t.Fatalf("handleUpdate failed: %v", err)
	}

	if listeners != 2 {
		t.Errorf("expected every port as a listener (2), got %d", listeners)
	}

	stored := &corev1.Service{}
	if err := reconciler.Client.Get(context.Background(), client.ObjectKeyFromObject(svc), stored); err != nil {
		t.Fatalf("failed to read back the Service: %v", err)
	}

	if got, recorded := stored.Annotations[AnnotationLoadBalancerProtocol]; recorded {
		t.Errorf("nothing may be recorded on a pre-UDP Service, got protocol %q", got)
	}

	args, err := reconciler.GetFirewallRuleArgs(context.Background(), stored)
	if err != nil {
		t.Fatalf("GetFirewallRuleArgs failed: %v", err)
	}

	if args == nil {
		t.Fatal("expected firewall rule args, got a skip")

		return // unreachable; tells staticcheck args is non-nil below (SA5011)
	}

	if want := []string{"tcp", "udp"}; !slices.Equal(args.protocols, want) {
		t.Errorf("firewall protocols = %v, want %v, as before the upgrade", args.protocols, want)
	}

	if want := []string{"30053", "30054"}; !slices.Equal(args.destinationPorts, want) {
		t.Errorf("firewall ports = %v, want %v, as before the upgrade", args.destinationPorts, want)
	}
}

// TestHandleUpdateDoesNotWaitForExternalIP checks that restoring a missing external IP never
// holds up the rest of the update. When the load balancer has no VIP yet (an adopted one still
// provisioning) or cannot be read, the listeners are still updated, no error is returned, and
// the pass comes back on the external IP's own interval rather than backing off.
func TestHandleUpdateDoesNotWaitForExternalIP(t *testing.T) {
	tests := []struct {
		name  string
		lbGet func(w http.ResponseWriter)
	}{
		{
			name:  "load balancer has no VIP yet",
			lbGet: func(w http.ResponseWriter) { writeJSON(w, `{"id":"`+testLoadBalancerID+`","vip":""}`) },
		},
		{
			name:  "load balancer cannot be read",
			lbGet: func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listeners := -1

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == loadBalancerPath():
					tt.lbGet(w)

				case r.Method == http.MethodPatch && r.URL.Path == loadBalancerPath():
					listeners = listenersInUpdate(t, r)
					writeJSON(w, `{}`)

				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusInternalServerError)
				}
			})

			// No external IP yet.
			svc := loadBalancerOnlyService(map[string]string{loadbalancerIDLabelKey: testLoadBalancerID})

			reconciler := newEnsureTestReconciler(t, handler, svc)

			result, err := reconciler.handleUpdate(context.Background(), svc)
			if err != nil {
				t.Fatalf("a missing external IP must not fail the update, got: %v", err)
			}

			if listeners != 1 {
				t.Errorf("the listener update must still be sent, got %d listener(s)", listeners)
			}

			if result.RequeueAfter != loadBalancerIngressRequeueInterval {
				t.Errorf("expected a requeue after %s to try the external IP again, got %+v",
					loadBalancerIngressRequeueInterval, result)
			}
		})
	}
}
