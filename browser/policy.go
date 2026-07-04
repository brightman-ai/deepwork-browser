package browser

import (
	"net"
	"strings"
)

// ============================================================
// § Session Policy — domain-neutral safety + determinism (SSOT)
// ============================================================
//
// dw-browser is a domain-neutral browser primitive. It protects against two
// hazards without knowing anything about the caller's product:
//
//  1. Remote writes — accidentally mutating the live public web (submitting a
//     form, sending a message) when the caller only meant to drive a local app.
//  2. Non-determinism — silently invoking an internal LLM (vision oracle / NL
//     planner) in a run that is supposed to be reproducible.
//
// Both axes are declared once at `open`, persisted on the session, and enforced
// per-act. SessionPolicy is the single source of truth for the decision logic;
// test harnesses and other callers select values but never re-implement checks.
//
// Safe-by-default: the zero value denies remote writes and imposes no LLM lock,
// so a caller that passes nothing is safe for local-app work and does not break
// (localhost acts are always allowed). Dangerous capability is opt-in.

// RemoteWritePolicy governs mutating actions whose effective target is a
// non-trusted (public web) or unknown origin.
type RemoteWritePolicy string

const (
	// RemoteWriteDeny blocks remote/unknown-origin writes. Default.
	RemoteWriteDeny RemoteWritePolicy = "deny"
	// RemoteWriteConfirm permits them only when the act carries an explicit
	// commit acknowledgement (see ActDecision.NeedsConfirm).
	RemoteWriteConfirm RemoteWritePolicy = "confirm"
	// RemoteWriteAllow permits remote writes unconditionally.
	RemoteWriteAllow RemoteWritePolicy = "allow"
)

// SessionPolicy is the per-session safety/determinism policy (persisted on
// SessionInfo). Zero value == DefaultSessionPolicy semantics (deny, no lock).
type SessionPolicy struct {
	// RemoteWrites governs mutating acts on remote/unknown origins. "" == deny.
	RemoteWrites RemoteWritePolicy `json:"remote_writes,omitempty"`
	// AllowHosts are additional trusted hosts (besides localhost/private ranges)
	// whose origins are treated as local — writes to them are always allowed.
	AllowHosts []string `json:"allow_hosts,omitempty"`
	// Deterministic hard-locks internal LLM use: vision oracle and NL planner
	// refuse to activate even on explicit request, so a baseline run cannot
	// silently become non-reproducible.
	Deterministic bool `json:"deterministic,omitempty"`
}

// DefaultSessionPolicy is the safe-by-default posture.
func DefaultSessionPolicy() SessionPolicy {
	return SessionPolicy{RemoteWrites: RemoteWriteDeny}
}

// NormalizeRemoteWritePolicy accepts only the three known values; unknown/empty
// falls back to the given fallback (fail-closed to deny at the call site).
func NormalizeRemoteWritePolicy(raw string, fallback RemoteWritePolicy) RemoteWritePolicy {
	switch RemoteWritePolicy(strings.ToLower(strings.TrimSpace(raw))) {
	case RemoteWriteDeny:
		return RemoteWriteDeny
	case RemoteWriteConfirm:
		return RemoteWriteConfirm
	case RemoteWriteAllow:
		return RemoteWriteAllow
	default:
		if fallback != "" {
			return fallback
		}
		return RemoteWriteDeny
	}
}

// effectiveRemoteWrites resolves "" to the safe default (deny).
func (p SessionPolicy) effectiveRemoteWrites() RemoteWritePolicy {
	if p.RemoteWrites == "" {
		return RemoteWriteDeny
	}
	return p.RemoteWrites
}

