package browser

import "testing"

// TestCheckActPolicy exercises the per-act remote-write chokepoint on the real
// core method (checkActPolicy). It passes a nil ctx so liveTopURL short-circuits
// to "" and the method falls back to the tracked lastURL — driving only
// policy/lastURL, no Chrome. This proves the localhost subtlety the wiring
// depends on: origin comes from the tracked lastURL, so a mutating act on a
// localhost origin stays allowed under the default (deny) policy, while an empty
// lastURL (PageURL never wired / live fetch unavailable) fails closed.
func TestCheckActPolicy(t *testing.T) {
	tests := []struct {
		name      string
		policy    SessionPolicy
		lastURL   string
		action    string
		wantBlock bool
	}{
		{"read op always allowed on remote", DefaultSessionPolicy(), "https://example.com/x", "scroll down", false},
		{"read op allowed with empty origin", DefaultSessionPolicy(), "", "hover e1", false},
		{"localhost mutating allowed under deny", DefaultSessionPolicy(), "http://localhost:3000/app", "type e1 'hi'", false},
		{"loopback IP mutating allowed under deny", DefaultSessionPolicy(), "http://127.0.0.1:8080/", "click e1", false},
		{"bracketed IPv6 loopback (no port) allowed under deny", DefaultSessionPolicy(), "http://[::1]/app", "click e1", false},
		{"bracketed IPv6 loopback (with port) allowed under deny", DefaultSessionPolicy(), "http://[::1]:8080/app", "type e1 'hi'", false},
		{"remote mutating blocked under deny", DefaultSessionPolicy(), "https://example.com/x", "click e1", true},
		{"empty origin mutating blocked under deny (fail-closed)", DefaultSessionPolicy(), "", "click e1", true},
		{"remote mutating allowed under allow", SessionPolicy{RemoteWrites: RemoteWriteAllow}, "https://example.com/x", "click e1", false},
		{"remote mutating blocked under confirm (P3 minimal)", SessionPolicy{RemoteWrites: RemoteWriteConfirm}, "https://example.com/x", "click e1", true},
		{"allow-listed host mutating allowed", SessionPolicy{RemoteWrites: RemoteWriteDeny, AllowHosts: []string{"staging.internal"}}, "https://staging.internal/x", "click e1", false},
		{"unparseable action defers to executor (no block)", DefaultSessionPolicy(), "https://example.com/x", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			impl := &browserCoreImpl{}
			impl.SetPolicy(tc.policy, tc.lastURL)
			err := impl.checkActPolicy(nil, tc.action)
			if tc.wantBlock && err == nil {
				t.Fatalf("expected block, got allowed (policy=%v origin=%q action=%q)", tc.policy, tc.lastURL, tc.action)
			}
			if !tc.wantBlock && err != nil {
				t.Fatalf("expected allowed, got block: %v", err)
			}
		})
	}
}

// TestSetPolicyURLTracking verifies SetPolicy refreshes lastURL only when a
// non-empty URL is supplied (Navigate/connect refresh semantics), which is what
// keeps origin classification fresh across in-process acts.
func TestSetPolicyURLTracking(t *testing.T) {
	impl := &browserCoreImpl{}
	impl.SetPolicy(DefaultSessionPolicy(), "http://localhost:3000/")
	if impl.lastURL != "http://localhost:3000/" {
		t.Fatalf("lastURL not set: %q", impl.lastURL)
	}
	// Empty currentURL must not clobber the tracked URL.
	impl.SetPolicy(SessionPolicy{RemoteWrites: RemoteWriteAllow}, "")
	if impl.lastURL != "http://localhost:3000/" {
		t.Fatalf("empty currentURL clobbered lastURL: %q", impl.lastURL)
	}
	if impl.policy.RemoteWrites != RemoteWriteAllow {
		t.Fatalf("policy not updated: %v", impl.policy)
	}
}
