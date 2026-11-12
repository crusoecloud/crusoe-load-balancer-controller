package crusoe

import (
	"context"
	"errors"
	"net/http"

	clientauth "github.com/crusoecloud/client-go/auth"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/oauth2"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Credentials carries both credential types an installation may hold during the
// IAM service account migration: the OAuth2 client credentials of the cluster's
// service account, and the legacy per-user HMAC key pair.
type Credentials struct {
	// AccessKey and SecretKey are the legacy HMAC API key pair.
	AccessKey string
	SecretKey string
	// ClientID and ClientSecret identify the cluster's IAM service account.
	ClientID     string
	ClientSecret string
	// TokenURL is the OAuth2 token endpoint the client credentials are exchanged
	// at. Empty selects client-go's production default; non-production clusters
	// must set it together with Audience, because neither can be derived from the
	// API endpoint.
	TokenURL string
	// Audience is sent as the token request's audience parameter. Empty selects
	// client-go's production default.
	Audience string
}

// HasServiceAccount reports whether a service account credential is configured.
func (c Credentials) HasServiceAccount() bool {
	return c.ClientID != "" && c.ClientSecret != ""
}

// HasHMAC reports whether the legacy HMAC key pair is configured.
func (c Credentials) HasHMAC() bool {
	return c.AccessKey != "" && c.SecretKey != ""
}

// saFallbackTotal counts requests that were retried with the HMAC keys after the
// service account token was rejected. Per the IAM SA RFC, this metric going
// silent fleet-wide is the signal that the HMAC keys can be removed.
//
//nolint:gochecknoglobals // prometheus collectors are registered once, process-wide
var saFallbackTotal = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "crusoe_lb_controller_sa_auth_fallback_total",
	Help: "Requests that fell back to HMAC key auth after service account auth was rejected.",
})

//nolint:gochecknoinits // controller-runtime's documented pattern for registering custom metrics
func init() {
	metrics.Registry.MustRegister(saFallbackTotal)
}

// NewAPIHTTPClient returns the http.Client every Crusoe API call goes through,
// choosing the authentication the configured credentials allow:
//
//   - service account only: JWT bearer auth, refreshed automatically before expiry
//   - HMAC keys only: request signing, as before service accounts existed
//   - both: JWT first; a request the API rejects as unauthenticated (401/403) is
//     replayed once with the HMAC signature, counting saFallbackTotal
//
// ctx is captured by the token source for its background refreshes, so pass a
// process-lifetime context, not a request-scoped one.
func NewAPIHTTPClient(ctx context.Context, creds Credentials) (*http.Client, error) {
	if !creds.HasServiceAccount() {
		return NewHTTPClient(creds.AccessKey, creds.SecretKey), nil
	}

	saCfg, err := clientauth.NewServiceAccountConfig(ctx, creds.ClientID, creds.ClientSecret, creds.TokenURL, creds.Audience)
	if err != nil {
		return nil, err //nolint:wrapcheck // the config error already names what was rejected
	}

	if !creds.HasHMAC() {
		return saCfg.HTTPClient, nil
	}

	return &http.Client{Transport: &fallbackTransport{
		primary:  saCfg.HTTPClient.Transport,
		fallback: NewAuthenticatingTransport(nil, creds.AccessKey, creds.SecretKey),
	}}, nil
}

// fallbackTransport sends every request with the service account token and
// replays one the API rejects as unauthenticated with the HMAC signature.
//
// Only an authentication rejection triggers the replay: a 401/403 response, or a
// token exchange the auth server itself refused. A network error or an auth
// server outage does not, because falling back on those would silently move the
// fleet off service accounts whenever Hydra has a bad day.
type fallbackTransport struct {
	primary  http.RoundTripper
	fallback http.RoundTripper
}

func (t *fallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone before the first attempt: the fallback transport signs by mutating its
	// request, and a consumed body can only be replayed through GetBody.
	retry, retryable := cloneForRetry(req)

	resp, err := t.primary.RoundTrip(req)
	switch {
	case err != nil:
		if !isTokenAuthError(err) {
			return nil, err
		}
	case resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden:
		return resp, nil
	}

	if !retryable {
		// A streaming body that cannot be re-read; surface the rejection as-is.
		return resp, err
	}

	if resp != nil {
		_ = resp.Body.Close()
	}

	saFallbackTotal.Inc()
	log.Log.WithName("crusoe-auth").Info(
		"service account auth rejected, falling back to HMAC keys",
		"method", req.Method, "path", req.URL.Path, "error", errString(err),
	)

	//nolint:wrapcheck // transport errors are forwarded untouched, like every RoundTripper
	return t.fallback.RoundTrip(retry)
}

// cloneForRetry returns a copy of req that can be sent after req itself has
// been. It reports false when the body cannot be reproduced.
func cloneForRetry(req *http.Request) (*http.Request, bool) {
	clone := req.Clone(req.Context())

	if req.Body == nil || req.Body == http.NoBody {
		return clone, true
	}

	if req.GetBody == nil {
		return nil, false
	}

	body, err := req.GetBody()
	if err != nil {
		return nil, false
	}
	clone.Body = body

	return clone, true
}

// isTokenAuthError reports whether err is the auth server refusing the client
// credentials during the token exchange, as opposed to the exchange failing for
// reasons retrying with the same credentials could fix (network, server error).
func isTokenAuthError(err error) bool {
	var retrieveErr *oauth2.RetrieveError
	if !errors.As(err, &retrieveErr) || retrieveErr.Response == nil {
		return false
	}

	switch retrieveErr.Response.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		// 400 carries OAuth2's invalid_client/invalid_grant style rejections.
		return true
	default:
		return false
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}