// mutatingOps is the set of action verbs that mutate page/app state. It is the
// SSOT for "is this a write?" — distinct from action_engine's domSettleOps
// (which is about DOM-settle waiting, an overlapping but different concern).
// Anything NOT explicitly read-only is treated as mutating (fail-closed for
// unknown/new verbs).
var readOnlyOps = map[string]bool{
	"scroll":     true,
	"scrollinto": true,
	"hover":      true,
	"hoverat":    true,
	"wheelat":    true,
	"focus":      true,
	// back/forward are treated as read-only navigation (consistent with Navigate
	// itself being ungated — it is *writes* that are gated, not movement). Known
	// accepted limitation: history back/forward can re-submit a prior POST on some
	// sites; that history-resubmit edge is not gated here.
	"back":    true,
	"forward": true,
}

// IsMutatingOp reports whether an action verb changes state (a "write").
func IsMutatingOp(op string) bool {
	return !readOnlyOps[strings.ToLower(strings.TrimSpace(op))]
}

// IsTrustedOrigin reports whether an origin ("scheme://host[:port]") is local
// and therefore always safe to write to: localhost, loopback/private IP ranges,
// or an explicitly allow-listed host. An empty origin (unknown / opaque target,
// e.g. input forwarded to embedded content whose real origin dw-browser cannot
// see) is treated as NOT trusted — the conservative, fail-closed choice.
func IsTrustedOrigin(origin string, allowHosts []string) bool {
	host := originHost(origin)
	if host == "" {
		return false
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return true
	}
	for _, h := range allowHosts {
		if strings.EqualFold(strings.TrimSpace(h), host) {
			return true
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	return false
}

// originHost extracts the host (no port) from an origin string. Reuses the
// URLOrigin scheme handling by parsing the authority section.
func originHost(origin string) string {
	origin = strings.TrimSpace(origin)
	for _, prefix := range []string{"http://", "https://", "ws://", "wss://"} {
		if after, found := strings.CutPrefix(origin, prefix); found {
			origin = after
			break
		}
	}
	if origin == "" {
		return ""
	}
	// strip path if a bare authority slipped through
	if idx := strings.IndexByte(origin, '/'); idx >= 0 {
		origin = origin[:idx]
	}
	if h, _, err := net.SplitHostPort(origin); err == nil {
		return h
	}
	// No port present (SplitHostPort failed): strip surrounding brackets for a
	// bracketed IPv6 authority (e.g. "[::1]" → "::1") so loopback/private IPv6 is
	// not mis-classified as untrusted (fail-closed edge).
	if strings.HasPrefix(origin, "[") && strings.HasSuffix(origin, "]") {
		return origin[1 : len(origin)-1]
	}
	return origin
}

// ActDecision is the outcome of evaluating an action against the policy.
type ActDecision struct {
	Allowed      bool
	NeedsConfirm bool   // confirm policy hit: allow only if the act is explicitly committed
	Reason       string // human-readable when blocked/needs-confirm
}

// EvaluateAct decides whether an action verb is permitted given the origin of
// its effective target. currentOrigin must be "" when the target origin is
// unknown/opaque (e.g. takeover-forwarded input to embedded content) so it is
// treated conservatively as remote.
func (p SessionPolicy) EvaluateAct(op, currentOrigin string) ActDecision {
	if !IsMutatingOp(op) {
		return ActDecision{Allowed: true}
	}
	if IsTrustedOrigin(currentOrigin, p.AllowHosts) {
		return ActDecision{Allowed: true}
	}
	target := currentOrigin
	if target == "" {
		target = "unknown/opaque target"
	}
	switch p.effectiveRemoteWrites() {
	case RemoteWriteAllow:
		return ActDecision{Allowed: true}
	case RemoteWriteConfirm:
		return ActDecision{
			Allowed:      false,
			NeedsConfirm: true,
			Reason:       "remote write to " + target + " blocked: session policy requires explicit confirmation",
		}
	default: // deny
		return ActDecision{
			Allowed: false,
			Reason:  "remote write to " + target + " denied by session policy (open with --scenario webvisit --allow-host <host> to operate this site)",
		}
	}
}
