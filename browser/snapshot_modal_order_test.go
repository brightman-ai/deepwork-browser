package browser

// Package browser — 模态优先 (modal-first) 排序测试 [BUG-MODAL-FIRST]
//
// 真因回归锚点: 模态/抽屉通常 Teleport/append 到 <body> 末尾 → A11y DFS 序天然排最后 →
// 被 observe 默认 top-20 截断 → agent 看不见"当前唯一能交互的那一层",却把背后点不到的
// 元素全列出来 → 一路点空气且 act 返回 success:true (silent failure)。
//
// 实测依据 (■ Chrome getFullAXTree, 2026-07-17):
//   - <div role="dialog" aria-modal="true"> → AX 节点 role=dialog + properties 含 modal=true
//   - 背景元素 **不会** 被 Chrome 标 ignored (ignoredReasons=[]) → 不能靠 Ignored 过滤识别遮挡
//   - display:none / hidden / visibility:hidden 的模态 → 整个子树不出现在 AX 树 → 不会误判活跃

import (
	"testing"

	"github.com/chromedp/cdproto/accessibility"
	cdp "github.com/chromedp/cdproto/cdp"
	"github.com/go-json-experiment/json/jsontext"
)

// — 构造 mock AX 节点的 helpers —

func axValue(raw string) *accessibility.Value {
	return &accessibility.Value{Value: jsontext.Value(raw)}
}

func axProp(name, raw string) *accessibility.Property {
	return &accessibility.Property{
		Name:  accessibility.PropertyName(name),
		Value: axValue(raw),
	}
}

// axNode 构造一个 AX 节点。role/name 以 JSON 串形式存放（与 CDP 实际返回一致）。
func axNode(id int, role, name string, children ...int) *accessibility.Node {
	childIDs := make([]accessibility.NodeID, 0, len(children))
	for _, c := range children {
		childIDs = append(childIDs, accessibility.NodeID(itoaAX(c)))
	}
	return &accessibility.Node{
		NodeID:           accessibility.NodeID(itoaAX(id)),
		Role:             axValue(`"` + role + `"`),
		Name:             axValue(`"` + name + `"`),
		ChildIDs:         childIDs,
		BackendDOMNodeID: backendID(id),
	}
}

func itoaAX(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func backendID(i int) cdp.BackendNodeID { return cdp.BackendNodeID(i * 100) }

// refNames 提取 refs 的 name 序列，便于断言顺序。
func refNames(refs []ElementRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Name)
	}
	return out
}

// ============================================================
// § 主回归: 模态子树必须排在最前
// ============================================================

// TestModalFirst_TeleportedModalHoistedToFront 复现真实 bug 形状:
// 背景 20+ 元素在 DOM 前面, 模态 append 在最后 → 必须被提到最前。
func TestModalFirst_TeleportedModalHoistedToFront(t *testing.T) {
	// root(1) → [bg(2), modal(20)]
	// bg(2) → 3 个背景按钮 + 2 个 tab
	// modal(20) role=dialog + modal=true → [textbox(21), button(22)]
	nodes := []*accessibility.Node{
		axNode(1, "RootWebArea", "工作台", 2, 20),
		axNode(2, "generic", "", 3, 4, 5, 6, 7),
		axNode(3, "button", "快捷新建 topic"),
		axNode(4, "button", "审核队列"),
		axNode(5, "button", "通知"),
		axNode(6, "tab", "🧭 Topic"),
		axNode(7, "tab", "🎯 目标"),
		axNode(20, "dialog", "身份设置", 21, 22),
		axNode(21, "textbox", "例：zhangsan 或 zhang.san"),
		axNode(22, "button", "就是我"),
	}
	// 关键: dialog 带 modal=true（Chrome 对 aria-modal="true" 的实际投影）
	nodes[7].Properties = []*accessibility.Property{axProp("modal", "true")}

	refs := extractInteractableRefs(nodes)
	names := refNames(refs)

	if len(refs) < 2 {
		t.Fatalf("modal-first: expected refs, got %d", len(refs))
	}

	// 断言 1: 模态两个元素排最前
	if names[0] != "例：zhangsan 或 zhang.san" || names[1] != "就是我" {
		t.Errorf("modal-first: modal elements must be FIRST, got order=%v", names)
	}

	// 断言 2: Ref 序号 == 最终位置（@rN 依赖这一点）
	if refs[0].Ref != "e1" || refs[1].Ref != "e2" {
		t.Errorf("modal-first: refs must be renumbered after sort, got %q,%q", refs[0].Ref, refs[1].Ref)
	}

	// 断言 3: 模态元素 ModalRank>0 且未被标 blocked
	for i := 0; i < 2; i++ {
		if refs[i].ModalRank == 0 {
			t.Errorf("modal-first: %q should have ModalRank>0", refs[i].Name)
		}
		if refs[i].BlockedByModal {
			t.Errorf("modal-first: top-layer %q must NOT be blocked", refs[i].Name)
		}
	}

	// 断言 4: 背景元素全部标 blocked（点了会静默失败）
	for _, r := range refs[2:] {
		if !r.BlockedByModal {
			t.Errorf("modal-first: background %q must be marked BlockedByModal", r.Name)
		}
	}

	// 断言 5: 背景元素没有被丢弃（只降权不删除）
	if len(refs) != 7 {
		t.Errorf("modal-first: background must be kept (demoted, not dropped); want 7 refs, got %d: %v",
			len(refs), names)
	}

	t.Logf("modal-first PASS order=%v", names)
}

