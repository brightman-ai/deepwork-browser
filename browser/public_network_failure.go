package browser

import (
	"errors"
	"net/http"
)

// Shared acquisition classifications preserve public authorization failures
// separately from transport failures across browser DOM and HTTP byte clients.
type publicNetworkPolicyFailure struct{ error }

func (e *publicNetworkPolicyFailure) Unwrap() error { return e.error }

type publicNetworkTransportFailure struct{ error }

func (e *publicNetworkTransportFailure) Unwrap() error { return e.error }

func publicProxyResponseFailure(headers http.Header) error {
	marker, kind := headers.Get(PublicNetworkErrorHeader), headers.Get(PublicNetworkErrorKindHeader)
	if marker == "" && kind == "" {
		return nil
	}
	if marker == "1" && kind == string(publicProxyTransportFailure) {
		return &publicNetworkTransportFailure{errors.New("resource transport failed through network guard")}
	}
	if marker == "1" && kind == string(publicProxyDNSFailure) {
		return &DNSResolutionError{Reason: "resolution_failed", Cause: errors.New("DNS resolution failed through network guard")}
	}
	return &publicNetworkPolicyFailure{errors.New("resource rejected by network guard")}
}
