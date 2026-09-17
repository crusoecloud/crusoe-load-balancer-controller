package crusoe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testProjectID = "11111111-1111-1111-1111-111111111111"

func newTestLookup(t *testing.T, handler http.HandlerFunc) *LoadBalancerLookup {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return &LoadBalancerLookup{BasePath: srv.URL, HTTPClient: srv.Client()}
}

// TestByNameMatchesExactly checks both halves of the lookup: it asks the server to filter by
// name, and it does not trust the server to have done so.
func TestByNameMatchesExactly(t *testing.T) {
	lookup := newTestLookup(t, func(w http.ResponseWriter, r *http.Request) {
		if want := "/projects/" + testProjectID + "/networking/load-balancers"; r.URL.Path != want {
			t.Errorf("expected a request to %s, got %s", want, r.URL.Path)
		}

		if got := r.URL.Query().Get("name"); got != "svc-lb" {
			t.Errorf("expected the name filter svc-lb, got %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"a","name":"svc-lb-other"},{"id":"b","name":"svc-lb"}]}`))
	})

	matches, err := lookup.ByName(context.Background(), testProjectID, "svc-lb")
	if err != nil {
		t.Fatalf("lookup failed: %v", err)
	}

	if len(matches) != 1 || matches[0].Id != "b" {
		t.Errorf("expected only the exact match b, got %+v", matches)
	}
}

// TestByNameFailsOnUnreadableAnswer is the property the generated client lacks. It returns an
// empty list with no error when the body does not decode, which would make every lookup look
// like "no such load balancer" and cause the duplicate it is meant to prevent.
func TestByNameFailsOnUnreadableAnswer(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"body is not the expected shape": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"items":[]}]`))
		},
		"server error": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	} {
		t.Run(name, func(t *testing.T) {
			lookup := newTestLookup(t, handler)

			if matches, err := lookup.ByName(context.Background(), testProjectID, "svc-lb"); err == nil {
				t.Errorf("expected an error, got %d matches and no error", len(matches))
			}
		})
	}
}
