package controller

import (
	"context"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
)

// noAPICalls fails the test on any request to the Crusoe API.
func noAPICalls(t *testing.T) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no Crusoe API call was expected, got %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
}

// expectWarning asserts that exactly one Event was recorded, a Warning with the given reason
// whose message contains want.
func expectWarning(t *testing.T, recorder *record.FakeRecorder, reason, want string) {
	t.Helper()

	select {
	case event := <-recorder.Events:
		if prefix := corev1.EventTypeWarning + " " + reason + " "; !strings.HasPrefix(event, prefix) || !strings.Contains(event, want) {
			t.Errorf("expected a %s Warning containing %q, got %q", reason, want, event)
		}
	default:
		t.Fatalf("expected a %s Warning, got no Event", reason)
	}

	select {
	case event := <-recorder.Events:
		t.Errorf("expected a single Event, also got %q", event)
	default:
	}
}

// TestHandleCreateRecordsRejectionEvent checks that a Service no load balancer can serve is
// rejected without a retry or any API call, and says so where its owner can see it: an
// UnsupportedPortProtocol Warning in `kubectl describe svc`, not only a controller log line
// that customers on managed clusters may not be able to read.
func TestHandleCreateRecordsRejectionEvent(t *testing.T) {
	tests := []struct {
		name        string
		ports       []corev1.ServicePort
		wantMessage string
	}{
		{
			name: "TCP and UDP mixed",
			ports: []corev1.ServicePort{
				{Port: 53, NodePort: 30053, Protocol: corev1.ProtocolTCP},
				{Port: 53, NodePort: 30530, Protocol: corev1.ProtocolUDP},
			},
			wantMessage: "Split the Service into one Service per protocol",
		},
		{
			name:        "SCTP",
			ports:       []corev1.ServicePort{{Port: 38412, NodePort: 30412, Protocol: corev1.ProtocolSCTP}},
			wantMessage: "SCTP",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := loadBalancerOnlyService(map[string]string{})
			svc.Spec.Ports = tt.ports

			reconciler := newEnsureTestReconciler(t, noAPICalls(t), svc)
			recorder := record.NewFakeRecorder(4)
			reconciler.Recorder = recorder

			result, err := reconciler.handleCreate(context.Background(), svc)
			if err != nil || !result.IsZero() {
				t.Fatalf("a rejected Service is final, not retried: got %+v, %v", result, err)
			}

			expectWarning(t, recorder, EventReasonUnsupportedPortProtocol, tt.wantMessage)
		})
	}
}

// TestHandleCreateReportsFailedCreateOperation covers a create operation that finishes FAILED,
// for example a protocol the location does not offer. It used to return a nil error: no retry,
// and nothing to explain why the Service stayed <pending>. It must now return an error, so the
// manager retries with backoff, record a LoadBalancerCreateFailed Warning carrying the
// operation's reason, and record no load balancer on the Service.
func TestHandleCreateReportsFailedCreateOperation(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/projects/"+testProjectID+"/networking/vpc-subnets/"+testSubnetID:
			writeJSON(w, `{"id":"`+testSubnetID+`","vpc_network_id":"`+testVPCID+`","location":"us-east1-a"}`)

		case r.Method == http.MethodGet && r.URL.Path == loadBalancerCollectionPath():
			writeJSON(w, `{"items":[]}`)

		case r.Method == http.MethodPost && r.URL.Path == loadBalancerCollectionPath():
			writeJSON(w, `{"operation":{"operation_id":"eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee","state":"FAILED",`+
				`"started_at":"","completed_at":"","metadata":null,`+
				`"result":{"error":"UDP load balancers are not offered in this location"}}}`)

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	svc := loadBalancerOnlyService(map[string]string{})

	reconciler := newEnsureTestReconciler(t, handler, svc)
	recorder := record.NewFakeRecorder(4)
	reconciler.Recorder = recorder

	if _, err := reconciler.handleCreate(context.Background(), svc); err == nil {
		t.Fatal("a failed create operation must return an error so the manager retries")
	}

	expectWarning(t, recorder, EventReasonLoadBalancerCreateFailed, "not offered in this location")

	if id, recorded := svc.Annotations[loadbalancerIDLabelKey]; recorded {
		t.Errorf("no load balancer exists, so none may be recorded, got %q", id)
	}
}

// TestHandleUpdateRecordsProtocolDrift covers a Service edited to a protocol its load balancer
// cannot adopt. It must get a ProtocolDrift Warning telling its owner to recreate it, and the
// load balancer must be left alone: with no port matching its protocol, no update may be sent,
// since the API rejects an empty listener list.
func TestHandleUpdateRecordsProtocolDrift(t *testing.T) {
	// A UDP port on a load balancer created for TCP.
	svc := loadBalancerOnlyService(map[string]string{
		loadbalancerIDLabelKey:         testLoadBalancerID,
		AnnotationLoadBalancerProtocol: LoadBalancerProtocolTCP,
	})
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: testVIP}}

	reconciler := newEnsureTestReconciler(t, noAPICalls(t), svc)
	recorder := record.NewFakeRecorder(4)
	reconciler.Recorder = recorder

	if _, err := reconciler.handleUpdate(context.Background(), svc); err != nil {
		t.Fatalf("handleUpdate failed: %v", err)
	}

	expectWarning(t, recorder, EventReasonProtocolDrift, "Recreate the Service to use UDP")
}
