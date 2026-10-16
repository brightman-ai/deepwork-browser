package browser

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const ResourceSchema = "dw.resource.v1"
const ResourceFailureSchema = "dw.resource.failure.v1"

type ResourceRequest struct {
	DNSResolver     string
	URL             string
	Timeout         time.Duration
	MaxBytes        int64
	MaxNetworkBytes int64
	NetworkRoute    NetworkRoute
}

type ResourcePolicy struct {
	NetworkRouting
	Network         string `json:"network"`
	Client          string `json:"client"`
	NetworkRequests int64  `json:"network_requests"`
	BlockedRequests int64  `json:"blocked_requests"`
	NetworkBytes    int64  `json:"network_bytes"`
	MaxBytes        int64  `json:"max_bytes"`
	MaxNetworkBytes int64  `json:"max_network_bytes"`
}

type ResourceCleanup struct {
	NetworkClosed bool `json:"network_closed"`
}

// Body is the HTTP client's entity bytes, after standard transfer/content
// decoding. It is never converted to text and is deliberately absent from JSON.
type Resource struct {
	Schema         string          `json:"schema"`
	ToolVersion    string          `json:"tool_version,omitempty"`
	RequestedURL   string          `json:"requested_url"`
	URL            string          `json:"url"`
	StatusCode     int             `json:"status_code"`
	ContentType    string          `json:"content_type"`
	Representation string          `json:"representation"`
	Bytes          int64           `json:"bytes"`
	SHA256         string          `json:"sha256"`
	Policy         ResourcePolicy  `json:"policy"`
	Cleanup        ResourceCleanup `json:"cleanup"`
	Body           []byte          `json:"-"`
}

type ResourceAcquisitionTiming struct {
	StartupMS float64 `json:"startup_ms"`
	RequestMS float64 `json:"request_ms"`
	ReadMS    float64 `json:"read_ms"`
	CleanupMS float64 `json:"cleanup_ms"`
	ElapsedMS float64 `json:"elapsed_ms"`
}

type ResourceFailureDiagnostic struct {
	Schema        string                    `json:"schema"`
	ToolVersion   string                    `json:"tool_version,omitempty"`
	RequestedURL  string                    `json:"requested_url"`
	URL           string                    `json:"url,omitempty"`
	Stage         string                    `json:"stage"`
	Reason        string                    `json:"reason"`
	Usable        bool                      `json:"usable"`
	StatusCode    int                       `json:"status_code,omitempty"`
	ContentType   string                    `json:"content_type"`
	BodyBytesRead int64                     `json:"body_bytes_read"`
	Policy        ResourcePolicy            `json:"policy"`
	Cleanup       ResourceCleanup           `json:"cleanup"`
	Timing        ResourceAcquisitionTiming `json:"timing"`
}

type ResourceAcquisitionError struct {
	Diagnostic ResourceFailureDiagnostic
	Cause      error
}

func (e *ResourceAcquisitionError) Error() string { return e.Cause.Error() }
func (e *ResourceAcquisitionError) Unwrap() error { return e.Cause }

type resourceStatusFailure struct{ status int }

func (e *resourceStatusFailure) Error() string {
	return fmt.Sprintf("resource HTTP status %d is not a complete successful response", e.status)
}

type resourceBodyBudgetFailure struct{}

func (*resourceBodyBudgetFailure) Error() string { return "resource decoded body byte budget exceeded" }

type resourceRedirectFailure struct{ error }

func (e *resourceRedirectFailure) Unwrap() error { return e.error }

func normalizeResourceRequest(r ResourceRequest) (ResourceRequest, error) {
	var err error
	r.NetworkRoute, err = normalizeNetworkRoute(r.NetworkRoute)
	if err != nil {
		return r, err
	}
	r.DNSResolver, err = ValidateDNSResolver(r.DNSResolver)
	if err != nil {
		return r, err
	}
	if r.Timeout < 0 || r.MaxBytes < 0 || r.MaxNetworkBytes < 0 {
		return r, errors.New("resource budgets must not be negative")
	}
	if r.Timeout == 0 {
		r.Timeout = 20 * time.Second
	}
	if r.MaxBytes == 0 {
		r.MaxBytes = 8 << 20
	}
	if r.MaxNetworkBytes == 0 {
		r.MaxNetworkBytes = 32 << 20
	}
	// The decoded reader requests one extra byte to detect overflow safely.
	if r.MaxBytes == int64(^uint64(0)>>1) {
		return r, errors.New("resource body byte budget is too large")
	}
	if _, err := ValidatePublicURL(r.URL, nil); err != nil {
		return r, fmt.Errorf("resource URL: %w", err)
	}
	return r, nil
}

