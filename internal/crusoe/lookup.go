package crusoe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
)

var errUnexpectedListStatus = errors.New("unexpected status listing load balancers")

// LoadBalancerLookup finds external load balancers by name by calling the Crusoe API directly.
//
// It exists because the generated client in use, client-go v0.1.78, declares the wrong response
// type for ListExternalLoadBalancers: the API returns one {"items": [...]} object, but the client
// expects a list of them. The generated code ignores decode errors on a successful response, so
// that call reports an empty list and no error, and any lookup built on it always misses. Remove
// this once the client is upgraded (CRUSOE-111359); v1.0.8 has the correct type.
type LoadBalancerLookup struct {
	// BasePath is the API endpoint, the same value the generated client is configured with.
	BasePath string
	// HTTPClient must sign requests; see NewHTTPClient.
	HTTPClient *http.Client
	UserAgent  string
}

type listExternalLoadBalancersResponse struct {
	Items []crusoeapi.ExternalLoadBalancer `json:"items"`
}

// ByName returns the project's external load balancers named exactly name.
func (l *LoadBalancerLookup) ByName(ctx context.Context, projectID, name string) ([]crusoeapi.ExternalLoadBalancer, error) {
	endpoint, err := url.JoinPath(l.BasePath, "projects", projectID, "networking", "load-balancers")
	if err != nil {
		return nil, fmt.Errorf("failed to build load balancer list URL: %w", err)
	}

	endpoint += "?" + url.Values{"name": {name}}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build load balancer list request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	if l.UserAgent != "" {
		req.Header.Set("User-Agent", l.UserAgent)
	}

	resp, err := l.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to list load balancers: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s", errUnexpectedListStatus, resp.Status)
	}

	// Unlike the generated client, fail on a body that does not decode: an empty result must
	// mean "no such load balancer", never "could not read the answer".
	var body listExternalLoadBalancersResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("failed to decode load balancer list: %w", err)
	}

	// Match exactly as well, in case the server treats the name filter loosely or ignores it.
	matches := make([]crusoeapi.ExternalLoadBalancer, 0, len(body.Items))
	for _, lb := range body.Items {
		if lb.Name == name {
			matches = append(matches, lb)
		}
	}

	return matches, nil
}
