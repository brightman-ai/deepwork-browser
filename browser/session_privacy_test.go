package browser

import (
	"context"
	"errors"
	"testing"
)

func TestSessionAuthorityAbsorbsPrivacyBeforeInput(t *testing.T) {
	impl := &browserCoreImpl{snapEngine: &snapshotEngine{}, actEngine: &actionEngine{}}
	info := &SessionInfo{AuthorityState: AuthorityAgentic}
	calls := 0
	impl.SetSessionAuthorityProvider(func(context.Context) (*SessionInfo, error) { calls++; return info, nil })
	check := func() {
		t.Helper()
		if err := impl.actEngine.authorityCheck(context.Background(), false, ""); err != nil {
			t.Fatal(err)
		}
	}
	check()
	if impl.snapEngine.redactFormValues {
		t.Fatal("ordinary session unexpectedly redacted")
	}
	info.RedactFormValues = true
	check()
	if !impl.snapEngine.redactFormValues {
		t.Fatal("input did not inherit privacy without a prior snapshot")
	}
	info.RedactFormValues = false
	check()
	if !impl.snapEngine.redactFormValues || calls != 3 {
		t.Fatal("privacy was not sticky across authority refreshes")
	}
}

func TestSessionAuthorityPrivacyFailuresStillRefuseInput(t *testing.T) {
	providerFailure := errors.New("session provider failed")
	for _, tc := range []struct {
		name string
		info *SessionInfo
		err  error
		want error
	}{
		{name: "provider error", err: providerFailure, want: providerFailure},
		{name: "missing session"},
		{name: "human control", info: &SessionInfo{AuthorityState: AuthorityHuman, RedactFormValues: true}, want: ErrTakeoverActive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			impl := &browserCoreImpl{snapEngine: &snapshotEngine{}, actEngine: &actionEngine{}}
			impl.SetSessionAuthorityProvider(func(context.Context) (*SessionInfo, error) { return tc.info, tc.err })
			err := impl.actEngine.authorityCheck(context.Background(), false, "")
			if err == nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("input authority error=%v, want=%v", err, tc.want)
			}
			if !impl.snapEngine.redactFormValues {
				t.Fatal("unavailable/private session exposed form facts")
			}
		})
	}
}