type resourceFetchOptions struct {
	network PublicNetworkOptions
	rootCAs *x509.CertPool // Owned TLS fixtures only; production uses system roots.
}

// FetchResource is a bounded public HTTP GET kernel. It has no browser, profile,
// cookies, caller headers, credentials, scripts, files or alternate transport.
func FetchResource(ctx context.Context, request ResourceRequest) (*Resource, error) {
	return fetchResourceWithOptions(ctx, request, resourceFetchOptions{})
}

func resourceFailureReason(err error) string {
	var policy *publicNetworkPolicyFailure
	var transport *publicNetworkTransportFailure
	var status *resourceStatusFailure
	var budget *resourceBodyBudgetFailure
	var redirect *resourceRedirectFailure
	var dns *DNSResolutionError
	switch {
	case errors.As(err, &policy):
		return "network_policy"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.As(err, &dns):
		return "dns_resolution"
	case errors.As(err, &budget):
		return "body_budget_exceeded"
	case errors.As(err, &status):
		return "http_status"
	case errors.As(err, &redirect):
		return "redirect_limit"
	case errors.As(err, &transport):
		return "network_transport"
	default:
		return "acquisition_failed"
	}
}

func resourceContentType(headers http.Header) (string, error) {
	values := headers.Values("Content-Type")
	if len(values) == 0 {
		return "", nil
	}
	// Reuse the strict response metadata canonicalizer, not DOM content sniffing.
	return normalizeDocumentContentType(strings.Join(values, "\n"))
}