// TestModalFirst_NoModal_KeepsDFSOrder 无模态时必须保持原 DFS（视觉阅读）顺序，零行为变化。
func TestModalFirst_NoModal_KeepsDFSOrder(t *testing.T) {
	nodes := []*accessibility.Node{
		axNode(1, "RootWebArea", "页面", 2, 3, 4),
		axNode(2, "button", "第一"),
		axNode(3, "button", "第二"),
		axNode(4, "button", "第三"),
	}
	refs := extractInteractableRefs(nodes)
	names := refNames(refs)
	// 注: RootWebArea 无 focusable 属性 → 不成 ref（真实页面里它带 focusable=true 才会出现）
	want := []string{"第一", "第二", "第三"}
	for i := range want {
		if i >= len(names) || names[i] != want[i] {
			t.Fatalf("no-modal: DFS order must be preserved; want %v got %v", want, names)
		}
	}
	for _, r := range refs {
		if r.BlockedByModal || r.ModalRank != 0 {
			t.Errorf("no-modal: %q must not be marked modal/blocked", r.Name)
		}
	}
	t.Logf("no-modal PASS order=%v", names)
}

// TestModalFirst_NonModalDialogNotHoisted 非模态 dialog（无 modal=true，如非阻断浮窗）
// 不应被提前 —— 它不拦截输入，背景仍可交互。
func TestModalFirst_NonModalDialogNotHoisted(t *testing.T) {
	nodes := []*accessibility.Node{
		axNode(1, "RootWebArea", "页面", 2, 10),
		axNode(2, "button", "背景按钮"),
		axNode(10, "dialog", "非模态浮窗", 11), // 没有 modal=true
		axNode(11, "button", "浮窗按钮"),
	}
	refs := extractInteractableRefs(nodes)
	names := refNames(refs)
	if names[0] != "背景按钮" {
		t.Errorf("non-modal dialog: must NOT hoist; want 背景按钮 first, got %v", names)
	}
	for _, r := range refs {
		if r.BlockedByModal {
			t.Errorf("non-modal dialog: nothing should be blocked, but %q was", r.Name)
		}
	}
	t.Logf("non-modal-dialog PASS order=%v", names)
}

// TestModalFirst_IgnoredModalNotHoisted 被 Chrome 标 ignored 的 dialog（隐藏态残留）
// 不得判为活跃 —— 否则关掉的弹窗会把真正能点的元素挤下去。
func TestModalFirst_IgnoredModalNotHoisted(t *testing.T) {
	nodes := []*accessibility.Node{
		axNode(1, "RootWebArea", "页面", 2, 10),
		axNode(2, "button", "背景按钮"),
		axNode(10, "dialog", "已关闭弹窗", 11),
		axNode(11, "button", "隐藏按钮"),
	}
	nodes[2].Properties = []*accessibility.Property{axProp("modal", "true")}
	nodes[2].Ignored = true // 隐藏 → Chrome 标 ignored

	refs := extractInteractableRefs(nodes)
	for _, r := range refs {
		if r.BlockedByModal {
			t.Errorf("ignored modal: must not activate modal layer, but %q got blocked", r.Name)
		}
	}
	if names := refNames(refs); names[0] != "背景按钮" {
		t.Errorf("ignored modal: background must stay first; got %v", names)
	}
	t.Log("ignored-modal PASS: 隐藏模态不激活模态层")
}

// TestModalFirst_StackedModals_TopmostWins 模态叠模态（如弹窗里再开确认框）:
// 栈顶（DOM 中更晚出现）优先, 下层模态元素同样标 blocked。
func TestModalFirst_StackedModals_TopmostWins(t *testing.T) {
	nodes := []*accessibility.Node{
		axNode(1, "RootWebArea", "页面", 2, 10, 20),
		axNode(2, "button", "背景按钮"),
		axNode(10, "dialog", "第一层弹窗", 11),
		axNode(11, "button", "第一层按钮"),
		axNode(20, "dialog", "确认框", 21),
		axNode(21, "button", "确认"),
	}
	nodes[2].Properties = []*accessibility.Property{axProp("modal", "true")}
	nodes[4].Properties = []*accessibility.Property{axProp("modal", "true")}

	refs := extractInteractableRefs(nodes)
	names := refNames(refs)

	if names[0] != "确认" {
		t.Errorf("stacked modals: topmost modal element must be first, got %v", names)
	}
	if refs[0].BlockedByModal {
		t.Error("stacked modals: topmost 确认 must NOT be blocked")
	}
	// 下层模态 + 背景都应标 blocked
	for _, r := range refs[1:] {
		if !r.BlockedByModal {
			t.Errorf("stacked modals: %q (below topmost) must be blocked", r.Name)
		}
	}
	t.Logf("stacked-modals PASS order=%v", names)
}

// TestModalFirst_AlertDialog alertdialog（confirm 框）同样是模态容器。
func TestModalFirst_AlertDialog(t *testing.T) {
	nodes := []*accessibility.Node{
		axNode(1, "RootWebArea", "页面", 2, 3, 10),
		axNode(2, "button", "背景1"),
		axNode(3, "button", "背景2"),
		axNode(10, "alertdialog", "确认删除", 11),
		axNode(11, "button", "确定删除"),
	}
	nodes[3].Properties = []*accessibility.Property{axProp("modal", "true")}

	refs := extractInteractableRefs(nodes)
	if names := refNames(refs); names[0] != "确定删除" {
		t.Errorf("alertdialog: must hoist to front, got %v", names)
	}
	t.Log("alertdialog PASS")
}
