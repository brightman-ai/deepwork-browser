// Package browser — TabIndex 单元测试 [v2 ]
//
// 绑定 TC (T6 v2):
// - (P1): Tab 四元身份唯一性 — 同 (TargetID, IdentityKey, WorkspaceID, Role) 不可重复 Register
// - 辅助: Role 矩阵守护 (IsValidRole), ByIdentity / ByWorkspace 切片正确性, Snapshot 顺序确定性
package browser

import (
	"sync"
	"testing"
	"time"
)

// TestTabIndex_Register_Lookup_Unregister_BasicHappyPath
// 验证 Register → Lookup 命中 → Unregister → Lookup 不命中.
func TestTabIndex_Register_Lookup_Unregister_BasicHappyPath(t *testing.T) {
	idx := NewTabIndex
	h := &TabHandle{
		TargetID: "target-A"
		IdentityKey: IdentityKey("idkey-1")
		WorkspaceID: "ws-0"
		Role: RoleAgent
	}

	if err := idx.Register(h); err != nil {
		t.Fatalf("Register: unexpected err: %v", err)
	}

	got, ok := idx.Lookup("target-A")
	if !ok {
		t.Fatalf("Lookup target-A: expected ok=true, got false")
	}
	if got.TargetID != "target-A" || got.IdentityKey != "idkey-1" || got.WorkspaceID != "ws-0" || got.Role != RoleAgent {
		t.Fatalf("Lookup returned wrong handle: %+v", got)
	}
	if got.AcquiredAt.IsZero {
		t.Fatalf("AcquiredAt should be auto-populated, got zero")
	}

	if err := idx.Unregister("target-A"); err != nil {
		t.Fatalf("Unregister: unexpected err: %v", err)
	}
	if _, ok := idx.Lookup("target-A"); ok {
		t.Fatalf("Lookup after Unregister: expected ok=false, got true")
	}
}

// TestTabIndex_Register_DuplicateTargetID_Rejects
// : 同 TargetID 第二次 Register → ErrTabAlreadyRegistered.
//
// 即使其他三元 (IdentityKey/Workspace/Role) 不同, TargetID 重复仍必须拒绝
// (TargetID 由 Chrome CDP 唯一生成, 重复 = 索引污染).
func TestTabIndex_TC09U43_DuplicateTargetIDRejected(t *testing.T) {
	idx := NewTabIndex
	h1 := &TabHandle{TargetID: "T1", IdentityKey: "id-1", WorkspaceID: "ws-0", Role: RoleAgent}
	h2 := &TabHandle{TargetID: "T1", IdentityKey: "id-2", WorkspaceID: "ws-1", Role: RoleHuman}

	if err := idx.Register(h1); err != nil {
		t.Fatalf("Register h1: unexpected err: %v", err)
	}
	err := idx.Register(h2)
	if err != ErrTabAlreadyRegistered {
		t.Fatalf("Register h2: expected ErrTabAlreadyRegistered, got %v", err)
	}

	// 正向断言: store size 仍为 1, 原 handle 未被覆盖.
	if idx.Len != 1 {
		t.Fatalf("Len: expected 1, got %d", idx.Len)
	}
	got, _ := idx.Lookup("T1")
	if got.IdentityKey != "id-1" || got.Role != RoleAgent {
		t.Fatalf("h1 was overwritten by h2: %+v", got)
	}
}

// TestTabIndex_TC09U43_QuadrupleUniquenessSameIdentitySameWorkspaceDifferentRole
// P1: 同 identity + 同 ws + 不同 Role → 两 Tab 共存 (各自有键, key 不等).
func TestTabIndex_TC09U43_QuadrupleUniqueness_SameIdSameWsDiffRole(t *testing.T) {
	idx := NewTabIndex
	hAgent := &TabHandle{TargetID: "T-agent", IdentityKey: "id-1", WorkspaceID: "ws-0", Role: RoleAgent}
	hHuman := &TabHandle{TargetID: "T-human", IdentityKey: "id-1", WorkspaceID: "ws-0", Role: RoleHuman}

	if err := idx.Register(hAgent); err != nil {
		t.Fatalf("Register hAgent: %v", err)
	}
	if err := idx.Register(hHuman); err != nil {
		t.Fatalf("Register hHuman: %v", err)
	}

	if idx.Len != 2 {
		t.Fatalf("Len: expected 2, got %d", idx.Len)
	}

	tabs := idx.ByIdentity("id-1")
	if len(tabs) != 2 {
		t.Fatalf("ByIdentity: expected 2 tabs, got %d", len(tabs))
	}
	roles := map[TabRole]bool{}
	for _, h := range tabs {
		roles[h.Role] = true
	}
	if !roles[RoleAgent] || !roles[RoleHuman] {
		t.Fatalf("ByIdentity: expected both roles, got %v", roles)
	}
}

// TestTabIndex_Register_RejectsInvalidRole — 防御 4 枚举矩阵.
func TestTabIndex_Register_RejectsInvalidRole(t *testing.T) {
	idx := NewTabIndex
	cases := TabRole{TabRole(""), TabRole("robot"), TabRole("HUMAN"), TabRole("admin")}
	for _, r := range cases {
		err := idx.Register(&TabHandle{TargetID: "T", IdentityKey: "id-1", WorkspaceID: "ws-0", Role: r})
		if err == nil {
			t.Fatalf("Register role=%q: expected err, got nil", r)
		}
	}
	if idx.Len != 0 {
		t.Fatalf("Len: expected 0 after all rejections, got %d", idx.Len)
	}
}

