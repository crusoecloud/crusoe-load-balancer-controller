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
	// ClusterID labels the auth metrics so a noisy fallback can be traced to the
	// cluster it comes from. Each controller serves one cluster, so this is a
	// single constant value per process.
	ClusterID string
}

// HasServiceAccount reports whether a service account credential is configured.
func (c Credentials) HasServiceAccount() bool {
	return c.ClientID != "" && c.ClientSecret != ""
}

// HasHMAC reports whether the legacy HMAC key pair is configured.
func (c Credentials) HasHMAC() bool {
	return c.AccessKey != "" && c.SecretKey != ""
}

// saAuthSuccessTotal counts requests the service account token authenticated
// successfully, labelled by cluster. It is the denominator the fallback counter
// needs: fallback_total staying at zero only means "safe to remove HMAC" when
// success_total is non-zero — otherwise zero fallbacks can just mean the service
// account was never exercised.
//
//nolint:gochecknoglobals // prometheus collectors are registered once, process-wide
var saAuthSuccessTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "crusoe_lb_controller_sa_auth_success_total",
	Help: "Crusoe API requests authenticated with the service account token (JWT).",
}, []string{"cluster_id"})

// saFallbackTotal counts requests that were retried with the HMAC keys after the
// service account token was rejected. Per the IAM SA RFC, this metric going
// silent fleet-wide (with success_total non-zero) is the signal that the HMAC
// keys can be removed. The reason label distinguishes an API rejection
// (unauthorized/forbidden) from a token exchange the auth server refused.
//
//nolint:gochecknoglobals // prometheus collectors are registered once, process-wide
var saFallbackTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "crusoe_lb_controller_sa_auth_fallback_total",
	Help: "Requests that fell back to HMAC key auth after service account auth was rejected.",
}, []string{"cluster_id", "reason"})

//nolint:gochecknoinits // controller-runtime's documented pattern for registering custom metrics
func init() {
	metrics.Registry.MustRegister(saAuthSuccessTotal, saFallbackTotal)
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
		return &http.Client{Transport: saLoggingTransport{
			inner:     saCfg.HTTPClient.Transport,
			clientID:  creds.ClientID,
			clusterID: creds.ClusterID,
		}}, nil
	}

	return &http.Client{Transport: &fallbackTransport{
		primary: saLoggingTransport{
			inner:     saCfg.HTTPClient.Transport,
			clientID:  creds.ClientID,
			clusterID: creds.ClusterID,
		},
		fallback:  NewAuthenticatingTransport(nil, creds.AccessKey, creds.SecretKey),
		clusterID: creds.ClusterID,
	}}, nil
}

// saLoggingTransport logs each Crusoe API request the service account path
// authenticates, so SA usage is visible directly rather than inferred from the
// absence of a fallback line. It logs only on a response the API accepted (not
// 401/403, and not a token-fetch error), so a request that then falls back to
// HMAC is never mislabeled as service-account authenticated.
type saLoggingTransport struct {
	inner     http.RoundTripper
	clientID  string
	clusterID string
}

func (t saLoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err == nil && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		saAuthSuccessTotal.WithLabelValues(t.clusterID).Inc()
		log.Log.WithName("crusoe-auth").Info(
			"Crusoe API request authenticated with service account token (JWT)",
			"method", req.Method, "path", req.URL.Path, "status", resp.StatusCode,
			"clientID", t.clientID, "clusterID", t.clusterID,
		)
	}

	//nolint:wrapcheck // transport errors are forwarded untouched, like every RoundTripper
	return resp, err
}

// fallbackTransport sends every request with the service account token and
// replays one the API rejects as unauthenticated with the HMAC signature.
//
// Only an authentication rejection triggers the replay: a 401/403 response, or a
// token exchange the auth server itself refused. A network error or an auth
// server outage does not, because falling back on those would silently move the
// fleet off service accounts whenever Hydra has a bad day.
type fallbackTransport struct {
	primary   http.RoundTripper
	fallback  http.RoundTripper
	clusterID string
}

func (t *fallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone before the first attempt: the fallback transport signs by mutating its
	// request, and a consumed body can only be replayed through GetBody.
	retry, retryable := cloneForRetry(req)

	resp, err := t.primary.RoundTrip(req)

	// reason classifies why the service account token was rejected, for the metric
	// label and the log. An empty reason means the request succeeded and no
	// fallback is needed.
	var reason string
	switch {
	case err != nil:
		if !isTokenAuthError(err) {
			return nil, err
		}
		reason = "token_rejected"
	case resp.StatusCode == http.StatusUnauthorized:
		reason = "unauthorized"
	case resp.StatusCode == http.StatusForbidden:
		reason = "forbidden"
	default:
		return resp, nil
	}

	if !retryable {
		// A streaming body that cannot be re-read; surface the rejection as-is.
		return resp, err
	}

	if resp != nil {
		_ = resp.Body.Close()
	}

	saFallbackTotal.WithLabelValues(t.clusterID, reason).Inc()
	log.Log.WithName("crusoe-auth").Info(
		"service account auth rejected, falling back to HMAC keys",
		"method", req.Method, "path", req.URL.Path, "reason", reason,
		"clusterID", t.clusterID, "error", errString(err),
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
