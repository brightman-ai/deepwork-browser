package browser

import (
	"context"
	"errors"
	"time"
)

const DocumentFailureSchema = "dw.document.failure.v1"

// DocumentFailureDiagnostic reports an acquisition attempt, never usable DOM.
// URL is present only after the main frame URL has actually been observed.
type DocumentFailureDiagnostic struct {
	Rendering    DocumentRendering         `json:"rendering"`
	Schema       string                    `json:"schema"`
	ToolVersion  string                    `json:"tool_version,omitempty"`
	RequestedURL string                    `json:"requested_url"`
	URL          string                    `json:"url,omitempty"`
	Stage        string                    `json:"stage"`
	Reason       string                    `json:"reason"`
	Usable       bool                      `json:"usable"`
	Policy       DocumentPolicy            `json:"policy"`
	Cleanup      DocumentCleanup           `json:"cleanup"`
	Timing       DocumentAcquisitionTiming `json:"timing"`
}

// Durations are measured elapsed milliseconds; zero also covers stages that
// were never entered. Cleanup includes slot release and final policy settlement.
type DocumentAcquisitionTiming struct {
	QueueMS      float64 `json:"queue_ms"`
	StartupMS    float64 `json:"startup_ms"`
	NavigationMS float64 `json:"navigation_ms"`
	ReadMS       float64 `json:"read_ms"`
	CleanupMS    float64 `json:"cleanup_ms"`
	ElapsedMS    float64 `json:"elapsed_ms"`
}

// DocumentAcquisitionError preserves the underlying error for errors.Is/As.
// Consumers must not use errors.As(UnsupportedDocumentContentError) as reroute
// authorization: only a direct, settled unsupported error carries permission.
type DocumentAcquisitionError struct {
	Diagnostic DocumentFailureDiagnostic
	Cause      error
}

func (e *DocumentAcquisitionError) Error() string { return e.Cause.Error() }
func (e *DocumentAcquisitionError) Unwrap() error { return e.Cause }

type documentCleanupFailure struct{ error }

func (e *documentCleanupFailure) Unwrap() error { return e.error }

type documentBudgetFailure struct{ error }

func (e *documentBudgetFailure) Unwrap() error { return e.error }

func documentFailureReason(err error) string {
	var policy *publicNetworkPolicyFailure
	var cleanup *documentCleanupFailure
	var transport *publicNetworkTransportFailure
	var budget *documentBudgetFailure
	var dns *DNSResolutionError
	switch {
	case errors.As(err, &policy):
		return "network_policy"
	case documentAcquisitionCauseIs(err, context.DeadlineExceeded):
		return "timeout"
	case documentAcquisitionCauseIs(err, context.Canceled):
		return "canceled"
	case errors.As(err, &dns):
		return "dns_resolution"
	case errors.As(err, &transport):
		return "network_transport"
	case errors.As(err, &cleanup):
		return "cleanup_failed"
	case errors.As(err, &budget):
		return "html_budget_exceeded"
	default:
		return "acquisition_failed"
	}
}

// Cleanup owns a separate context and budget. A context error beneath that
// boundary must not be presented as an acquisition deadline or caller cancel.
func documentAcquisitionCauseIs(err, target error) bool {
	if err == nil {
		return false
	}
	if _, cleanup := err.(*documentCleanupFailure); cleanup {
		return false
	}
	if err == target {
		return true
	}
	if matcher, ok := err.(interface{ Is(error) bool }); ok && matcher.Is(target) {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, cause := range wrapped.Unwrap() {
			if documentAcquisitionCauseIs(cause, target) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return documentAcquisitionCauseIs(wrapped.Unwrap(), target)
	}
	return false
}

type documentAttempt struct {
	diagnostic            DocumentFailureDiagnostic
	started, phaseStarted time.Time
	phase                 string
	failureStage          string
}

func newDocumentAttempt(url string) *documentAttempt {
	now := time.Now()
	return &documentAttempt{diagnostic: DocumentFailureDiagnostic{Schema: DocumentFailureSchema, RequestedURL: url}, started: now, phaseStarted: now, phase: "validation"}
}
func (a *documentAttempt) advance(stage string) {
	now := time.Now()
	elapsed := float64(now.Sub(a.phaseStarted)) / float64(time.Millisecond)
	switch a.phase {
	case "queue":
		a.diagnostic.Timing.QueueMS += elapsed
	case "startup":
		a.diagnostic.Timing.StartupMS += elapsed
	case "navigation":
		a.diagnostic.Timing.NavigationMS += elapsed
	case "read":
		a.diagnostic.Timing.ReadMS += elapsed
	case "cleanup":
		a.diagnostic.Timing.CleanupMS += elapsed
	}
	a.phase, a.phaseStarted = stage, now
}
func (a *documentAttempt) beginCleanup(failed bool) {
	if a.phase == "cleanup" {
		return
	}
	if failed {
		a.failureStage = a.phase
	}
	a.advance("cleanup")
}
func (a *documentAttempt) settle(doc *Document, err error) (*Document, error) {
	if err == nil {
		return doc, nil
	}
	// Preserve the direct-type fallback contract only for pure, settled outcomes.
	if unsupported, ok := err.(*UnsupportedDocumentContentError); ok && unsupported.Reroutable {
		return nil, err
	}
	stage := a.phase
	if a.failureStage != "" {
		stage = a.failureStage
	}
	a.advance("done")
	a.diagnostic.Stage = stage
	a.diagnostic.Reason = documentFailureReason(err)
	a.diagnostic.Timing.ElapsedMS = float64(time.Since(a.started)) / float64(time.Millisecond)
	return nil, &DocumentAcquisitionError{Diagnostic: a.diagnostic, Cause: err}
}