// TestTabIndex_Register_RejectsEmptyFields — 防御.
func TestTabIndex_Register_RejectsEmptyFields(t *testing.T) {
	idx := NewTabIndex
	if err := idx.Register(nil); err == nil {
		t.Fatalf("Register nil: expected err, got nil")
	}
	if err := idx.Register(&TabHandle{TargetID: "", IdentityKey: "id", Role: RoleAgent}); err == nil {
		t.Fatalf("Register empty TargetID: expected err, got nil")
	}
	if err := idx.Register(&TabHandle{TargetID: "T", IdentityKey: "", Role: RoleAgent}); err == nil {
		t.Fatalf("Register empty IdentityKey: expected err, got nil")
	}
}

// TestTabIndex_Unregister_NotFound — 校验 ErrTabNotFound.
func TestTabIndex_Unregister_NotFound(t *testing.T) {
	idx := NewTabIndex
	if err := idx.Unregister("ghost"); err != ErrTabNotFound {
		t.Fatalf("Unregister ghost: expected ErrTabNotFound, got %v", err)
	}
}

// TestTabIndex_ByIdentity_ByWorkspace_Filtering — 三元交叉过滤.
func TestTabIndex_ByIdentity_ByWorkspace_Filtering(t *testing.T) {
	idx := NewTabIndex
	register := func(t *testing.T, target string, id IdentityKey, ws string, role TabRole) {
		t.Helper
		if err := idx.Register(&TabHandle{TargetID: target, IdentityKey: id, WorkspaceID: ws, Role: role}); err != nil {
			t.Fatalf("Register %s: %v", target, err)
		}
	}
	register(t, "T-1A", "id-1", "ws-A", RoleAgent)
	register(t, "T-1B", "id-1", "ws-B", RoleAgent)
	register(t, "T-2A", "id-2", "ws-A", RoleHuman)

	id1 := idx.ByIdentity("id-1")
	if len(id1) != 2 {
		t.Fatalf("ByIdentity id-1: expected 2, got %d", len(id1))
	}
	wsA := idx.ByWorkspace("ws-A")
	if len(wsA) != 2 {
		t.Fatalf("ByWorkspace ws-A: expected 2, got %d", len(wsA))
	}
	// 字典序断言.
	if id1[0].TargetID != "T-1A" || id1[1].TargetID != "T-1B" {
		t.Fatalf("ByIdentity not sorted: %+v", id1)
	}
}

// TestTabIndex_Snapshot_DeterministicOrder — 确定输出.
func TestTabIndex_Snapshot_DeterministicOrder(t *testing.T) {
	idx := NewTabIndex
	_ = idx.Register(&TabHandle{TargetID: "T-Z", IdentityKey: "id-Z", WorkspaceID: "ws", Role: RoleAgent})
	_ = idx.Register(&TabHandle{TargetID: "T-A", IdentityKey: "id-A", WorkspaceID: "ws", Role: RoleAgent})
	_ = idx.Register(&TabHandle{TargetID: "T-M", IdentityKey: "id-A", WorkspaceID: "ws", Role: RoleHuman})

	snap := idx.Snapshot
	if len(snap) != 2 {
		t.Fatalf("Snapshot: expected 2 identities, got %d", len(snap))
	}
	if snap[0].Key != "id-A" || snap[1].Key != "id-Z" {
		t.Fatalf("Snapshot identities not sorted: %+v", snap)
	}
	if len(snap[0].Tabs) != 2 || snap[0].Tabs[0].TargetID != "T-A" || snap[0].Tabs[1].TargetID != "T-M" {
		t.Fatalf("Snapshot id-A tabs not sorted: %+v", snap[0].Tabs)
	}
}

// TestTabIndex_ConcurrentRegister — race detector 触发场景.
func TestTabIndex_ConcurrentRegister(t *testing.T) {
	idx := NewTabIndex
	var wg sync.WaitGroup
	const N = 50
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done
			h := &TabHandle{
				TargetID: targetIDForI(i)
				IdentityKey: IdentityKey("id-" + targetIDForI(i%5))
				WorkspaceID: "ws-0"
				Role: RoleAgent
				AcquiredAt: time.Now
			}
			_ = idx.Register(h)
		}(i)
	}
	wg.Wait
	if idx.Len != N {
		t.Fatalf("Len: expected %d unique tabs, got %d", N, idx.Len)
	}
}

func targetIDForI(i int) string {
	return "T-" + string(rune('A'+i%26)) + string(rune('0'+(i/26)%10))
}

// TestIsValidRole_FourEnumOnly — 矩阵守护.
func TestIsValidRole_FourEnumOnly(t *testing.T) {
	good := TabRole{RoleHuman, RoleAgent, RoleCouncil, RoleBackground}
	for _, r := range good {
		if !IsValidRole(r) {
			t.Fatalf("IsValidRole(%q): expected true", r)
		}
	}
	bad := TabRole{"", "robot", "Human", "agent ", "Bg"}
	for _, r := range bad {
		if IsValidRole(r) {
			t.Fatalf("IsValidRole(%q): expected false", r)
		}
	}
}
