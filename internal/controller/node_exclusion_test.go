package controller

import (
	"context"
	"slices"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestBackendsRespectExcludeFromExternalLoadBalancers(t *testing.T) {
	node := func(name, ip string, labels map[string]string) *corev1.Node {
		return &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
				Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}},
			},
		}
	}
	exclude := func(v string) map[string]string {
		return map[string]string{corev1.LabelNodeExcludeBalancers: v}
	}

	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		node("unlabelled", "10.0.0.1", nil),
		node("excluded-true", "10.0.0.2", exclude("true")),
		node("excluded-false", "10.0.0.3", exclude("false")),
		node("excluded-empty", "10.0.0.4", exclude("")),
		node("excluded-garbage", "10.0.0.5", exclude("yes-please")),
	).Build()
	r := &ServiceReconciler{Client: c}
	svc := &corev1.Service{Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 443, NodePort: 30443}}}}

	got := r.parseListenPortsAndBackends(context.Background(), svc.Spec.Ports, logr.Discard())
	if len(got) != 1 {
		t.Fatalf("want 1 listen port, got %d", len(got))
	}
	var ips []string
	for _, b := range got[0].Backends {
		ips = append(ips, b.Ip)
	}
	// Upstream semantics: any value that doesn't parse as false excludes the node.
	slices.Sort(ips)
	if want := []string{"10.0.0.1", "10.0.0.3"}; !slices.Equal(ips, want) {
		t.Fatalf("backends = %v, want [10.0.0.1 10.0.0.3]", ips)
	}
}

func TestNodeEventsFromExcludedNodesSkipReconcile(t *testing.T) {
	node := func(excluded, ready bool) *corev1.Node {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}}
		if excluded {
			n.Labels = map[string]string{corev1.LabelNodeExcludeBalancers: "true"}
		}
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
		return n
	}
	p := nodeEventPredicate()
	update := func(old, new *corev1.Node) bool {
		return p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: new})
	}

	for _, tc := range []struct {
		name string
		got  bool
		want bool
	}{
		{"create included", p.Create(event.CreateEvent{Object: node(false, false)}), true},
		{"create excluded", p.Create(event.CreateEvent{Object: node(true, false)}), false},
		{"delete included", p.Delete(event.DeleteEvent{Object: node(false, true)}), true},
		{"delete excluded", p.Delete(event.DeleteEvent{Object: node(true, true)}), true},
		{"included becomes ready", update(node(false, false), node(false, true)), true},
		{"excluded becomes ready", update(node(true, false), node(true, true)), false},
		{"exclusion added", update(node(false, true), node(true, true)), true},
		{"exclusion removed", update(node(true, true), node(false, true)), true},
		{"no change", update(node(false, true), node(false, true)), false},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: reconcile = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}
