package browser

import "testing"

func TestIsMutatingOp(t *testing.T) {
	writes := []string{"click", "clickat", "tap", "tapat", "tapxy", "fill", "type", "typetext", "press", "select", "check", "uncheck", "dragat", "swipeat", "dblclickat", "rclickat"}
	for _, op := range writes {
		if !IsMutatingOp(op) {
			t.Errorf("op %q should be mutating", op)
		}
	}
	reads := []string{"scroll", "scrollinto", "hover", "hoverat", "wheelat", "focus", "back", "forward"}
	for _, op := range reads {
		if IsMutatingOp(op) {
			t.Errorf("op %q should be read-only", op)
		}
	}
	// unknown verb is treated as mutating (fail-closed)
	if !IsMutatingOp("frobnicate") {
		t.Error("unknown op should be treated as mutating")
	}
	// case/space-insensitive
	if IsMutatingOp("  SCROLL ") {
		t.Error("SCROLL should be read-only regardless of case/space")
	}
}

func TestIsTrustedOrigin(t *testing.T) {
	cases := []struct {
		origin string
		allow  []string
		want   bool
	}{
		{"http://localhost:8080", nil, true},
		{"http://localhost", nil, true},
		{"https://app.localhost", nil, true},
		{"http://127.0.0.1:3000", nil, true},
		{"http://[::1]:8080", nil, true},
		{"http://192.168.1.10", nil, true},
		{"http://10.0.0.5:9000", nil, true},
		{"http://172.16.0.1", nil, true},
		{"http://0.0.0.0:8080", nil, true},
		{"https://www.baidu.com", nil, false},
		{"https://github.com", nil, false},
		{"", nil, false}, // unknown/opaque -> not trusted
		{"https://api.example.com", []string{"api.example.com"}, true}, // allowlisted
		{"https://api.example.com:443", []string{"api.example.com"}, true},
		{"https://other.com", []string{"api.example.com"}, false},
	}
	for _, c := range cases {
		if got := IsTrustedOrigin(c.origin, c.allow); got != c.want {
			t.Errorf("IsTrustedOrigin(%q, %v) = %v, want %v", c.origin, c.allow, got, c.want)
		}
	}
}

func TestEvaluateAct(t *testing.T) {
	deny := SessionPolicy{RemoteWrites: RemoteWriteDeny}
	confirm := SessionPolicy{RemoteWrites: RemoteWriteConfirm}
	allow := SessionPolicy{RemoteWrites: RemoteWriteAllow}
	zero := SessionPolicy{} // zero value == deny

	local := "http://localhost:8080"
	remote := "https://www.baidu.com"

	// reads are always allowed, any origin, any policy
	for _, p := range []SessionPolicy{deny, confirm, allow, zero} {
		for _, o := range []string{local, remote, ""} {
			if d := p.EvaluateAct("scroll", o); !d.Allowed || d.NeedsConfirm {
				t.Errorf("read scroll should always be allowed (policy=%v origin=%q) got %+v", p, o, d)
			}
		}
	}

	// writes to a trusted/local origin are always allowed, even under deny
	if d := deny.EvaluateAct("click", local); !d.Allowed {
		t.Errorf("local write should be allowed under deny, got %+v", d)
	}
	if d := zero.EvaluateAct("fill", local); !d.Allowed {
		t.Errorf("local write should be allowed under zero policy, got %+v", d)
	}

	// writes to remote/unknown under deny -> blocked
	if d := deny.EvaluateAct("click", remote); d.Allowed || d.NeedsConfirm {
		t.Errorf("remote write under deny should be blocked, got %+v", d)
	}
	if d := zero.EvaluateAct("click", ""); d.Allowed || d.NeedsConfirm {
		t.Errorf("unknown-origin write under zero(deny) should be blocked, got %+v", d)
	}

	// confirm -> NeedsConfirm (not outright allowed)
	if d := confirm.EvaluateAct("press", remote); d.Allowed || !d.NeedsConfirm {
		t.Errorf("remote write under confirm should need confirm, got %+v", d)
	}

	// allow -> permitted
	if d := allow.EvaluateAct("clickat", remote); !d.Allowed {
		t.Errorf("remote write under allow should be permitted, got %+v", d)
	}

	// allowlisted host write permitted even under deny
	dp := SessionPolicy{RemoteWrites: RemoteWriteDeny, AllowHosts: []string{"www.baidu.com"}}
	if d := dp.EvaluateAct("click", remote); !d.Allowed {
		t.Errorf("write to allow-listed host should be permitted under deny, got %+v", d)
	}
}

func TestNormalizeRemoteWritePolicy(t *testing.T) {
	if NormalizeRemoteWritePolicy("allow", RemoteWriteDeny) != RemoteWriteAllow {
		t.Error("allow should normalize to allow")
	}
	if NormalizeRemoteWritePolicy("CONFIRM", RemoteWriteDeny) != RemoteWriteConfirm {
		t.Error("CONFIRM should normalize to confirm")
	}
	if NormalizeRemoteWritePolicy("bogus", RemoteWriteDeny) != RemoteWriteDeny {
		t.Error("unknown should fall back to deny")
	}
	if NormalizeRemoteWritePolicy("", RemoteWriteAllow) != RemoteWriteAllow {
		t.Error("empty should use provided fallback")
	}
}
