package controller

import (
	"context"
	"net/http"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestGetFirewallRuleArgsFollowsLoadBalancerProtocol checks that the firewall rule covers the
// load balancer's own protocol, not whatever the Service's ports say. A rule that followed a
// protocol edit would cut traffic to a load balancer that is still serving. Without a recorded
// protocol (a legacy load balancer whose protocol could not be read) every port is covered, as
// before UDP support. When no port matches, the rule is left as it is.
func TestGetFirewallRuleArgsFollowsLoadBalancerProtocol(t *testing.T) {
	tcp := corev1.ServicePort{Port: 53, NodePort: 30053, Protocol: corev1.ProtocolTCP}
	udp := corev1.ServicePort{Port: 53, NodePort: 30530, Protocol: corev1.ProtocolUDP}

	tests := []struct {
		name          string
		lbProtocol    string // recorded on the Service; empty for a legacy load balancer
		ports         []corev1.ServicePort
		wantSkip      bool
		wantProtocols []string
		wantPorts     []string
	}{
		{
			name:          "pre-UDP load balancer (no recorded protocol) covers every port, as before",
			ports:         []corev1.ServicePort{tcp, udp},
			wantProtocols: []string{"tcp", "udp"},
			wantPorts:     []string{"30053", "30530"},
		},
		{
			name:          "TCP load balancer covers only its TCP port",
			lbProtocol:    LoadBalancerProtocolTCP,
			ports:         []corev1.ServicePort{tcp, udp},
			wantProtocols: []string{"tcp"},
			wantPorts:     []string{"30053"},
		},
		{
			name:          "UDP load balancer covers only its UDP port",
			lbProtocol:    LoadBalancerProtocolUDP,
			ports:         []corev1.ServicePort{tcp, udp},
			wantProtocols: []string{"udp"},
			wantPorts:     []string{"30530"},
		},
		{
			name:       "no port matches, rule left unchanged",
			lbProtocol: LoadBalancerProtocolTCP,
			ports:      []corev1.ServicePort{udp},
			wantSkip:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/projects/"+testProjectID+"/networking/vpc-subnets/"+testSubnetID {
					writeJSON(w, `{"id":"`+testSubnetID+`","vpc_network_id":"`+testVPCID+`","location":"us-east1-a"}`)

					return
				}

				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			})

			annotations := map[string]string{loadbalancerIDLabelKey: testLoadBalancerID}
			if tt.lbProtocol != "" {
				annotations[AnnotationLoadBalancerProtocol] = tt.lbProtocol
			}

			svc := liveService(annotations)
			svc.Spec.Ports = tt.ports

			reconciler := newEnsureTestReconciler(t, handler, svc)

			args, err := reconciler.GetFirewallRuleArgs(context.Background(), svc)
			if err != nil {
				t.Fatalf("GetFirewallRuleArgs failed: %v", err)
			}

			if tt.wantSkip {
				if args != nil {
					t.Errorf("expected the rule to be left unchanged, got args %+v", args)
				}

				return
			}

			if args == nil {
				t.Fatal("expected firewall rule args, got a skip")

				return // unreachable; tells staticcheck args is non-nil below (SA5011)
			}

			if !slices.Equal(args.protocols, tt.wantProtocols) {
				t.Errorf("protocols = %v, want %v", args.protocols, tt.wantProtocols)
			}

			if !slices.Equal(args.destinationPorts, tt.wantPorts) {
				t.Errorf("destination ports = %v, want %v", args.destinationPorts, tt.wantPorts)
			}
		})
	}
}