func fetchResourceWithOptions(parent context.Context, request ResourceRequest, options resourceFetchOptions) (resource *Resource, resultErr error) {
	started := time.Now()
	d := ResourceFailureDiagnostic{Schema: ResourceFailureSchema, RequestedURL: request.URL, Stage: "validation"}
	routeName := request.NetworkRoute
	if routeName == "" {
		routeName = NetworkRouteSystem
	}
	d.Policy = ResourcePolicy{NetworkRouting: NetworkRouting{DNSResolver: dnsResolverForReceipt(request.DNSResolver), RequestedNetworkRoute: routeName, EffectiveTransport: "not-started", NetworkRouteSource: "unresolved", NumericTargetPinning: true}, Network: "page-requests-public-only", Client: "http-get"}
	phaseStarted := started
	advance := func(stage string) {
		elapsed := float64(time.Since(phaseStarted)) / float64(time.Millisecond)
		switch d.Stage {
		case "startup":
			d.Timing.StartupMS += elapsed
		case "request":
			d.Timing.RequestMS += elapsed
		case "read":
			d.Timing.ReadMS += elapsed
		case "cleanup":
			d.Timing.CleanupMS += elapsed
		}
		d.Stage = stage
		phaseStarted = time.Now()
	}
	defer func() {
		if resultErr == nil {
			return
		}
		resultErr = withDNSFailureContext(resultErr, d.Policy.DNSResolver)
		failedStage := d.Stage
		advance("done")
		d.Stage = failedStage
		if d.Reason == "" {
			d.Reason = resourceFailureReason(resultErr)
		}
		d.Timing.ElapsedMS = float64(time.Since(started)) / float64(time.Millisecond)
		resource = nil
		resultErr = &ResourceAcquisitionError{Diagnostic: d, Cause: resultErr}
	}()
	r, err := normalizeResourceRequest(request)
	if err != nil {
		return nil, err
	}
	d.Policy.MaxBytes, d.Policy.MaxNetworkBytes = r.MaxBytes, r.MaxNetworkBytes
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, r.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	advance("startup")
	route, err := resolveNetworkRoute(r.NetworkRoute)
	route.receipt.DNSResolver = r.DNSResolver
	d.Policy.NetworkRouting = route.receipt
	if err != nil {
		return nil, fmt.Errorf("resource network route: %w", err)
	}
	options.network.NetworkRoute, options.network.routeSnapshot = r.NetworkRoute, route
	options.network.DNSResolver = r.DNSResolver
	options.network.Timeout, options.network.MaxBytes, options.network.MaxRequests = r.Timeout, r.MaxNetworkBytes, 128
	options.network.AllowedPorts = nil
	guard, err := StartPublicNetworkGuard(ctx, options.network)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Acquisition includes hashing. Cleanup then owns its independent budget.
		if err := ctx.Err(); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
		failedStage := d.Stage
		if resultErr != nil {
			d.Reason = resourceFailureReason(resultErr)
		}
		advance("cleanup")
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if err := guard.CloseContext(cleanupCtx); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("resource network cleanup: %w", err))
			if d.Reason == "" {
				d.Reason = "cleanup_failed"
				failedStage = "cleanup"
			}
		} else {
			d.Cleanup.NetworkClosed = true
		}
		if err := guard.PolicyError(); err != nil {
			resultErr = errors.Join(resultErr, &publicNetworkPolicyFailure{err})
			d.Reason = "network_policy"
		}
		stats := guard.Stats()
		d.Policy.NetworkRequests, d.Policy.BlockedRequests, d.Policy.NetworkBytes = stats.Requests, stats.DeniedRequests, stats.Bytes
		d.Timing.CleanupMS += float64(time.Since(phaseStarted)) / float64(time.Millisecond)
		d.Stage = failedStage
		phaseStarted = time.Now()
		if resultErr == nil && resource != nil {
			resource.Policy, resource.Cleanup = d.Policy, d.Cleanup
		}
	}()
	proxyURL, err := url.Parse(guard.ProxyURL())
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true, MaxResponseHeaderBytes: 64 << 10, ResponseHeaderTimeout: r.Timeout,
		OnProxyConnectResponse: func(_ context.Context, _ *url.URL, _ *http.Request, response *http.Response) error {
			return publicProxyResponseFailure(response.Header)
		},
	}
	if options.rootCAs != nil {
		transport.TLSClientConfig = &tls.Config{RootCAs: options.rootCAs, MinVersion: tls.VersionTLS12}
	}
	defer transport.CloseIdleConnections()
	// Handle redirects explicitly. ErrUseLastResponse prevents Go's automatic
	// redirect-body draining, so skipped bodies consume zero decoded read budget.
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	advance("request")
	nextURL := r.URL
	var response *http.Response
	for redirects := 0; ; redirects++ {
		get, err := http.NewRequestWithContext(ctx, http.MethodGet, nextURL, nil)
		if err != nil {
			return nil, err
		}
		response, err = client.Do(get)
		if err != nil {
			return nil, &publicNetworkTransportFailure{err}
		}
		d.URL, d.StatusCode = response.Request.URL.String(), response.StatusCode
		if response.Request.URL.Scheme == "http" {
			if err := publicProxyResponseFailure(response.Header); err != nil {
				response.Body.Close()
				return nil, err
			}
		}
		d.ContentType, err = resourceContentType(response.Header)
		if err != nil {
			response.Body.Close()
			return nil, err
		}
		redirect := response.StatusCode == 301 || response.StatusCode == 302 || response.StatusCode == 303 || response.StatusCode == 307 || response.StatusCode == 308
		if !redirect || response.Header.Get("Location") == "" {
			break
		}
		location, err := response.Location()
		response.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("resource redirect: %w", err)
		}
		if redirects >= 5 {
			return nil, &resourceRedirectFailure{errors.New("resource redirect limit of five exceeded")}
		}
		if _, err := ValidatePublicURL(location.String(), nil); err != nil {
			return nil, &publicNetworkPolicyFailure{fmt.Errorf("resource redirect URL: %w", err)}
		}
		nextURL = location.String()
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 || response.StatusCode == http.StatusPartialContent {
		return nil, &resourceStatusFailure{response.StatusCode}
	}
	advance("read")
	body, err := io.ReadAll(io.LimitReader(&resourceCountingReader{Reader: response.Body, Count: &d.BodyBytesRead}, r.MaxBytes+1))
	if err != nil {
		return nil, &publicNetworkTransportFailure{err}
	}
	if int64(len(body)) > r.MaxBytes {
		return nil, &resourceBodyBudgetFailure{}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(body)
	return &Resource{Schema: ResourceSchema, RequestedURL: r.URL, URL: d.URL, StatusCode: d.StatusCode, ContentType: d.ContentType, Representation: "http-body", Bytes: int64(len(body)), SHA256: hex.EncodeToString(digest[:]), Body: body}, nil
}

type resourceCountingReader struct {
	io.Reader
	Count *int64
}

func (r *resourceCountingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	*r.Count += int64(n)
	return n, err
}
