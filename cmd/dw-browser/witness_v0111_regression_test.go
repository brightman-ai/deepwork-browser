package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/brightman-ai/deepwork-browser/browser"
)

// [S2] 一次校验期失败的 act 不得作废整份 observation。
// 现场症状: observe→@r11 ✓→@r12 ✓→一个打错的定位器 ✗→@r13 报
// "not found in current observation", 整批句柄陪葬, 必须重新 observe。
func TestFailedValidationKeepsSessionRefAuthority(t *testing.T) {
	refs := []browser.SessionRef{
		{Ref: "@r1", BackendNodeID: 11, Role: "button", Name: "继续", Visible: true, Observed: true},
		{Ref: "@r2", BackendNodeID: 12, Role: "textbox", Name: "搜索", Visible: true, Observed: true},
	}

	// 校验期失败: 没派发任何输入 → 句柄照旧, 且不留下会被 normalize 撤销的 outcome。
	preDispatch := &browser.SessionInfo{
		SessionID: "s1", PageURL: "http://127.0.0.1/app",
		Refs: append([]browser.SessionRef(nil), refs...),
	}
	applyFailedActionOutcome(preDispatch, true)
	if len(preDispatch.Refs) != 2 {
		t.Fatalf("校验期失败作废了 refs: %+v", preDispatch.Refs)
	}
	if preDispatch.LastActionOutcome != browser.SessionActionOutcomeReconciled {
		t.Fatalf("outcome=%q, want reconciled", preDispatch.LastActionOutcome)
	}
	// NormalizeSessionInfo 是最后一道闸: 它只在 in_progress/unknown 时撤销 refs,
	// 所以 reconciled 必须真的能把句柄带过这道闸。
	browser.NormalizeSessionInfo(preDispatch)
	if len(preDispatch.Refs) != 2 || preDispatch.Refs[0].Ref != "@r1" {
		t.Fatalf("归一化后句柄丢失: %+v", preDispatch.Refs)
	}

	// 派发之后失败: 页面可能已变 → 句柄必须作废(旧行为在这一支是对的)。
	dispatched := &browser.SessionInfo{
		SessionID: "s1", PageURL: "http://127.0.0.1/app",
		Refs: append([]browser.SessionRef(nil), refs...),
	}
	applyFailedActionOutcome(dispatched, false)
	if len(dispatched.Refs) != 0 {
		t.Fatalf("派发后失败仍保留了句柄: %+v", dispatched.Refs)
	}
	if dispatched.LastActionOutcome != browser.SessionActionOutcomeUnknown {
		t.Fatalf("outcome=%q, want unknown", dispatched.LastActionOutcome)
	}
}

// [S6] 坐标动作的 hit 必须出现在 act 的 JSON 输出里, 并且带上"success≠命中"的警示,
// 否则 agent 仍然只会去看 success。
func TestActOutputCarriesCoordinateHitAndItsCaveat(t *testing.T) {
	output := map[string]interface{}{"success": true}
	injectActionFidelity(output, browser.ActionFidelityReport{
		Fidelity: browser.InteractionFidelityStrictHuman,
		Hit: &browser.CoordinateHit{
			X: 100, Y: 82, Role: "", Name: "", Selector: "#wt-occluder", Tag: "div",
		},
	})
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"hit"`) || !strings.Contains(string(encoded), "#wt-occluder") {
		t.Fatalf("act 输出没有命中回报: %s", encoded)
	}
	note, _ := output["hit_note"].(string)
	if !strings.Contains(note, "success:true") {
		t.Fatalf("缺少 success≠命中 的警示: %q", note)
	}

	// 非坐标动作不该凭空多出 hit 字段。
	plain := map[string]interface{}{"success": true}
	injectActionFidelity(plain, browser.ActionFidelityReport{Fidelity: browser.InteractionFidelityStrictHuman})
	if _, ok := plain["hit"]; ok {
		t.Fatalf("语义动作也带上了 hit: %+v", plain)
	}
}

// [S10] --scope 只接受 visible|all, 且默认 visible(既有口径不变)。
func TestHitAuditScopeFlagParsing(t *testing.T) {
	clean, want, scope, err := stripObserveHitAuditFlag([]string{"--id", "s1", "--hit-audit", "--scope", "all"})
	if err != nil || !want || scope != hitAuditScopeAll || strings.Join(clean, " ") != "--id s1" {
		t.Fatalf("scope all=(%q,%t,%q,%v)", clean, want, scope, err)
	}
	if _, _, _, err := stripObserveHitAuditFlag([]string{"--hit-audit", "--scope=nonsense"}); err == nil {
		t.Fatal("非法 scope 被静默接受")
	}
	_, _, scope, err = stripObserveHitAuditFlag([]string{"--hit-audit", "--scope=visible"})
	if err != nil || scope != hitAuditScopeVisible {
		t.Fatalf("scope visible=(%q,%v)", scope, err)
	}
}
