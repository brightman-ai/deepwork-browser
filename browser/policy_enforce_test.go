package browser

import "testing"

// TestCheckActPolicy exercises the per-act remote-write chokepoint on the real
// core method (checkActionPolicy). It passes a nil ctx so liveTopURL short-circuits
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
		action    Action
		wantBlock bool
	}{
		{"read op always allowed on remote", DefaultSessionPolicy(), "https://example.com/x", Action{Verb: "scroll", Direction: "down"}, false},
		{"read op allowed with empty origin", DefaultSessionPolicy(), "", Action{Verb: "hover", Target: fixtureRef("@ofixture-r1")}, false},
		{"localhost mutating allowed under deny", DefaultSessionPolicy(), "http://localhost:3000/app", Action{Verb: "type", Target: fixtureRef("@ofixture-r1"), Text: "hi"}, false},
		{"loopback IP mutating allowed under deny", DefaultSessionPolicy(), "http://127.0.0.1:8080/", Action{Verb: "click", Target: fixtureRef("@ofixture-r1")}, false},
		{"bracketed IPv6 loopback (no port) allowed under deny", DefaultSessionPolicy(), "http://[::1]/app", Action{Verb: "click", Target: fixtureRef("@ofixture-r1")}, false},
		{"bracketed IPv6 loopback (with port) allowed under deny", DefaultSessionPolicy(), "http://[::1]:8080/app", Action{Verb: "type", Target: fixtureRef("@ofixture-r1"), Text: "hi"}, false},
		{"remote mutating blocked under deny", DefaultSessionPolicy(), "https://example.com/x", Action{Verb: "click", Target: fixtureRef("@ofixture-r1")}, true},
		{"empty origin mutating blocked under deny (fail-closed)", DefaultSessionPolicy(), "", Action{Verb: "click", Target: fixtureRef("@ofixture-r1")}, true},
		{"remote mutating allowed under allow", SessionPolicy{RemoteWrites: RemoteWriteAllow}, "https://example.com/x", Action{Verb: "click", Target: fixtureRef("@ofixture-r1")}, false},
		{"remote mutating blocked under confirm (P3 minimal)", SessionPolicy{RemoteWrites: RemoteWriteConfirm}, "https://example.com/x", Action{Verb: "click", Target: fixtureRef("@ofixture-r1")}, true},
		{"allow-listed host mutating allowed", SessionPolicy{RemoteWrites: RemoteWriteDeny, AllowHosts: []string{"staging.internal"}}, "https://staging.internal/x", Action{Verb: "click", Target: fixtureRef("@ofixture-r1")}, false},
		{"malformed typed request rejected before policy/input", DefaultSessionPolicy(), "https://example.com/x", Action{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			impl := &browserCoreImpl{}
			impl.SetPolicy(tc.policy, tc.lastURL)
			err := tc.action.Validate()
			if err == nil {
				err = impl.checkActionPolicy(nil, tc.action)
			}
			if tc.wantBlock && err == nil {
				t.Fatalf("expected block, got allowed (policy=%v origin=%q action=%+v)", tc.policy, tc.lastURL, tc.action)
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
