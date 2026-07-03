package testing

import (
	"os"
	"testing"
)

// TestUsingClassConflict covers the generalized oracle-class rule: a structural
// predicate's intrinsic class must not be re-labeled into a UX acceptance class
// (visual/behavior), regardless of how many classes inferUsing returns.
func TestUsingClassConflict(t *testing.T) {
	tests := []struct {
		name        string
		inferred    []string
		callerUsing []string
		wantConf    bool
	}{
		{"structural relabeled visual", []string{"structural"}, []string{"visual"}, true},
		{"structural relabeled behavior", []string{"structural"}, []string{"behavior"}, true},
		{"structural narrowed to structural (consistent)", []string{"structural"}, []string{"structural"}, false},
		{"multi-class incl structural relabeled visual", []string{"structural", "page"}, []string{"visual"}, true},
		{"non-structural (page) + visual is not this rule's concern", []string{"page"}, []string{"visual"}, false},
		{"free-form (nil inferred) + visual allowed", nil, []string{"visual"}, false},
		{"structural + non-acceptance caller class allowed", []string{"structural"}, []string{"page"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := usingClassConflict(tc.inferred, tc.callerUsing)
			if got != tc.wantConf {
				t.Fatalf("usingClassConflict(%v,%v)=%v want %v", tc.inferred, tc.callerUsing, got, tc.wantConf)
			}
		})
	}
}

// TestEvaluateWithUsingOracleReject verifies REJECT-by-default: a structural
// predicate labeled --using visual is BLOCKED, and DW_BROWSER_ORACLE_WARN_ONLY=1
// downgrades to a non-blocking warning.
func TestEvaluateWithUsingOracleReject(t *testing.T) {
	e := &AssertionEngine{}
	obs := &Observation{}

	// Default: hard reject → StatusBlocked with REJECT reason.
	os.Unsetenv("DW_BROWSER_ORACLE_WARN_ONLY")
	res := e.EvaluateWithUsing(obs, "visible(#foo)", []string{"visual"})
	if res.Status != StatusBlocked {
		t.Fatalf("default oracle-class: expected StatusBlocked, got %s (reason=%q)", res.Status, res.Reason)
	}
	if res.OracleWarning == "" {
		t.Fatalf("expected OracleWarning to be annotated, got empty")
	}

	// WARN_ONLY escape: not blocked by the oracle-class rule; warning annotated.
	t.Setenv("DW_BROWSER_ORACLE_WARN_ONLY", "1")
	res2 := e.EvaluateWithUsing(obs, "visible(#foo)", []string{"visual"})
	if res2.Status == StatusBlocked && res2.Reason != "" && len(res2.Reason) >= 6 && res2.Reason[:6] == "REJECT" {
		t.Fatalf("WARN_ONLY: expected no oracle-class REJECT, got blocked: %q", res2.Reason)
	}
	if res2.OracleWarning == "" {
		t.Fatalf("WARN_ONLY: expected OracleWarning annotation, got empty")
	}
}
