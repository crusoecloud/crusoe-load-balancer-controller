package crusoe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// recordingTransport answers every request with a canned response or error and
// remembers the last request it saw, including its body.
type recordingTransport struct {
	status   int
	err      error
	requests int
	lastBody string
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.requests++
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		rt.lastBody = string(body)
	}

	if rt.err != nil {
		return nil, rt.err
	}

	return &http.Response{StatusCode: rt.status, Body: http.NoBody}, nil
}

func newTestRequest(t *testing.T, body string) *http.Request {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://api.example/v1alpha5/things", reader)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}

	return req
}

func TestFallbackTransportPassesThroughSuccess(t *testing.T) {
	primary := &recordingTransport{status: http.StatusOK}
	fallback := &recordingTransport{status: http.StatusOK}
	transport := &fallbackTransport{primary: primary, fallback: fallback}

	resp, err := transport.RoundTrip(newTestRequest(t, ""))
	if err != nil {
		t.Fatalf("round trip failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if fallback.requests != 0 {
		t.Errorf("fallback must not run on success, ran %d times", fallback.requests)
	}
}

func TestFallbackTransportRetriesOnUnauthorized(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		primary := &recordingTransport{status: status}
		fallback := &recordingTransport{status: http.StatusOK}
		transport := &fallbackTransport{primary: primary, fallback: fallback}

		resp, err := transport.RoundTrip(newTestRequest(t, `{"name":"lb"}`))
		if err != nil {
			t.Fatalf("status %d: round trip failed: %v", status, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status %d: expected the fallback's 200, got %d", status, resp.StatusCode)
		}
		if fallback.requests != 1 {
			t.Errorf("status %d: expected one fallback request, got %d", status, fallback.requests)
		}
		if fallback.lastBody != `{"name":"lb"}` {
			t.Errorf("status %d: fallback got body %q, want the original body", status, fallback.lastBody)
		}
	}
}

func TestFallbackTransportRetriesOnTokenRejection(t *testing.T) {
	tokenRejected := &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusUnauthorized}}
	primary := &recordingTransport{err: tokenRejected}
	fallback := &recordingTransport{status: http.StatusOK}
	transport := &fallbackTransport{primary: primary, fallback: fallback}

	resp, err := transport.RoundTrip(newTestRequest(t, ""))
	if err != nil {
		t.Fatalf("round trip failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected the fallback's 200, got %d", resp.StatusCode)
	}
	if fallback.requests != 1 {
		t.Errorf("expected one fallback request, got %d", fallback.requests)
	}
}

func TestFallbackTransportDoesNotRetryOnOutage(t *testing.T) {
	cases := map[string]error{
		"network error":     errors.New("connection refused"),
		"token server down": &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusServiceUnavailable}},
	}

	for name, primaryErr := range cases {
		primary := &recordingTransport{err: primaryErr}
		fallback := &recordingTransport{status: http.StatusOK}
		transport := &fallbackTransport{primary: primary, fallback: fallback}

		_, err := transport.RoundTrip(newTestRequest(t, ""))
		if err == nil {
			t.Errorf("%s: expected the primary's error to surface", name)
		}
		if fallback.requests != 0 {
			t.Errorf("%s: fallback must not run, ran %d times", name, fallback.requests)
		}
	}
}

func TestFallbackTransportDoesNotRetryServerErrors(t *testing.T) {
	primary := &recordingTransport{status: http.StatusInternalServerError}
	fallback := &recordingTransport{status: http.StatusOK}
	transport := &fallbackTransport{primary: primary, fallback: fallback}

	resp, err := transport.RoundTrip(newTestRequest(t, ""))
	if err != nil {
		t.Fatalf("round trip failed: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected the primary's 500 to surface, got %d", resp.StatusCode)
	}
	if fallback.requests != 0 {
		t.Errorf("fallback must not run on a server error, ran %d times", fallback.requests)
	}
}

func TestNewAPIHTTPClientSelectsByCredentials(t *testing.T) {
	hmacOnly, err := NewAPIHTTPClient(context.Background(), Credentials{AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatalf("HMAC-only client: %v", err)
	}
	if _, ok := hmacOnly.Transport.(AuthenticatingTransport); !ok {
		t.Errorf("HMAC-only credentials should produce the signing transport, got %T", hmacOnly.Transport)
	}

	saOnly, err := NewAPIHTTPClient(context.Background(), Credentials{ClientID: "id", ClientSecret: "secret"})
	if err != nil {
		t.Fatalf("SA-only client: %v", err)
	}
	if _, ok := saOnly.Transport.(*fallbackTransport); ok {
		t.Error("SA-only credentials must not produce a fallback transport")
	}

	both, err := NewAPIHTTPClient(context.Background(), Credentials{
		AccessKey: "ak", SecretKey: "sk", ClientID: "id", ClientSecret: "secret",
	})
	if err != nil {
		t.Fatalf("dual-credential client: %v", err)
	}
	if _, ok := both.Transport.(*fallbackTransport); !ok {
		t.Errorf("both credential types should produce the fallback transport, got %T", both.Transport)
	}
}

func TestNewAPIHTTPClientRejectsInsecureTokenURL(t *testing.T) {
	_, err := NewAPIHTTPClient(context.Background(), Credentials{
		ClientID: "id", ClientSecret: "secret", TokenURL: "http://auth.example/oauth2/token",
	})
	if err == nil {
		t.Fatal("expected a plain-http token URL to be rejected")
	}
}
