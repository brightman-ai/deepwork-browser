package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// ============================================================
// § ActionEngine [Ref: CAP-BS09-C2, T5-B5]
// ============================================================

// actionEngine 实现操作解析与执行。
type actionEngine struct {
	snapEngine *snapshotEngine
	// seeToClick is enabled by every recognized agent-driven scenario. humanInput
	// reuses the existing InputGateway dispatch path (Gaussian jitter + real CDP
	// mouse events) without acquiring a takeover lease.
	seeToClick bool
	// interactionPolicy is assigned only from ScenarioInteractionPolicy. The
	// legacy seeToClick mirror remains for the established visibility code.
	interactionPolicy       InteractionPolicy
	humanInput              *InputGateway
	fidelityMu              sync.Mutex
	lastReport              ActionFidelityReport
	humanFocusBackendNodeID int64
	// pointerGuard 指针动作前置守卫（browser chrome 仿真：拒绝点击遮挡带内的点）。
	// nil = 无守卫。由 browserCoreImpl.EnableBrowserChromeSim 在会话启用时安装
	// （单写者：act 执行前设置，执行期只读）。
	pointerGuard func(ctx context.Context, x, y float64) error
	// pageScale 镜像 Emulation.setPageScaleFactor 当前值（act "zoom" 更新；0 视为 1）。
	// psMu 保护 pageScale/keyboardVisible：写(executeZoom/executeKeyboard)发生在
	// impl.mu.RLock 下，读(PageScale/KeyboardVisible)亦然，
	// RWMutex 读锁不互斥这对读写 —— 单进程串行下无活 race，但语义上必须自持锁。
	psMu      sync.Mutex
	pageScale float64
	// keyboardCtl 软键盘态控制器（browser chrome 仿真安装；nil = 键盘仿真不可用）。
	// keyboardVisible 镜像当前键盘态（act "keyboard"/焦点自动同步更新；psMu 保护）。
	keyboardCtl     func(ctx context.Context, show bool) error
	keyboardVisible bool
}

// newActionEngine 创建 ActionEngine 实例。
func newActionEngine(snapEngine *snapshotEngine) *actionEngine {
	return &actionEngine{
		snapEngine: snapEngine,
		humanInput: NewInputGateway(nil, nil),
	}
}

func (e *actionEngine) setSeeToClick(enabled bool) {
	e.seeToClick = enabled
}

func (e *actionEngine) setInteractionPolicy(policy InteractionPolicy) {
	e.interactionPolicy = policy
	e.seeToClick = policy.SeeToClick
}

func (e *actionEngine) beginActionFidelity() {
	e.fidelityMu.Lock()
	// Dispatched starts true on purpose. "Input may have reached the page" is the
	// safe assumption; it is narrowed to false only on code paths that provably
	// run before any input is sent. A missed narrowing costs an extra observe;
	// a wrong narrowing would keep refs alive across a page change.
	e.lastReport = ActionFidelityReport{Fidelity: e.interactionPolicy.Fidelity, Dispatched: true}
	e.fidelityMu.Unlock()
}

// markPreDispatchFailure records that this action died in its validation phase:
// nothing was parsed into input, nothing was sent, the page cannot have moved.
// Callers of the CLI use it to keep the previous observation's refs alive.
func (e *actionEngine) markPreDispatchFailure() {
	e.fidelityMu.Lock()
	e.lastReport.Dispatched = false
	e.fidelityMu.Unlock()
}

// failedBeforeDispatch tags err as a validation-phase failure and returns it, so
// error sites stay single-expression.
func (e *actionEngine) failedBeforeDispatch(err error) error {
	e.markPreDispatchFailure()
	return err
}

func (e *actionEngine) updateActionFidelity(update func(*ActionFidelityReport)) {
	e.fidelityMu.Lock()
	update(&e.lastReport)
	e.fidelityMu.Unlock()
}

func (e *actionEngine) lastActionFidelity() ActionFidelityReport {
	e.fidelityMu.Lock()
	defer e.fidelityMu.Unlock()
	report := e.lastReport
	report.HumanPath = append([]string(nil), report.HumanPath...)
	return report
}

func (e *actionEngine) restoreHumanFocus(state HumanFocusState) {
	e.fidelityMu.Lock()
	e.humanFocusBackendNodeID = state.BackendNodeID
	e.fidelityMu.Unlock()
}

func (e *actionEngine) recordHumanFocus(backendNodeID int64) {
	e.fidelityMu.Lock()
	e.humanFocusBackendNodeID = backendNodeID
	e.lastReport.FocusUpdated = true
	e.lastReport.FocusedBackend = backendNodeID
	e.fidelityMu.Unlock()
}

func (e *actionEngine) currentHumanFocus() int64 {
	e.fidelityMu.Lock()
	defer e.fidelityMu.Unlock()
	return e.humanFocusBackendNodeID
}

// setPointerGuard 安装指针守卫（见 pointerGuard 字段）。
func (e *actionEngine) setPointerGuard(g func(ctx context.Context, x, y float64) error) {
	e.pointerGuard = g
}

// guardPoint 指针动作统一前置：把 (x,y) 交守卫判定，守卫拒绝则动作 fail-loud。
// 守卫按定义跑在派发之前，所以拒绝 = 校验期失败，不得作废既有 observation。
func (e *actionEngine) guardPoint(ctx context.Context, x, y float64) error {
	if e.pointerGuard == nil {
		return nil
	}
	if err := e.pointerGuard(ctx, x, y); err != nil {
		return e.failedBeforeDispatch(err)
	}
	return nil
}

// PageScale 当前页面缩放（默认 1.0）。
func (e *actionEngine) PageScale() float64 {
	e.psMu.Lock()
	defer e.psMu.Unlock()
	if e.pageScale <= 0 {
		return 1
	}
	return e.pageScale
}

// restorePageScale 对齐本进程镜像的缩放值（CDP 重放由调用方负责）。
func (e *actionEngine) restorePageScale(scale float64) {
	if scale > 0 {
		e.psMu.Lock()
		e.pageScale = scale
		e.psMu.Unlock()
	}
}

// setKeyboardController 安装软键盘态控制器（见 keyboardCtl 字段）。
func (e *actionEngine) setKeyboardController(ctl func(ctx context.Context, show bool) error) {
	e.keyboardCtl = ctl
}

// KeyboardVisible 当前软键盘态（默认 false）。
func (e *actionEngine) KeyboardVisible() bool {
	e.psMu.Lock()
	defer e.psMu.Unlock()
	return e.keyboardVisible
}

// restoreKeyboard 对齐本进程镜像的键盘态（页面侧推入由调用方负责）。
func (e *actionEngine) restoreKeyboard(visible bool) {
	e.psMu.Lock()
	e.keyboardVisible = visible
	e.psMu.Unlock()
}

// executeKeyboard 软键盘态显式切换（act "keyboard show|hide"，REQ-BC-12）。
func (e *actionEngine) executeKeyboard(ctx context.Context, value string) error {
	if e.keyboardCtl == nil {
		return fmt.Errorf("%w: keyboard simulation requires browser chrome sim (open with --persona mobile)", ErrActFailed)
	}
	var show bool
	switch value {
	case "show":
		show = true
	case "hide":
		show = false
	default:
		return fmt.Errorf("%w: keyboard expects 'show' or 'hide', got %q", ErrActFailed, value)
	}
	if err := e.keyboardCtl(ctx, show); err != nil {
		return err
	}
	e.restoreKeyboard(show)
	return nil
}

// autoSyncKeyboard 焦点自动同步（真机语义：点输入框键盘弹起、失焦收起）。
// 在可能改变焦点的 op 之后调用；探测/切换失败只记日志不失败主 op（辅助路径）。
func (e *actionEngine) autoSyncKeyboard(ctx context.Context) {
	if e.keyboardCtl == nil {
		return
	}
	var editable bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(editableFocusProbeJS, &editable)); err != nil {
		return
	}
	if editable == e.KeyboardVisible() {
		return
	}
	if err := e.keyboardCtl(ctx, editable); err != nil {
		log.Printf("[BROWSER] keyboard auto-sync (%v) failed: %v", editable, err)
		return
	}
	e.restoreKeyboard(editable)
}

// ParsedAction 是解析后的操作结构。
type ParsedAction struct {
	Op      string  // "click" | "clickxy" | "clickat" | "hoverat" | "dragat" | "tap" | "tapat" | "type" | "scroll" | "hover" | "select"
	Ref     string  // Element Ref（如 "e3"）或语义选择器（如 "#testid", "button:'name'"）
	Value   string  // type/select 的值
	CoordX  float64 // 有 Ref: 元素内相对 X(0..1)；无 Ref: 视口 CSS px
	CoordY  float64 // 有 Ref: 元素内相对 Y(0..1)；无 Ref: 视口 CSS px
	CoordX2 float64 // dragat/swipeat 终点相对 X 坐标（0..1）
	CoordY2 float64 // dragat/swipeat 终点相对 Y 坐标（0..1）
	DeltaX  float64 // wheelat 横向滚轮位移（带符号像素）
	DeltaY  float64 // wheelat 纵向滚轮位移（带符号像素）
	// ScrollTarget is empty for viewport scrolling; otherwise the pointer is
	// moved to this element before wheel input. Steps=0 means the human default
	// of approximately one viewport.
	ScrollTarget string
	Direction    string
	Steps        int
}

// SelectorType 语义选择器类型。
type SelectorType int

const (
	SelectorSessionRef    SelectorType = iota // @rN — session ref
	SelectorTestID                            // #testid
	SelectorCanonical                         // role=button[name*="x"][nth=3]
	SelectorScoped                            // A >> B
	SelectorRoleName                          // role:'name' (shorthand, contains)
	SelectorRoleNameExact                     // role="name" (shorthand, exact)
	SelectorRole                              // role (bare role, first match)
	SelectorCSS                               // css=... or fallback
	SelectorLegacyRef                         // e{N} — rejected
)

// CanonicalFilter canonical DSL 过滤器（name/placeholder/testid + op）。
type CanonicalFilter struct {
	Field string // "name", "placeholder", "testid"
	Op    string // "=" (exact), "*=" (contains), "^=" (prefix)
	Value string
}

// ParsedSelector 解析后的语义选择器。
type ParsedSelector struct {
	SType       SelectorType
	TestID      string
	Role        string
	Name        string
	NameOp      string            // "=" exact | "*=" contains | "^=" prefix
	Filters     []CanonicalFilter // canonical DSL 过滤器
	Nth         int               // nth filter (0 = no nth)
	ScopeParent *ParsedSelector   // A >> B 中的 A
	Raw         string            // 原始字符串
	SessionRef  int               // @rN 的 N
}

// ParseSelector 解析选择器字符串。
//
// 优先级顺序:
//  1. @rN               — session ref（仅 session 模式有效）
//  2. #testid           — data-testid
//  3. role=TYPE[...]    — canonical DSL
//  4. A >> B            — scoped selector
//  5. role:'name'       — shorthand（contains）
//  6. role="name"       — shorthand（exact）
//  7. css=...           — explicit CSS
//  8. e{N}              — legacy ref（拒绝）
//  9. pure identifier   — bare role
//
// 10. everything else   — CSS fallback
func ParseSelector(selector string) (*ParsedSelector, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil, fmt.Errorf("%w: empty selector", ErrActFailed)
	}

	// 1. @rN — session ref
	if strings.HasPrefix(selector, "@r") {
		nStr := selector[2:]
		n := 0
		for _, c := range nStr {
			if c < '0' || c > '9' {
				n = -1
				break
			}
			n = n*10 + int(c-'0')
		}
		if n > 0 {
			return &ParsedSelector{SType: SelectorSessionRef, SessionRef: n, Raw: selector}, nil
		}
	}

	// 2. e{N} 位置编码 → 拒绝
	if isLegacyRef(selector) {
		return nil, fmt.Errorf(
			"位置编码 %q 不可用于 act（DOM 变化会导致序号漂移）。\n"+
				"请使用语义选择器:\n"+
				"  click #<testid>           — 按 data-testid\n"+
				"  click button:'<名称>'      — 按 ARIA role + name\n"+
				"  type textbox:'<名称>' 'text'\n"+
				"运行 'dw-browser snap <url>' 查看可用的 role 和 name。",
			selector)
	}

	// 3. #testid — # 后跟纯标识符（字母/数字/连字符/下划线）
	if selector[0] == '#' && isIdentifier(selector[1:]) {
		return &ParsedSelector{SType: SelectorTestID, TestID: selector[1:], Raw: selector}, nil
	}

	// 4. role=TYPE[filters...] — canonical DSL
	if strings.HasPrefix(selector, "role=") {
		return parseCanonicalSelector(selector)
	}

	// 5. A >> B — scoped selector
	if idx := strings.Index(selector, " >> "); idx > 0 {
		parentStr := strings.TrimSpace(selector[:idx])
		childStr := strings.TrimSpace(selector[idx+4:])
		parent, err := ParseSelector(parentStr)
		if err != nil {
			return nil, fmt.Errorf("scoped selector parent: %w", err)
		}
		child, err := ParseSelector(childStr)
		if err != nil {
			return nil, fmt.Errorf("scoped selector child: %w", err)
		}
		child.ScopeParent = parent
		child.Raw = selector
		return child, nil
	}

	// 6. css=... — explicit CSS
	if strings.HasPrefix(selector, "css=") {
		return &ParsedSelector{SType: SelectorCSS, Raw: selector[4:]}, nil
	}

	// 7. role:'name' — shorthand（contains）
	if idx := strings.Index(selector, ":"); idx > 0 && isAlphaOnly(selector[:idx]) {
		nameRaw := selector[idx+1:]
		if len(nameRaw) >= 2 && (nameRaw[0] == '\'' || nameRaw[0] == '"') {
			name := strings.Trim(nameRaw, "'\"")
			return &ParsedSelector{SType: SelectorRoleName, Role: selector[:idx], Name: name, NameOp: "*=", Raw: selector}, nil
		}
	}

	// 8. role="name" — shorthand（exact）
	if idx := strings.Index(selector, "="); idx > 0 && isAlphaOnly(selector[:idx]) {
		nameRaw := selector[idx+1:]
		if len(nameRaw) >= 2 && (nameRaw[0] == '\'' || nameRaw[0] == '"') {
			name := strings.Trim(nameRaw, "'\"")
			return &ParsedSelector{SType: SelectorRoleNameExact, Role: selector[:idx], Name: name, NameOp: "=", Raw: selector}, nil
		}
	}

	// 9. 纯标识符单词 → ARIA role
	if isIdentifier(selector) {
		return &ParsedSelector{SType: SelectorRole, Role: selector, Raw: selector}, nil
	}

	// 10. 其他一切 → CSS 选择器（兜底）
	return &ParsedSelector{SType: SelectorCSS, Raw: selector}, nil
}

// parseCanonicalSelector 解析 canonical DSL: role=TYPE[field op "value"][nth=N]。
func parseCanonicalSelector(selector string) (*ParsedSelector, error) {
	// Strip leading "role="
	rest := selector[5:]
	// Find role type (up to first '[' or end)
	roleEnd := strings.IndexByte(rest, '[')
	var role string
	var filterStr string
	if roleEnd < 0 {
		role = rest
	} else {
		role = rest[:roleEnd]
		filterStr = rest[roleEnd:]
	}
	role = strings.TrimSpace(role)
	if role == "" {
		return nil, fmt.Errorf("%w: canonical selector missing role", ErrActFailed)
	}

	ps := &ParsedSelector{SType: SelectorCanonical, Role: role, Raw: selector}

	// Parse filters: [field op "value"] [nth=N]
	for filterStr != "" {
		filterStr = strings.TrimSpace(filterStr)
		if filterStr == "" {
			break
		}
		if filterStr[0] != '[' {
			break
		}
		end := strings.IndexByte(filterStr, ']')
		if end < 0 {
			break
		}
		inner := filterStr[1:end]
		filterStr = filterStr[end+1:]

		// Parse inner: field op "value" or field=N (for nth)
		inner = strings.TrimSpace(inner)
		if strings.HasPrefix(inner, "nth=") {
			nStr := inner[4:]
			n := 0
			for _, c := range nStr {
				if c >= '0' && c <= '9' {
					n = n*10 + int(c-'0')
				}
			}
			ps.Nth = n
			continue
		}

		// Detect operator
		ops := []string{"*=", "^=", "="}
		var field, op, val string
		for _, candidate := range ops {
			if idx := strings.Index(inner, candidate); idx > 0 {
				field = strings.TrimSpace(inner[:idx])
				op = candidate
				val = strings.TrimSpace(inner[idx+len(candidate):])
				val = strings.Trim(val, `"'`)
				break
			}
		}
		if field == "" {
			continue
		}

		// Shorthand: if field is "name", populate top-level Name/NameOp
		if field == "name" && ps.Name == "" {
			ps.Name = val
			ps.NameOp = op
		}
		ps.Filters = append(ps.Filters, CanonicalFilter{Field: field, Op: op, Value: val})
	}

	return ps, nil
}

// isIdentifier 判断字符串是否为纯标识符（字母/数字/连字符/下划线，无空格或 CSS 特殊字符）。
func isIdentifier(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// isAlphaOnly 判断字符串是否全为字母。
func isAlphaOnly(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}

// isLegacyRef 判断字符串是否为旧式位置编码 "e{N}"。
func isLegacyRef(s string) bool {
	if len(s) < 2 || s[0] != 'e' {
		return false
	}
	for _, c := range s[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ParseAction 解析操作语法字符串 [TC-09-U-05, TC-09-U-06]。
//
// 支持操作:
//   - "click #testid" | "click button:'名称'"
//   - "click 320,240" — 视口 CSS 像素坐标真实鼠标点击
//   - 坐标指针动作族（canvas 类 UI：echarts/Univer/地图/白板，无子 DOM，靠真实坐标命中）:
//   - "clickat #canvas 92% 8%" — 真实鼠标左键单击（点选柱/扇区/数据点）
//   - "dblclickat css=#cell 30% 40%" — 真实鼠标双击（Univer 单元格进入编辑 / 缩放复位）
//   - "rclickat css=#chart 30% 40%" — 真实鼠标右键（上下文菜单）
//   - "hoverat css=#chart 50% 50%" — 真实鼠标悬停（echarts tooltip/十字线）
//   - "hoverat 414,314" — 视口 CSS 像素绝对坐标悬停（无需 selector）
//   - "dragat css=#chart 20% 50% 80% 50%" — 真实鼠标拖拽（echarts dataZoom/brush 框选）
//   - "wheelat css=#chart 50% 50% -240" — 真实滚轮（echarts dataZoom:'inside'、Univer/地图缩放）
//   - "wheelat 1200,800 down 6" — 在视口绝对坐标向下滚 6 格（无需 selector）
//   - "tap button:'接管'" | "tapat #browser-liveview 92% 8%" — 真实触控点击
//   - "swipeat css=#chart 80% 50% 20% 50%" — 真实触控滑动（移动端 canvas 平移）
//     注意: #x 默认按 data-testid 解析；echarts/Univer 容器多为 id/class，需用 css= 前缀
//   - "fill #input 'text'" — 清空后输入
//   - "type textbox:'名称' 'hello'"
//   - "press Enter" | "press Ctrl+A" | "press #btn Ctrl+K"
//   - "hover button:'名称'"
//   - "scroll down" | "scroll up 3" | "scroll @r4 down 3"
//   - "select combobox:'状态' 'opt2'"
//   - "back" | "forward"
//   - "focus #selector"
//   - "scrollinto #selector" | "scrollto @r4"
//   - "check #selector" | "uncheck #selector"
func ParseAction(action string) (*ParsedAction, error) {
	action = strings.TrimSpace(action)
	if action == "" {
		return nil, fmt.Errorf("%w: empty action", ErrActFailed)
	}

	parts := splitActionParts(action)
	if len(parts) == 0 {
		return nil, fmt.Errorf("%w: empty action", ErrActFailed)
	}

	op := strings.ToLower(parts[0])

	// 绝对视口坐标真实点击 — 完全绕过 a11y/locator 模型，直接对视口像素发真实鼠标事件。
	// 服务自定义 Vue/canvas UI: 大量可见控件（自定义 button、原生 select、div 点击区）
	// 不进 a11y 树，css= 选择器引擎解析超时，clickat 又需已被 a11y 找到的 ref 作原点。
	// 两种等价语法:
	//   - "tapxy <xfrac> <yfrac>"            (无 ref，对视口比例点击)
	//   - "clickat viewport <xfrac> <yfrac>" (与 clickat 同形，parts[1]=="viewport" 触发)
	if op == "tapxy" || (op == "clickat" && len(parts) >= 2 && strings.ToLower(parts[1]) == "viewport") {
		coordParts := parts[1:] // tapxy: x y
		if op == "clickat" {
			coordParts = parts[2:] // clickat viewport x y
		}
		if len(coordParts) != 2 {
			return nil, fmt.Errorf("%w: %s requires exactly xfrac and yfrac (0..1 or 0%%..100%%)", ErrActFailed, op)
		}
		x, err := parseNormalizedCoordinate(coordParts[0])
		if err != nil {
			return nil, err
		}
		y, err := parseNormalizedCoordinate(coordParts[1])
		if err != nil {
			return nil, err
		}
		return &ParsedAction{Op: "tapxy", CoordX: x, CoordY: y}, nil
	}

	// 向当前聚焦元素插入文本 — 完全绕过 a11y/locator 模型，不解析任何 ref/selector。
	// 服务自定义 Vue/canvas input: 配合 tapxy 先聚焦不进 a11y 树的输入框，再用本动作
	//   "typetext <text>" 直接对聚焦元素发 CDP Input.insertText（一次性插入，不走键盘逐字）。
	// <text> 取整个剩余字符串（允许含空格/点/特殊字符），不做引号剥离。
	if op == "typetext" {
		text := strings.TrimSpace(strings.TrimPrefix(action, parts[0]))
		if text == "" {
			return nil, fmt.Errorf("%w: typetext requires text argument", ErrActFailed)
		}
		return &ParsedAction{Op: "typetext", Value: text}, nil
	}

	// Vision-native absolute click: coordinates are viewport CSS pixels, not
	// normalized fractions. Only a pair of numeric values claims this syntax so
	// CSS selector lists such as "button.primary,a.cta" remain CSS selectors.
	if op == "click" {
		rest := strings.TrimSpace(strings.TrimPrefix(action, parts[0]))
		if x, y, matched, err := parseViewportCoordinatePair(rest); matched {
			if err != nil {
				return nil, err
			}
			return &ParsedAction{Op: "clickxy", CoordX: x, CoordY: y}, nil
		}
	}

	// Vision-native absolute hover/wheel: the first argument is the same
	// screenshot-derived CSS-pixel x,y pair accepted by click. These forms do
	// not need an artificial selector merely to provide a coordinate origin.
	if op == "hoverat" && len(parts) >= 2 {
		if x, y, matched, err := parseViewportCoordinatePair(parts[1]); matched {
			if err != nil {
				return nil, err
			}
			if len(parts) != 2 {
				return nil, fmt.Errorf("%w: hoverat x,y accepts no additional arguments", ErrActFailed)
			}
			return &ParsedAction{Op: "hoverat", CoordX: x, CoordY: y}, nil
		}
	}
	if op == "wheelat" && len(parts) >= 2 {
		if x, y, matched, err := parseViewportCoordinatePair(parts[1]); matched {
			if err != nil {
				return nil, err
			}
			if len(parts) < 3 {
				return nil, fmt.Errorf("%w: wheelat x,y requires down|up [steps] or deltaY [deltaX]", ErrActFailed)
			}
			direction := strings.ToLower(parts[2])
			if direction == "down" || direction == "up" {
				steps, err := parseScrollSteps(parts, 3)
				if err != nil {
					return nil, err
				}
				if steps == 0 {
					steps = 1
				}
				deltaY := humanWheelStepCSSPixels
				if direction == "up" {
					deltaY = -deltaY
				}
				return &ParsedAction{
					Op: "wheelat", CoordX: x, CoordY: y,
					DeltaY: deltaY, Direction: direction, Steps: steps,
				}, nil
			}
			if len(parts) > 4 {
				return nil, fmt.Errorf("%w: wheelat x,y delta form accepts deltaY and optional deltaX", ErrActFailed)
			}
			deltaY, err := parseDelta(parts[2])
			if err != nil {
				return nil, err
			}
			deltaX := 0.0
			if len(parts) == 4 {
				deltaX, err = parseDelta(parts[3])
				if err != nil {
					return nil, err
				}
			}
			return &ParsedAction{Op: "wheelat", CoordX: x, CoordY: y, DeltaX: deltaX, DeltaY: deltaY, Steps: 1}, nil
		}
	}

	switch op {
	case "click", "tap", "hover", "focus", "scrollinto", "scrollto", "check", "uncheck":
		if len(parts) != 2 {
			return nil, fmt.Errorf("%w: %s requires exactly one selector argument", ErrActFailed, op)
		}
		return &ParsedAction{Op: op, Ref: parts[1]}, nil

	// 坐标指针动作族 — 单点 (ref x y): 鼠标单/双/右击、悬停、触控点击。
	// 按"形状/参数个数"而非逐个动词分支，新增同形动作零成本。
	case "clickat", "dblclickat", "rclickat", "hoverat", "tapat":
		if len(parts) != 4 {
			return nil, fmt.Errorf("%w: %s requires exactly selector, x, and y", ErrActFailed, op)
		}
		x, err := parseNormalizedCoordinate(parts[2])
		if err != nil {
			return nil, err
		}
		y, err := parseNormalizedCoordinate(parts[3])
		if err != nil {
			return nil, err
		}
		return &ParsedAction{Op: op, Ref: parts[1], CoordX: x, CoordY: y}, nil

	// 坐标指针动作族 — 两点 (ref x1 y1 x2 y2): 鼠标拖拽、触控滑动。
	case "dragat", "swipeat":
		if len(parts) != 6 {
			return nil, fmt.Errorf("%w: %s requires exactly selector, x1, y1, x2, and y2", ErrActFailed, op)
		}
		x1, err := parseNormalizedCoordinate(parts[2])
		if err != nil {
			return nil, err
		}
		y1, err := parseNormalizedCoordinate(parts[3])
		if err != nil {
			return nil, err
		}
		x2, err := parseNormalizedCoordinate(parts[4])
		if err != nil {
			return nil, err
		}
		y2, err := parseNormalizedCoordinate(parts[5])
		if err != nil {
			return nil, err
		}
		return &ParsedAction{Op: op, Ref: parts[1], CoordX: x1, CoordY: y1, CoordX2: x2, CoordY2: y2}, nil

	// 滚轮 (ref x y deltaY [deltaX]): deltaY 必填，deltaX 可选默认 0。
	case "wheelat":
		if len(parts) != 5 && len(parts) != 6 {
			return nil, fmt.Errorf("%w: wheelat requires exactly selector, x, y, deltaY, and optional deltaX", ErrActFailed)
		}
		x, err := parseNormalizedCoordinate(parts[2])
		if err != nil {
			return nil, err
		}
		y, err := parseNormalizedCoordinate(parts[3])
		if err != nil {
			return nil, err
		}
		dy, err := parseDelta(parts[4])
		if err != nil {
			return nil, err
		}
		dx := 0.0
		if len(parts) == 6 {
			dx, err = parseDelta(parts[5])
			if err != nil {
				return nil, err
			}
		}
		return &ParsedAction{Op: "wheelat", Ref: parts[1], CoordX: x, CoordY: y, DeltaX: dx, DeltaY: dy}, nil

	// 页面缩放态（browser chrome 仿真的视口状态空间维度之一）：
	//   "zoom 2" / "zoom 1.5" — 模拟捏合/聚焦放大后的视觉视口（CDP Emulation.setPageScaleFactor）
	//   "zoom reset"          — 回到 1.0
	// Safari 语义：页面缩放不改变布局视口，chrome 层（底栏）恒定不动。
	case "zoom":
		if len(parts) != 2 {
			return nil, fmt.Errorf("%w: zoom requires exactly factor (1..5) or 'reset'", ErrActFailed)
		}
		return &ParsedAction{Op: "zoom", Value: strings.ToLower(parts[1])}, nil

	// 软键盘态（REQ-BC-12）：显式切换；click/tap/fill 后另有焦点自动同步。
	//   "keyboard show" / "keyboard hide"
	case "keyboard":
		if len(parts) != 2 {
			return nil, fmt.Errorf("%w: keyboard requires exactly 'show' or 'hide'", ErrActFailed)
		}
		value := strings.ToLower(parts[1])
		if value != "show" && value != "hide" {
			return nil, fmt.Errorf("%w: keyboard expects 'show' or 'hide', got %q", ErrActFailed, parts[1])
		}
		return &ParsedAction{Op: "keyboard", Value: value}, nil

	case "fill":
		if len(parts) < 3 {
			return nil, fmt.Errorf("%w: fill requires selector and value", ErrActFailed)
		}
		value, quoted := extractQuotedValue(action, parts[1])
		if !quoted {
			if err := checkWithKeywordMisuse("fill", parts); err != nil {
				return nil, err
			}
			value = strings.Join(parts[2:], " ")
		}
		return &ParsedAction{Op: "fill", Ref: parts[1], Value: value}, nil

	case "type":
		if len(parts) < 3 {
			return nil, fmt.Errorf("%w: type requires selector and value", ErrActFailed)
		}
		// 提取引号内的值（支持多词）
		value, quoted := extractQuotedValue(action, parts[1])
		if !quoted {
			if err := checkWithKeywordMisuse("type", parts); err != nil {
				return nil, err
			}
			value = strings.Join(parts[2:], " ")
		}
		return &ParsedAction{Op: "type", Ref: parts[1], Value: value}, nil

	case "fillsecret":
		// fillsecret <selector> '<value>' — 对 password/敏感字段的显式 opt-in 安全填充。
		// 默认 fill/type 仍拒绝 password [IR-03]；只有调用方明确用 fillsecret 才经安全 CDP
		// Input.insertText 通道注入（可信输入事件，穿透 Vue/React 受控输入）。值不回显、不入结果。
		if len(parts) < 3 {
			return nil, fmt.Errorf("%w: fillsecret requires selector and value", ErrActFailed)
		}
		value, quoted := extractQuotedValue(action, parts[1])
		if !quoted {
			if err := checkWithKeywordMisuse("fillsecret", parts); err != nil {
				return nil, err
			}
			value = strings.Join(parts[2:], " ")
		}
		return &ParsedAction{Op: "fillsecret", Ref: parts[1], Value: value}, nil

	case "press":
		// press <key>  OR  press <selector> <key>
		if len(parts) != 2 && len(parts) != 3 {
			return nil, fmt.Errorf("%w: press requires exactly key or selector + key", ErrActFailed)
		}
		key := parts[len(parts)-1]
		if err := validatePressKeySyntax(key); err != nil {
			return nil, err
		}
		if len(parts) == 2 {
			// press <key> — page-level
			return &ParsedAction{Op: "press", Ref: "", Value: key}, nil
		}
		// press <selector> <key>
		return &ParsedAction{Op: "press", Ref: parts[1], Value: key}, nil

	case "scroll":
		if len(parts) < 2 {
			return nil, fmt.Errorf("%w: scroll requires direction or selector + direction", ErrActFailed)
		}
		if dir := strings.ToLower(parts[1]); dir == "down" || dir == "up" {
			steps, err := parseScrollSteps(parts, 2)
			if err != nil {
				return nil, err
			}
			// Keep Ref=direction for compatibility with the original parser
			// contract; Direction is the unambiguous execution field.
			return &ParsedAction{Op: "scroll", Ref: dir, Direction: dir, Steps: steps}, nil
		}
		if len(parts) < 3 {
			return nil, fmt.Errorf("%w: scroll %q requires up|down", ErrActFailed, parts[1])
		}
		dir := strings.ToLower(parts[2])
		if dir != "down" && dir != "up" {
			return nil, fmt.Errorf("%w: scroll direction must be up or down, got %q", ErrActFailed, parts[2])
		}
		steps, err := parseScrollSteps(parts, 3)
		if err != nil {
			return nil, err
		}
		return &ParsedAction{Op: "scroll", ScrollTarget: parts[1], Direction: dir, Steps: steps}, nil

	case "select":
		if len(parts) < 3 {
			return nil, fmt.Errorf("%w: select requires selector and value", ErrActFailed)
		}
		value, quoted := extractQuotedValue(action, parts[1])
		if !quoted {
			value = strings.Join(parts[2:], " ")
		}
		return &ParsedAction{Op: "select", Ref: parts[1], Value: value}, nil

	case "back":
		if len(parts) != 1 {
			return nil, fmt.Errorf("%w: back accepts no arguments", ErrActFailed)
		}
		return &ParsedAction{Op: "back"}, nil

	case "forward":
		if len(parts) != 1 {
			return nil, fmt.Errorf("%w: forward accepts no arguments", ErrActFailed)
		}
		return &ParsedAction{Op: "forward"}, nil

	default:
		return nil, fmt.Errorf("%w: unknown operation %q", ErrActFailed, op)
	}
}

func parseViewportCoordinatePair(raw string) (x, y float64, matched bool, err error) {
	left, right, ok := strings.Cut(strings.TrimSpace(raw), ",")
	if !ok || strings.Contains(right, ",") {
		return 0, 0, false, nil
	}
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	x, xErr := strconv.ParseFloat(left, 64)
	y, yErr := strconv.ParseFloat(right, 64)
	if xErr != nil && yErr != nil {
		return 0, 0, false, nil
	}
	if xErr != nil || yErr != nil || math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) {
		return 0, 0, true, fmt.Errorf("%w: viewport coordinates must be finite CSS pixels in x,y form, got %q", ErrActFailed, raw)
	}
	return x, y, true, nil
}

func parseScrollSteps(parts []string, index int) (int, error) {
	if len(parts) <= index {
		return 0, nil
	}
	if len(parts) > index+1 {
		return 0, fmt.Errorf("%w: scroll accepts at most one step count", ErrActFailed)
	}
	steps, err := strconv.Atoi(parts[index])
	if err != nil || steps <= 0 || steps > 100 {
		return 0, fmt.Errorf("%w: scroll steps must be an integer in 1..100, got %q", ErrActFailed, parts[index])
	}
	return steps, nil
}

func validatePressKeySyntax(key string) error {
	base := key
	if strings.Contains(key, "+") {
		_, parsedBase, ok := parseKeyCombo(key)
		if !ok {
			return fmt.Errorf("%w: press has an invalid modifier chord %q", ErrActFailed, key)
		}
		base = parsedBase
	}
	canonical := canonicalKeyName(base)
	if utf8.RuneCountInString(canonical) == 1 {
		return nil
	}
	switch canonical {
	case "Enter", "Tab", "Space", "Escape", "Backspace", "Delete",
		"ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight",
		"Home", "End", "PageUp", "PageDown":
		return nil
	}
	if _, ok := keyVirtualCodeMap[strings.ToUpper(canonical)]; ok && strings.HasPrefix(strings.ToUpper(canonical), "F") {
		return nil
	}
	return fmt.Errorf("%w: press key %q is not a supported single key or modifier chord", ErrActFailed, key)
}

// parseDelta 解析滚轮位移量（带符号像素，如 -240 / 120），不做 0..1 归一。
func parseDelta(raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid wheel delta %q", ErrActFailed, raw)
	}
	return v, nil
}

func parseNormalizedCoordinate(raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("%w: empty coordinate", ErrActFailed)
	}

	if strings.HasSuffix(raw, "%") {
		v, err := strconv.ParseFloat(strings.TrimSuffix(raw, "%"), 64)
		if err != nil {
			return 0, fmt.Errorf("%w: invalid percentage coordinate %q", ErrActFailed, raw)
		}
		if v < 0 || v > 100 {
			return 0, fmt.Errorf("%w: percentage coordinate %q out of range 0%%..100%%", ErrActFailed, raw)
		}
		return v / 100.0, nil
	}

	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid coordinate %q", ErrActFailed, raw)
	}
	if v < 0 || v > 1 {
		return 0, fmt.Errorf("%w: coordinate %q out of range 0..1 (use 92%% or 0.92)", ErrActFailed, raw)
	}
	return v, nil
}

// splitActionParts 分割 action 字符串，保留引号内内容（支持 role:'name' 不被空格分割）。
// 例: "click button:'open dialog'" → ["click", "button:'open dialog'"]
//
//	"type textbox:'名称' 'hello'" → ["type", "textbox:'名称'", "'hello'"]
func splitActionParts(action string) []string {
	var parts []string
	var cur strings.Builder
	inSingle := false
	inDouble := false

	for i := 0; i < len(action); i++ {
		c := action[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
			cur.WriteByte(c)
		case c == '"' && !inSingle:
			inDouble = !inDouble
			cur.WriteByte(c)
		case c == ' ' && !inSingle && !inDouble:
			if cur.Len() > 0 {
				parts = append(parts, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		parts = append(parts, cur.String())
	}
	return parts
}

// extractQuotedValue 从操作字符串中提取引号内的值。
// 第二个返回值表示“是否真的提供了引号包裹的参数”，
// 用于区分 fill #x ” 与未加引号的 fallback 解析。
func extractQuotedValue(action string, afterRef string) (string, bool) {
	// 找到 ref 之后的内容
	idx := strings.Index(action, afterRef)
	if idx < 0 {
		return "", false
	}
	rest := strings.TrimSpace(action[idx+len(afterRef):])

	// 处理单引号或双引号
	for _, quote := range []byte{'\'', '"'} {
		if len(rest) > 0 && rest[0] == quote {
			end := strings.LastIndexByte(rest, quote)
			if end > 0 {
				return rest[1:end], true
			}
		}
	}
	return "", false
}

// checkWithKeywordMisuse 拦截 `fill <loc> with 'v'` 这类不存在的 `with` 语法 [BUG-FILL-WITH]。
//
// 真因: DSL 合法支持不带引号的多词值（fill @r1 hello world → "hello world"），于是
// `fill @r1 with 'hello'` 会被静默当成字面值 "with 'hello'" 填进输入框 —— act 返回
// success:true、退出码 0，agent 完全无从察觉。实测已复现（输入框里真是 with 'hello'）。
//
// 判据取"极不可能是真实意图"的窄形状: 值以裸词 with 开头 + 其后是完整引号串。
// 逃生舱: 真要填这个字面量就整体加引号 —— fill @r1 "with 'hello'" 走 quoted 路径，不受影响。
func checkWithKeywordMisuse(op string, parts []string) error {
	if len(parts) < 4 || !strings.EqualFold(parts[2], "with") {
		return nil
	}
	tail := strings.TrimSpace(strings.Join(parts[3:], " "))
	if len(tail) < 2 {
		return nil
	}
	q := tail[0]
	if (q != '\'' && q != '"') || tail[len(tail)-1] != q {
		return nil
	}
	inner := tail[1 : len(tail)-1]
	literal := strings.Join(parts[2:], " ") // 误用时会被填进去的整串, 如: with 'hello'
	return fmt.Errorf("%w: %s 不支持 `with` 语法 (整串 %q 会被当成字面值填进去)。正确写法: %s %s %s%s%s；"+
		"若确实要填字面量 %q，请整体加引号: %s %s %q",
		ErrActFailed, op, literal,
		op, parts[1], string(q), inner, string(q),
		literal, op, parts[1], literal)
}

// Execute 执行操作 [TC-09-U-05~08, TC-09-U-27, TC-09-U-28]。
// observe=false 时返回 nil Snapshot [TC-09-U-28]。
// 语义选择器（TH-0405-p7c）: #testid / role:'name' / role
// 位置编码 e{N} 被拒绝并返回引导性错误。
// sessionMode=true 时允许 @rN ref。
func (e *actionEngine) Execute(ctx context.Context, action string, observe bool) (*Snapshot, error) {
	return e.ExecuteWithSessionMode(ctx, action, observe, false)
}

// ExecuteWithSessionMode 执行操作，支持 session 模式（允许 @rN ref）。
func (e *actionEngine) ExecuteWithSessionMode(ctx context.Context, action string, observe bool, sessionMode bool) (*Snapshot, error) {
	return e.ExecuteWithInteractionMode(ctx, action, observe, sessionMode, InteractionModeVisual)
}

const actionExecutionTimeout = 15 * time.Second

type actionExecutionResult struct {
	snap *Snapshot
	err  error
}

type actionExecutionTimeoutError struct {
	Action  string
	Timeout time.Duration
}

func (e *actionExecutionTimeoutError) Error() string {
	return fmt.Sprintf("%v: action %q timed out after %s (internal watchdog)", ErrActFailed, e.Action, e.Timeout)
}

func (e *actionExecutionTimeoutError) Unwrap() []error {
	return []error{ErrActFailed, context.DeadlineExceeded}
}

// runActionWithTimeout is the innermost action watchdog. The worker receives a
// cancellable child context, but the caller does not depend on a CDP transport
// honoring cancellation: even a wedged worker cannot keep Act blocked forever.
func runActionWithTimeout(
	parent context.Context,
	timeout time.Duration,
	action string,
	run func(context.Context) (*Snapshot, error),
) (*Snapshot, error) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		timeout = actionExecutionTimeout
	}
	if deadline, ok := parent.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, &actionExecutionTimeoutError{Action: action, Timeout: 0}
		}
		if remaining < timeout {
			timeout = remaining
		}
	}

	actionCtx, cancelAction := context.WithCancel(parent)
	done := make(chan actionExecutionResult, 1)
	go func() {
		snap, err := run(actionCtx)
		done <- actionExecutionResult{snap: snap, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	defer cancelAction()
	select {
	case result := <-done:
		return result.snap, result.err
	case <-parent.Done():
		if parent.Err() == context.DeadlineExceeded {
			return nil, &actionExecutionTimeoutError{Action: action, Timeout: timeout}
		}
		return nil, parent.Err()
	case <-timer.C:
		return nil, &actionExecutionTimeoutError{Action: action, Timeout: timeout}
	}
}

// ExecuteWithInteractionMode executes one action with a step-local fidelity
// choice. It never mutates the scenario/global posture.
func (e *actionEngine) ExecuteWithInteractionMode(ctx context.Context, action string, observe bool, sessionMode bool, mode InteractionMode) (*Snapshot, error) {
	return runActionWithTimeout(ctx, actionExecutionTimeout, action, func(actionCtx context.Context) (*Snapshot, error) {
		return e.executeWithInteractionMode(actionCtx, action, observe, sessionMode, mode)
	})
}

func (e *actionEngine) executeWithInteractionMode(ctx context.Context, action string, observe bool, sessionMode bool, mode InteractionMode) (*Snapshot, error) {
	e.beginActionFidelity()
	// — 校验期 —— 以下每一步都跑在任何输入派发之前。失败 = 页面绝无可能被改动，
	// 因此标记 pre-dispatch，调用方据此保留上一份 observation 的 @rN 句柄
	// [BUG-ACT-FAILURE-REVOKES-OBSERVATION]。
	normalizedMode, err := NormalizeInteractionMode(string(mode))
	if err != nil {
		return nil, e.failedBeforeDispatch(fmt.Errorf("%w: %v", ErrActFailed, err))
	}
	elementMode := normalizedMode == InteractionModeElement
	if elementMode && e.interactionPolicy.StrictHuman() {
		return nil, e.failedBeforeDispatch(fmt.Errorf("%w: mode element is unavailable in strict-human scenario", ErrActFailed))
	}
	seeToClick := e.seeToClick && !elementMode
	strictHuman := e.interactionPolicy.StrictHuman()

	parsed, err := ParseAction(action)
	if err != nil {
		return nil, e.failedBeforeDispatch(err)
	}

	// page-level 操作不需要选择器
	// tapxy 是绝对视口坐标点击，刻意不解析 ref（绕过 a11y/locator）。
	// typetext 向当前聚焦元素插入文本，同样刻意不解析 ref。
	noSelectorOps := map[string]bool{"scroll": true, "back": true, "forward": true, "clickxy": true, "tapxy": true, "typetext": true, "zoom": true, "keyboard": true}

	var resolvedRef string
	if !noSelectorOps[parsed.Op] && parsed.Ref != "" {
		ref, err := e.resolveSemanticSelectorForMode(parsed.Ref, sessionMode, elementMode)
		if err != nil {
			return nil, e.failedBeforeDispatch(err)
		}
		resolvedRef = ref
	} else {
		resolvedRef = parsed.Ref
	}
	var resolvedScrollTarget string
	if parsed.Op == "scroll" && parsed.ScrollTarget != "" {
		resolvedScrollTarget, err = e.resolveSemanticSelectorForMode(parsed.ScrollTarget, sessionMode, elementMode)
		if err != nil {
			return nil, e.failedBeforeDispatch(err)
		}
	}

	// 直接使用调用方 context（不派生子 context）。
	// 原因: chromedp 在 NewRemoteAllocator 模式下，context.WithTimeout 派生的 context
	// 会导致 CDP 响应路由异常（Run 挂起直到超时）。调用方 context 已包含合理超时。
	// 诊断证据: EvalJS(browserCtx) 370µs OK, Run(WithTimeout(browserCtx, 5s)) 5s 超时。
	switch parsed.Op {
	case "click":
		if err := e.executeClick(ctx, resolvedRef, seeToClick, elementMode); err != nil {
			return nil, err
		}
	case "clickxy":
		if err := e.executeClickXY(ctx, parsed.CoordX, parsed.CoordY, seeToClick); err != nil {
			return nil, err
		}
	case "clickat":
		if err := e.executeClickAt(ctx, resolvedRef, parsed.CoordX, parsed.CoordY); err != nil {
			return nil, err
		}
	case "tapxy":
		if err := e.executeTapXY(ctx, parsed.CoordX, parsed.CoordY); err != nil {
			return nil, err
		}
	case "typetext":
		if err := e.executeTypeText(ctx, parsed.Value); err != nil {
			return nil, err
		}
	case "zoom":
		if err := e.executeZoom(ctx, parsed.Value); err != nil {
			return nil, err
		}
	case "keyboard":
		if err := e.executeKeyboard(ctx, parsed.Value); err != nil {
			return nil, err
		}
	case "dblclickat":
		if err := e.executeDoubleClickAt(ctx, resolvedRef, parsed.CoordX, parsed.CoordY); err != nil {
			return nil, err
		}
	case "rclickat":
		if err := e.executeRightClickAt(ctx, resolvedRef, parsed.CoordX, parsed.CoordY); err != nil {
			return nil, err
		}
	case "hoverat":
		if err := e.executeHoverAt(ctx, resolvedRef, parsed.CoordX, parsed.CoordY); err != nil {
			return nil, err
		}
	case "dragat":
		if err := e.executeDragAt(ctx, resolvedRef, parsed.CoordX, parsed.CoordY, parsed.CoordX2, parsed.CoordY2); err != nil {
			return nil, err
		}
	case "wheelat":
		if err := e.executeWheelAt(ctx, resolvedRef, parsed.CoordX, parsed.CoordY, parsed.DeltaX, parsed.DeltaY, parsed.Steps); err != nil {
			return nil, err
		}
	case "swipeat":
		if err := e.executeSwipeAt(ctx, resolvedRef, parsed.CoordX, parsed.CoordY, parsed.CoordX2, parsed.CoordY2); err != nil {
			return nil, err
		}
	case "tap":
		if err := e.executeTap(ctx, resolvedRef); err != nil {
			return nil, err
		}
	case "tapat":
		if err := e.executeTapAt(ctx, resolvedRef, parsed.CoordX, parsed.CoordY); err != nil {
			return nil, err
		}
	case "fill":
		if strictHuman {
			err = e.executeStrictHumanText(ctx, resolvedRef, parsed.Value, true, false)
		} else {
			err = e.executeFill(ctx, resolvedRef, parsed.Value)
		}
		if err != nil {
			return nil, err
		}
	case "fillsecret":
		if strictHuman {
			err = e.executeStrictHumanText(ctx, resolvedRef, parsed.Value, true, true)
		} else {
			err = e.executeFillSecret(ctx, resolvedRef, parsed.Value)
		}
		if err != nil {
			return nil, err
		}
	case "type":
		if strictHuman {
			err = e.executeStrictHumanText(ctx, resolvedRef, parsed.Value, false, false)
		} else {
			err = e.executeType(ctx, resolvedRef, parsed.Value)
		}
		if err != nil {
			return nil, err
		}
	case "press":
		if strictHuman {
			err = e.executeStrictHumanPress(ctx, resolvedRef, parsed.Value)
		} else {
			err = e.executePress(ctx, resolvedRef, parsed.Value)
		}
		if err != nil {
			return nil, err
		}
	case "hover":
		if err := e.executeHover(ctx, resolvedRef); err != nil {
			return nil, err
		}
	case "scroll":
		if err := e.executeScroll(ctx, resolvedScrollTarget, parsed.Direction, parsed.Steps, seeToClick); err != nil {
			return nil, err
		}
	case "select":
		if err := e.executeSelect(ctx, resolvedRef, parsed.Value); err != nil {
			return nil, err
		}
		e.updateActionFidelity(func(report *ActionFidelityReport) {
			report.Synthetic = true
			report.SyntheticNote = "native <select> uses DOM state plus synthetic input/change events; this is not evidence that a human can operate the picker"
		})
	case "back":
		if err := e.executeBack(ctx); err != nil {
			return nil, err
		}
	case "forward":
		if err := e.executeForward(ctx); err != nil {
			return nil, err
		}
	case "focus":
		if strictHuman {
			err = e.executeStrictHumanFocus(ctx, resolvedRef)
		} else {
			err = e.executeFocusSelector(ctx, resolvedRef)
		}
		if err != nil {
			return nil, err
		}
	case "scrollinto":
		if err := e.executeScrollIntoView(ctx, resolvedRef); err != nil {
			return nil, err
		}
	case "scrollto":
		if err := e.executeScrollIntoView(ctx, resolvedRef); err != nil {
			return nil, err
		}
	case "check":
		if err := e.executeCheck(ctx, resolvedRef, seeToClick, elementMode); err != nil {
			return nil, err
		}
	case "uncheck":
		if err := e.executeUncheck(ctx, resolvedRef, seeToClick, elementMode); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: unsupported op %q", ErrActFailed, parsed.Op)
	}

	// 点击/输入后等待 DOM 稳定（MutationObserver idle 检测）
	// 灵感来源: Stagehand _waitForSettledDom()
	// 注意: 直接使用 ctx，不创建派生 context（chromedp remote allocator 兼容性）
	// back/forward 不需要 DOM settle — 导航本身有 readyState 等待机制
	// clickat/tapat 是坐标式 Human pointer 输入，主要服务 LiveView/takeover。
	// Browser Portal 会持续更新 frame/blob URL，宿主 DOM 并不会像普通页面点击那样稳定；
	// 坐标输入的完成条件应由 input-ack/后续截图证明，而不是在宿主页等待 DOM settle。
	domSettleOps := map[string]bool{"click": true, "tap": true, "type": true, "fill": true, "fillsecret": true, "select": true, "check": true, "uncheck": true}
	if domSettleOps[parsed.Op] {
		_ = waitForDOMSettle(ctx, 500, 5000)
	}

	// 焦点自动同步（REQ-BC-12 真机语义）：可能改变焦点的 op 之后，
	// activeElement 可编辑 ⇒ 软键盘弹起，否则收起。settle 之后判（焦点已定）。
	focusSyncOps := map[string]bool{
		"click": true, "tap": true, "clickxy": true, "clickat": true, "tapat": true, "tapxy": true,
		"dblclickat": true, "fill": true, "fillsecret": true, "type": true,
		"typetext": true, "press": true, "focus": true,
	}
	if focusSyncOps[parsed.Op] {
		e.autoSyncKeyboard(ctx)
	}

	// observe=false 时不返回 Snapshot [TC-09-U-28]
	if !observe {
		return nil, nil
	}

	if sessionMode {
		return e.snapEngine.GetSnapshotWithSessionMode(ctx, 0)
	}
	return e.snapEngine.GetSnapshot(ctx)
}

// waitForDOMSettle 等待 DOM 稳定 — 通过 MutationObserver 检测连续 idleMs 毫秒无变化。
// 参考: Stagehand (browserbase) 的 _waitForSettledDom() 方案。
// 使用纯 JS Promise + MutationObserver，不调用 CDP DOM API。
func waitForDOMSettle(ctx context.Context, idleMs, timeoutMs int) error {
	js := fmt.Sprintf(`new Promise((resolve) => {
		let timer = null, settled = false;
		const IDLE = %d, MAX = %d;
		const maxT = setTimeout(() => {
			if (!settled) { settled = true; obs.disconnect(); resolve('timeout'); }
		}, MAX);
		const reset = () => {
			if (timer) clearTimeout(timer);
			timer = setTimeout(() => {
				if (!settled) { settled = true; obs.disconnect(); clearTimeout(maxT); resolve('idle'); }
			}, IDLE);
		};
		const obs = new MutationObserver(() => reset());
		obs.observe(document.body, { childList: true, subtree: true, attributes: true, characterData: true });
		reset();
	})`, idleMs, timeoutMs)

	var result string
	return chromedp.Run(ctx, chromedp.Evaluate(js, &result,
		chromedp.EvalAsValue,
		evalAwaitPromise,
	))
}

// evalAwaitPromise 是 chromedp.EvaluateOption，让 Evaluate 等待 Promise resolve。
func evalAwaitPromise(p *runtime.EvaluateParams) *runtime.EvaluateParams {
	return p.WithAwaitPromise(true)
}

// resolveSemanticSelector 将语义选择器解析为内部可执行的 ref 或 CSS 选择器字符串。
// 不支持 session ref（@rN），请使用 resolveSemanticSelectorWithSession。
func (e *actionEngine) resolveSemanticSelector(selector string) (string, error) {
	return e.resolveSemanticSelectorWithSession(selector, false)
}

// resolveSemanticSelectorWithSession 将语义选择器解析为内部可执行的 ref 或 CSS 选择器字符串。
// sessionMode=true 时允许 @rN ref。
func (e *actionEngine) resolveSemanticSelectorWithSession(selector string, sessionMode bool) (string, error) {
	return e.resolveSemanticSelectorForMode(selector, sessionMode, false)
}

func (e *actionEngine) resolveSemanticSelectorForMode(selector string, sessionMode, elementMode bool) (string, error) {
	sel, err := ParseSelector(selector)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrActFailed, err)
	}

	switch sel.SType {
	case SelectorSessionRef:
		if !sessionMode {
			return "", ErrInvalidRefInOneShot
		}
		// @rN → look up in refTable using @rN key
		refKey := fmt.Sprintf("@r%d", sel.SessionRef)
		if _, ok := e.snapEngine.LookupRef(refKey); !ok {
			return "", fmt.Errorf("%w: %s not found in current observation (run observe --id <id> first)", ErrRefNotFound, refKey)
		}
		// Return as internal ref string so executeClick etc. can use it
		return refKey, nil
	}

	if elementMode {
		return e.resolveElementModeSelector(sel)
	}

	switch sel.SType {
	case SelectorTestID:
		meta, ok := e.snapEngine.LookupByTestID(sel.TestID)
		if ok && (meta.BackendNodeID != 0 || e.seeToClick) {
			return meta.Ref, nil
		}
		if e.seeToClick {
			return "", fmt.Errorf("%w: #%s is not an actionable element from the most recent visible observation — scroll and observe again", ErrRefNotFound, sel.TestID)
		}
		// 回退到 CSS 选择器
		return `[data-testid="` + sel.TestID + `"]`, nil

	case SelectorCanonical:
		return e.resolveCanonicalSelector(sel, sessionMode)

	case SelectorRoleName, SelectorRoleNameExact:
		allMeta := e.snapEngine.LookupAllByRoleName(sel.Role, sel.Name, sel.NameOp)
		if len(allMeta) == 0 {
			return "", e.locatorMissError(sel.Role, sel.Name)
		}
		if len(allMeta) > 1 {
			return "", buildAmbiguousError(sel.Raw, allMeta)
		}
		return allMeta[0].Ref, nil

	case SelectorRole:
		meta, ok := e.snapEngine.LookupByRoleName(sel.Role, "")
		if !ok {
			return "", e.locatorMissError(sel.Role, "")
		}
		return meta.Ref, nil

	default:
		// Raw CSS is an explicit element-mode capability. In visual scenarios it
		// has no provenance in the visible ref set and therefore cannot be used
		// for click, fill, focus, or any other selector action.
		if e.seeToClick {
			return "", fmt.Errorf("%w: CSS selector %q is not a capability from the most recent visible observation; use a semantic visible locator or mode: element", ErrRefNotFound, sel.Raw)
		}
		return sel.Raw, nil
	}
}

// locatorMissError 把"没匹配上"拆成三种如实说法：没有 observation / observation
// 里没这个 role / role 在但 name 不匹配。engine 只知道 observation，绝不代页面
// 断言"页面上没有 X" [BUG-FALSE-NEGATIVE-ROLE-CLAIM]。
func (e *actionEngine) locatorMissError(role, name string) error {
	setLabel := "observation ref 集"
	if e.seeToClick {
		setLabel = "可见集"
	}
	size := e.snapEngine.observationSize()
	if size == 0 {
		return errNoObservation(role, name, setLabel)
	}
	candidates := e.snapEngine.observationCandidatesByRole(role)
	if len(candidates) == 0 {
		return errRoleAbsentFromObservation(role, name, setLabel, size, e.snapEngine.observationRoleHistogram())
	}
	return errNameNoMatch(role, name, setLabel, candidates)
}

// elementModeLocatorMissError 是 mode:element 通道的对偶（候选来自全文档索引，
// 无 @ref，因此不给句柄，只给 role/name）。
func (e *actionEngine) elementModeLocatorMissError(role, name string) error {
	const setLabel = "element-mode 全文档索引"
	if len(e.snapEngine.elementCandidates) == 0 {
		return errNoObservation(role, name, setLabel)
	}
	candidates := e.snapEngine.elementCandidatesByRoleForDiagnostics(role)
	if len(candidates) == 0 {
		return errRoleAbsentFromObservation(role, name, setLabel,
			len(e.snapEngine.elementCandidates), e.snapEngine.elementCandidateHistogram())
	}
	return errNameNoMatch(role, name, setLabel, candidates)
}

const internalBackendNodePrefix = "__dw_backend_node__:"

func elementCandidateLocator(candidate elementCandidate) (string, error) {
	if candidate.BackendNodeID != 0 {
		return internalBackendNodePrefix + strconv.FormatInt(candidate.BackendNodeID, 10), nil
	}
	if candidate.TestID != "" {
		return `[data-testid="` + candidate.TestID + `"]`, nil
	}
	return "", fmt.Errorf("%w: element-mode candidate role=%q name=%q has no live locator", ErrRefNotFound, candidate.Role, candidate.Name)
}

func (e *actionEngine) resolveElementModeSelector(sel *ParsedSelector) (string, error) {
	switch sel.SType {
	case SelectorTestID:
		if candidate, ok := e.snapEngine.lookupElementCandidateByTestID(sel.TestID); ok {
			return elementCandidateLocator(candidate)
		}
		return `[data-testid="` + sel.TestID + `"]`, nil

	case SelectorCanonical:
		return e.resolveCanonicalElementSelector(sel)

	case SelectorRoleName, SelectorRoleNameExact:
		matches := e.snapEngine.lookupElementCandidatesByRoleName(sel.Role, sel.Name, sel.NameOp)
		if len(matches) == 0 {
			return "", e.elementModeLocatorMissError(sel.Role, sel.Name)
		}
		if len(matches) > 1 {
			return "", buildAmbiguousCandidateError(sel.Raw, matches)
		}
		return elementCandidateLocator(matches[0])

	case SelectorRole:
		matches := e.snapEngine.lookupElementCandidatesByRoleName(sel.Role, "", "*=")
		if len(matches) == 0 {
			return "", fmt.Errorf("%w: role=%s 的元素未找到", ErrRefNotFound, sel.Role)
		}
		return elementCandidateLocator(matches[0])

	default:
		return sel.Raw, nil
	}
}

func (e *actionEngine) resolveCanonicalElementSelector(sel *ParsedSelector) (string, error) {
	name, nameOp := "", "*="
	for _, filter := range sel.Filters {
		if filter.Field == "testid" {
			if candidate, ok := e.snapEngine.lookupElementCandidateByTestID(filter.Value); ok {
				return elementCandidateLocator(candidate)
			}
			return `[data-testid="` + filter.Value + `"]`, nil
		}
		if filter.Field == "name" {
			name, nameOp = filter.Value, filter.Op
		}
	}
	matches := e.snapEngine.lookupElementCandidatesByRoleName(sel.Role, name, nameOp)
	for _, filter := range sel.Filters {
		if filter.Field != "placeholder" {
			continue
		}
		filtered := matches[:0:0]
		for _, candidate := range matches {
			if matchOp(candidate.Placeholder, filter.Op, filter.Value) {
				filtered = append(filtered, candidate)
			}
		}
		matches = filtered
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("%w: canonical selector %q matched no elements", ErrRefNotFound, sel.Raw)
	}
	if sel.Nth > 0 {
		if sel.Nth > len(matches) {
			return "", fmt.Errorf("%w: nth=%d out of range (matched %d elements for %q)", ErrRefNotFound, sel.Nth, len(matches), sel.Raw)
		}
		return elementCandidateLocator(matches[sel.Nth-1])
	}
	if len(matches) > 1 {
		return "", buildAmbiguousCandidateError(sel.Raw, matches)
	}
	return elementCandidateLocator(matches[0])
}

func buildAmbiguousCandidateError(locator string, matches []elementCandidate) error {
	suggestions := make([]string, 0, len(matches))
	for i, candidate := range matches {
		if candidate.TestID != "" {
			suggestions = append(suggestions, fmt.Sprintf("  - #%s", candidate.TestID))
		} else {
			suggestions = append(suggestions, fmt.Sprintf("  - role=%s[name=%q][nth=%d]", candidate.Role, candidate.Name, i+1))
		}
	}
	msg := fmt.Sprintf("%s:\n  locator: %s\n  matches: %d\n  suggestions:\n%s",
		ErrAmbiguousLocator.Error(), locator, len(matches), strings.Join(suggestions, "\n"))
	return fmt.Errorf("%w: %s", ErrAmbiguousLocator, msg)
}

// resolveCanonicalSelector 解析 canonical DSL 选择器（role=TYPE[filters...]）。
func (e *actionEngine) resolveCanonicalSelector(sel *ParsedSelector, sessionMode bool) (string, error) {
	// If scoped (A >> B), first resolve parent scope (simplified: we just use child resolution for now)
	// Full scope support would require filtering refs by parent node, which needs DOM traversal.
	// For now, use nth filter if present for disambiguation.

	nameVal := ""
	nameOp := "*="
	for _, f := range sel.Filters {
		if f.Field == "name" {
			nameVal = f.Value
			nameOp = f.Op
		}
	}

	// Check testid filter
	for _, f := range sel.Filters {
		if f.Field == "testid" {
			meta, ok := e.snapEngine.LookupByTestID(f.Value)
			if ok && (meta.BackendNodeID != 0 || e.seeToClick) {
				return meta.Ref, nil
			}
			if e.seeToClick {
				return "", fmt.Errorf("%w: testid=%q is not an actionable element from the most recent visible observation — scroll and observe again", ErrRefNotFound, f.Value)
			}
			return `[data-testid="` + f.Value + `"]`, nil
		}
	}

	allMeta := e.snapEngine.LookupAllByRoleName(sel.Role, nameVal, nameOp)

	// Apply placeholder filter
	for _, f := range sel.Filters {
		if f.Field == "placeholder" {
			var filtered []*ElementRef
			for _, m := range allMeta {
				if matchOp(m.Placeholder, f.Op, f.Value) {
					filtered = append(filtered, m)
				}
			}
			allMeta = filtered
		}
	}

	if len(allMeta) == 0 {
		// 同 SelectorRoleName 分支: 三分法如实报, 不糊成 "matched no elements"
		// [BUG-FALSE-NEGATIVE-ROLE-CLAIM]。
		return "", fmt.Errorf("canonical selector %q: %w", sel.Raw, e.locatorMissError(sel.Role, nameVal))
	}

	// Apply nth filter
	if sel.Nth > 0 {
		if sel.Nth > len(allMeta) {
			return "", fmt.Errorf("%w: nth=%d out of range (matched %d elements for %q)",
				ErrRefNotFound, sel.Nth, len(allMeta), sel.Raw)
		}
		return allMeta[sel.Nth-1].Ref, nil
	}

	if len(allMeta) > 1 {
		return "", buildAmbiguousError(sel.Raw, allMeta)
	}
	return allMeta[0].Ref, nil
}

// matchOp applies op ("=", "*=", "^=") to haystack and needle.
func matchOp(haystack, op, needle string) bool {
	switch op {
	case "=":
		return haystack == needle
	case "*=":
		return strings.Contains(haystack, needle)
	case "^=":
		return strings.HasPrefix(haystack, needle)
	default:
		return strings.Contains(haystack, needle)
	}
}

// buildAmbiguousError 构建歧义错误信息，附带候选项和消歧建议。
func buildAmbiguousError(locator string, matches []*ElementRef) error {
	suggestions := make([]string, 0, len(matches))
	for i, m := range matches {
		if m.TestID != "" {
			suggestions = append(suggestions, fmt.Sprintf("  - #%s", m.TestID))
		} else if m.RecommendedLocator != "" {
			suggestions = append(suggestions, fmt.Sprintf("  - %s", m.RecommendedLocator))
		} else {
			suggestions = append(suggestions, fmt.Sprintf("  - role=%s[name=\"%s\"][nth=%d]", m.Role, m.Name, i+1))
		}
	}
	msg := fmt.Sprintf("%s:\n  locator: %s\n  matches: %d\n  suggestions:\n%s",
		ErrAmbiguousLocator.Error(), locator, len(matches), strings.Join(suggestions, "\n"))
	return fmt.Errorf("%w: %s", ErrAmbiguousLocator, msg)
}

// isCSSSelector 判断参数是否为 CSS 选择器（而非 element ref 如 "e3" 或 "@r1"）。
func isCSSSelector(ref string) bool {
	if len(ref) < 2 {
		return false
	}
	if strings.HasPrefix(ref, internalBackendNodePrefix) {
		return false
	}
	// Session refs 格式: @r1, @r2, ...
	if strings.HasPrefix(ref, "@r") {
		return false
	}
	// Element refs 格式: e1, e2, ..., e999
	if ref[0] == 'e' {
		for _, c := range ref[1:] {
			if c < '0' || c > '9' {
				return true // 非纯数字，是 CSS 选择器
			}
		}
		return false // e + 纯数字 = element ref
	}
	return true // 不以 'e' 或 '@r' 开头的都是 CSS 选择器
}

// resolveBackendNodeID 将 ref（eN 或 @rN）解析为 BackendNodeID。
func (e *actionEngine) resolveBackendNodeID(ref string) (int64, error) {
	if strings.HasPrefix(ref, internalBackendNodePrefix) {
		value := strings.TrimPrefix(ref, internalBackendNodePrefix)
		backendNodeID, err := strconv.ParseInt(value, 10, 64)
		if err != nil || backendNodeID <= 0 {
			return 0, fmt.Errorf("%w: invalid internal element locator %q", ErrRefNotFound, ref)
		}
		return backendNodeID, nil
	}
	backendNodeID, ok := e.snapEngine.LookupRef(ref)
	if !ok {
		return 0, fmt.Errorf("%w: ref %q not found in current snapshot", ErrRefNotFound, ref)
	}
	return backendNodeID, nil
}

// executeClick 执行点击操作（支持 element ref、@rN ref 和 CSS 选择器）。
func (e *actionEngine) executeClick(ctx context.Context, ref string, seeToClick, elementMode bool) error {
	if elementMode {
		return e.executeElementModeClick(ctx, ref)
	}
	if seeToClick {
		return e.executeSeeToClick(ctx, ref)
	}

	// CSS 选择器模式 — CDP 坐标点击（anti-bot 安全）
	if isCSSSelector(ref) {
		if err := chromedp.Run(ctx, chromedp.WaitVisible(ref, chromedp.ByQuery)); err != nil {
			return err
		}
		box, err := e.resolveElementBox(ctx, ref)
		if err != nil {
			return err
		}
		if box.Width <= 0 || box.Height <= 0 {
			return fmt.Errorf("%w: element %q has invalid box %.1fx%.1f", ErrActFailed, ref, box.Width, box.Height)
		}
		x := box.Left + box.Width*0.5
		y := box.Top + box.Height*0.5
		if err := e.guardPoint(ctx, x, y); err != nil {
			return err
		}
		return dispatchMouseClickAt(ctx, x, y)
	}

	// 对 DOM 发现的 clickable 类型元素，用 data-testid CSS 选择器点击
	if meta, ok := e.snapEngine.LookupRefMeta(ref); ok && meta.Role == "clickable" && meta.Name != "" {
		selector := `[data-testid="` + meta.Name + `"]`
		if err := e.guardBoxCenter(ctx, func() (actionElementBox, error) { return e.elementBoxForSelector(ctx, selector) }); err != nil {
			return err
		}
		return chromedp.Run(ctx, chromedp.Click(selector, chromedp.ByQuery))
	}

	if err := e.guardBoxCenter(ctx, func() (actionElementBox, error) { return e.elementBoxForRef(ctx, ref) }); err != nil {
		return err
	}

	backendNodeID, err := e.resolveBackendNodeID(ref)
	if err != nil {
		return err
	}

	// 通过 BackendNodeID 找到节点并点击
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		// 将 BackendNodeID 解析为 NodeID
		nodeIDs, err := dom.PushNodesByBackendIDsToFrontend([]cdp.BackendNodeID{cdp.BackendNodeID(backendNodeID)}).Do(ctx)
		if err != nil {
			return fmt.Errorf("%w: backend node not found: %v", ErrActFailed, err)
		}
		if len(nodeIDs) == 0 {
			return fmt.Errorf("%w: node not found for ref %q", ErrRefNotFound, ref)
		}
		// 通过 NodeID 执行点击
		return chromedp.Run(ctx, chromedp.Click([]cdp.NodeID{nodeIDs[0]}, chromedp.ByNodeID))
	}))
}

// executeElementModeClick is the explicit journey escape hatch. Locator-driven
// chromedp.Click may scroll the target into view; this is intentionally scoped
// to one mode: element step and never mutates the scenario posture.
func (e *actionEngine) executeElementModeClick(ctx context.Context, ref string) error {
	if isCSSSelector(ref) {
		return chromedp.Run(ctx, chromedp.Click(ref, chromedp.ByQuery))
	}
	backendNodeID, err := e.resolveBackendNodeID(ref)
	if err != nil {
		return err
	}
	return chromedp.Run(ctx, chromedp.ActionFunc(func(execCtx context.Context) error {
		nodeIDs, err := dom.PushNodesByBackendIDsToFrontend([]cdp.BackendNodeID{cdp.BackendNodeID(backendNodeID)}).Do(execCtx)
		if err != nil {
			return fmt.Errorf("%w: backend node not found: %v", ErrActFailed, err)
		}
		if len(nodeIDs) == 0 {
			return fmt.Errorf("%w: node not found for element locator", ErrRefNotFound)
		}
		return chromedp.Run(execCtx, chromedp.Click([]cdp.NodeID{nodeIDs[0]}, chromedp.ByNodeID))
	}))
}

// executeSeeToClick is the default agent click path. It never delegates to
// chromedp.Click, so no implicit scrollIntoView/wait can click content that was
// absent from the witness viewport.
func (e *actionEngine) executeSeeToClick(ctx context.Context, ref string) error {
	// Everything up to dispatchHumanMouseClick below is validation: no input has
	// been sent yet, so every failure here is marked pre-dispatch and leaves the
	// caller's observation intact.
	if isCSSSelector(ref) {
		return e.failedBeforeDispatch(fmt.Errorf("%w: selector %q was not granted by the most recent visible observation", ErrRefNotFound, ref))
	}
	var (
		meta  *ElementRef
		probe elementVisibilityProbe
		err   error
	)
	requireObserved := true
	var ok bool
	meta, ok = e.snapEngine.LookupRefMeta(ref)
	if !ok {
		return e.failedBeforeDispatch(fmt.Errorf("%w: click ref %q is not from the most recent observe — run observe again", ErrRefNotFound, ref))
	}
	// Reject before touching the DOM when session persistence says this came
	// from open/post-act rather than an explicit observe.
	if !meta.Observed || !meta.VisibleInViewport {
		return e.failedBeforeDispatch(fmt.Errorf("%w: click target %q was not in the visible set from the most recent observe — scroll, then run observe again", ErrRefNotFound, ref))
	}
	probe, err = probeMetaVisibilityNow(ctx, meta)
	if err != nil {
		return e.failedBeforeDispatch(fmt.Errorf("%w: resolve click target %q without scrolling: %v — run observe again", ErrRefNotFound, ref, err))
	}
	if err := validateSeeToClickProbe("click", ref, meta, requireObserved, probe); err != nil {
		// The legacy visibility probe uses the bounding-box center. ContentQuads
		// below owns the authoritative aim/hit decision, so defer only the
		// center-occlusion branch and retain all other visibility failures.
		if probe.Box.Width <= 0 || probe.Box.Height <= 0 || !probe.StyleVisible || probe.AreaRatio <= visibleAreaThreshold {
			return e.failedBeforeDispatch(err)
		}
	}
	geometry := resolveHumanTargetGeometry(ctx, meta, probe.Box)
	coverage, err := probeMetaHitCoverage(ctx, meta, geometry)
	if err != nil {
		return e.failedBeforeDispatch(fmt.Errorf("%w: sample click target %q: %v", ErrActFailed, ref, err))
	}
	probe.HitTarget = coverage.centerHit()
	probe.Occluder = coverage.centerOccluder()
	if err := validateSeeToClickProbe("click", ref, meta, requireObserved, probe); err != nil {
		return e.failedBeforeDispatch(err)
	}
	e.updateActionFidelity(func(report *ActionFidelityReport) {
		report.AimSource = geometry.AimSource
		if coverage.count() < len(coverage.Hits) {
			report.HitCoverage = coverage.String()
		}
		if e.interactionPolicy.StrictHuman() {
			report.HumanPath = []string{"mouse_click"}
		}
	})
	if err := e.guardPoint(ctx, geometry.Aim.X, geometry.Aim.Y); err != nil {
		return err
	}
	if err := e.dispatchHumanMouseClick(ctx, geometry.Aim.X, geometry.Aim.Y); err != nil {
		return err
	}
	if e.interactionPolicy.StrictHuman() {
		focused, backendNodeID, focusErr := activeElementMatchesMeta(ctx, meta)
		if focusErr != nil || !focused {
			e.recordHumanFocus(0)
		} else {
			e.recordHumanFocus(backendNodeID)
		}
	}
	return nil
}

// auditOccludedInteractables audits the elements observe dropped for occlusion.
// This is the blind spot --hit-audit used to have: its scope was exactly the
// visible set, so the elements most worth warning about — on screen, covered,
// unclickable — could never be sampled [BUG-OCCLUDED-SILENTLY-DROPPED].
func (e *actionEngine) auditOccludedInteractables(ctx context.Context) ([]HitAuditFinding, error) {
	findings := make([]HitAuditFinding, 0, len(e.snapEngine.occluded))
	for i := range e.snapEngine.occluded {
		entry := e.snapEngine.occluded[i]
		element := entry.Element
		geometry := resolveHumanTargetGeometry(ctx, &element, element.BBox)
		coverage := hitCoverageProbe{}
		occludedBy := entry.Occluder
		if probed, err := probeMetaHitCoverage(ctx, &element, geometry); err == nil {
			coverage = probed
			if named := probed.centerOccluder(); named != "" {
				occludedBy = named
			}
		}
		hitCoverage := "0/5"
		if len(coverage.Hits) > 0 {
			hitCoverage = coverage.String()
		}
		name := element.NameFull
		if name == "" {
			name = element.Name
		}
		box := element.BBox
		findings = append(findings, HitAuditFinding{
			Role:        element.Role,
			Name:        name,
			HitCoverage: hitCoverage,
			AimSource:   geometry.AimSource,
			Scope:       "occluded",
			OccludedBy:  occludedBy,
			BBox:        &box,
		})
	}
	return findings, nil
}

func (e *actionEngine) occludedInteractableCount() int {
	return len(e.snapEngine.occluded)
}

func (e *actionEngine) auditHitCoverage(ctx context.Context, refs []ElementRef) ([]HitAuditFinding, error) {
	findings := make([]HitAuditFinding, 0)
	for i := range refs {
		ref := &refs[i]
		if !ref.VisibleInViewport || !ref.VisibilityKnown {
			continue
		}
		geometry := resolveHumanTargetGeometry(ctx, ref, ref.BBox)
		coverage, err := probeMetaHitCoverage(ctx, ref, geometry)
		if err != nil {
			return nil, fmt.Errorf("hit-audit %s: %w", ref.Ref, err)
		}
		if coverage.count() < len(coverage.Hits) {
			name := ref.NameFull
			if name == "" {
				name = ref.Name
			}
			findings = append(findings, HitAuditFinding{
				Ref:         ref.Ref,
				Role:        ref.Role,
				Name:        name,
				HitCoverage: coverage.String(),
				AimSource:   geometry.AimSource,
				Scope:       "visible",
				OccludedBy:  coverage.centerOccluder(),
			})
		}
	}
	return findings, nil
}

// dispatchHumanMouseClick reuses InputGateway's real mouse dispatcher so the
// explore model gets the same Gaussian event jitter as Human takeover input.
func (e *actionEngine) dispatchHumanMouseClick(ctx context.Context, x, y float64) error {
	if e.humanInput == nil {
		e.humanInput = NewInputGateway(nil, nil)
	}
	events := []*InputEvent{
		{Type: "mouse", Event: "mouseMoved", X: x, Y: y},
		{Type: "mouse", Event: "mousePressed", X: x, Y: y, Button: "left", ClickCount: 1},
		{Type: "mouse", Event: "mouseReleased", X: x, Y: y, Button: "left", ClickCount: 1},
	}
	for _, event := range events {
		if err := e.humanInput.dispatchMouseEvent(ctx, event); err != nil {
			return fmt.Errorf("%w: dispatch human mouse %s: %v", ErrActFailed, event.Event, err)
		}
	}
	return nil
}

type actionElementBox struct {
	Left   float64 `json:"left"`
	Top    float64 `json:"top"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type fillSelectorResult struct {
	Found    bool `json:"found"`
	Editable bool `json:"editable"`
	Password bool `json:"password"`
	Applied  bool `json:"applied"`
	Exact    bool `json:"exact"`
}

type editableValueProbe struct {
	Present bool   `json:"present"`
	Value   string `json:"value"`
}

func (e *actionEngine) elementBoxForSelector(ctx context.Context, ref string) (actionElementBox, error) {
	var boxJSON string
	js := fmt.Sprintf(`(() => {
		const el = document.querySelector(%q);
		if (!el) return null;
		const r = el.getBoundingClientRect();
		return JSON.stringify({left: r.left, top: r.top, width: r.width, height: r.height});
	})()`, ref)
	// Coordinate actions operate on the geometry visible now. Waiting here makes a
	// typo look like a hung gesture and duplicates the explicit `wait` command.
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &boxJSON)); err != nil {
		return actionElementBox{}, err
	}
	if boxJSON == "" || boxJSON == "null" {
		return actionElementBox{}, fmt.Errorf("%w: element %q not found", ErrRefNotFound, ref)
	}
	var box actionElementBox
	if err := json.Unmarshal([]byte(boxJSON), &box); err != nil {
		return actionElementBox{}, fmt.Errorf("%w: parse element box: %v", ErrActFailed, err)
	}
	return box, nil
}

func (e *actionEngine) elementBoxForRef(ctx context.Context, ref string) (actionElementBox, error) {
	backendNodeID, err := e.resolveBackendNodeID(ref)
	if err != nil {
		return actionElementBox{}, err
	}

	var box actionElementBox
	err = chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		nodeIDs, err := dom.PushNodesByBackendIDsToFrontend([]cdp.BackendNodeID{cdp.BackendNodeID(backendNodeID)}).Do(ctx)
		if err != nil || len(nodeIDs) == 0 {
			return fmt.Errorf("%w: node not found for ref %q", ErrRefNotFound, ref)
		}
		boxModel, err := dom.GetBoxModel().WithNodeID(nodeIDs[0]).Do(ctx)
		if err != nil || boxModel == nil || len(boxModel.Border) < 8 {
			return fmt.Errorf("%w: box model unavailable for ref %q", ErrActFailed, ref)
		}
		minX, maxX := boxModel.Border[0], boxModel.Border[0]
		minY, maxY := boxModel.Border[1], boxModel.Border[1]
		for i := 0; i < len(boxModel.Border); i += 2 {
			x := boxModel.Border[i]
			y := boxModel.Border[i+1]
			if x < minX {
				minX = x
			}
			if x > maxX {
				maxX = x
			}
			if y < minY {
				minY = y
			}
			if y > maxY {
				maxY = y
			}
		}
		box = actionElementBox{
			Left:   minX,
			Top:    minY,
			Width:  maxX - minX,
			Height: maxY - minY,
		}
		return nil
	}))
	if err != nil {
		return actionElementBox{}, err
	}
	return box, nil
}

func (e *actionEngine) resolveElementBox(ctx context.Context, ref string) (actionElementBox, error) {
	if isCSSSelector(ref) {
		return e.elementBoxForSelector(ctx, ref)
	}
	return e.elementBoxForRef(ctx, ref)
}

// resolveValidBox 解析元素 box 并校验尺寸有效。坐标动作的统一前置（去重）。
func (e *actionEngine) resolveValidBox(ctx context.Context, ref string) (actionElementBox, error) {
	box, err := e.resolveElementBox(ctx, ref)
	if err != nil {
		return actionElementBox{}, err
	}
	if box.Width <= 0 || box.Height <= 0 {
		return actionElementBox{}, fmt.Errorf("%w: target %q has invalid box %.1fx%.1f", ErrActFailed, ref, box.Width, box.Height)
	}
	return box, nil
}

// resolvePoint 将 (ref, 相对坐标 0..1) 解析为视口绝对坐标。
// 单点坐标动作（clickat/dblclickat/rclickat/hoverat/wheelat/tapat）的统一目标解析。
func (e *actionEngine) resolvePoint(ctx context.Context, ref string, relX, relY float64) (float64, float64, error) {
	box, err := e.resolveValidBox(ctx, ref)
	if err != nil {
		return 0, 0, err
	}
	return box.Left + box.Width*relX, box.Top + box.Height*relY, nil
}

// resolveCoordinateActionPoint accepts both selector-relative coordinates and
// screenshot-derived absolute viewport CSS pixels. Absolute forms deliberately
// carry an empty ref, so hoverat/wheelat can target canvas/page coordinates
// without inventing a selector solely as an origin.
func (e *actionEngine) resolveCoordinateActionPoint(ctx context.Context, op, ref string, x, y float64) (float64, float64, error) {
	if ref != "" {
		return e.resolvePoint(ctx, ref, x, y)
	}
	w, h, err := e.viewportSize(ctx)
	if err != nil {
		return 0, 0, err
	}
	if x < 0 || y < 0 || x >= w || y >= h {
		return 0, 0, fmt.Errorf("%w: %s point (%.1f,%.1f) is outside the %.1fx%.1f CSS-pixel viewport",
			ErrActFailed, op, x, y, w, h)
	}
	if err := e.guardPoint(ctx, x, y); err != nil {
		return 0, 0, err
	}
	return x, y, nil
}

// mouseButtonsMask 返回按键对应的 buttons 位掩码（左1/右2/中4），
// 用于 press 及拖拽过程中 move 的"按键保持按下"状态。
func mouseButtonsMask(button input.MouseButton) int64 {
	switch button {
	case input.Right:
		return 2
	case input.Middle:
		return 4
	default:
		return 1
	}
}

// dispatchMouseClick 在 (x,y) 派发真实鼠标点击：move → 递增 clickCount 的多对 press/release。
// 单一原语参数化覆盖 单击(left,1) / 双击(left,2) / 右键(right,1)：
// clickCount>1 时连续发多对 down/up 且 detail 递增，浏览器据此识别 dblclick。
func dispatchMouseClick(ctx context.Context, x, y float64, button input.MouseButton, clickCount int64) error {
	if clickCount < 1 {
		clickCount = 1
	}
	mask := mouseButtonsMask(button)
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		if err := input.DispatchMouseEvent(input.MouseMoved, x, y).Do(ctx); err != nil {
			return err
		}
		time.Sleep(18 * time.Millisecond)
		for n := int64(1); n <= clickCount; n++ {
			if err := input.DispatchMouseEvent(input.MousePressed, x, y).
				WithButton(button).
				WithButtons(mask).
				WithClickCount(n).
				Do(ctx); err != nil {
				return err
			}
			time.Sleep(42 * time.Millisecond)
			if err := input.DispatchMouseEvent(input.MouseReleased, x, y).
				WithButton(button).
				WithClickCount(n).
				Do(ctx); err != nil {
				return err
			}
			if n < clickCount {
				time.Sleep(30 * time.Millisecond)
			}
		}
		return nil
	}))
}

// dispatchMouseClickAt 是左键单击便捷封装（保留既有调用方语义，如 executeClick 的 CSS 路径）。
func dispatchMouseClickAt(ctx context.Context, x, y float64) error {
	return dispatchMouseClick(ctx, x, y, input.Left, 1)
}

// ============================================================
// § 滚轮派发：目标激活 + ack 等待预算 [BUG-WHEEL-DISPATCH-HANGS]
// ============================================================
//
// 症状：直连 headless 会话里 `scroll down N` / `wheelat x,y` 打满 15s 动作看门狗、
// 无输出，同一页上 click/fill/observe 全部正常。
//
// 根因（实测钉死，见 action_wheel_ack_budget_test.go 的契约与下面的数据）：
//
//  1. Chromium 的 Input.dispatchMouseEvent 不是"发出去就回"：DevTools 把回调压进
//     pending_mouse_callbacks_，要等渲染器 ack 了这个输入事件才 sendSuccess。
//  2. 当页面 target 不是浏览器的前台 target 时，它的鼠标/滚轮事件根本不会被处理，
//     于是那个 ack 永远不来 —— CDP 命令永久挂住。同一 target 上 Runtime.evaluate
//     仍然 1~6ms 返回（渲染器主线程是好的），所以 observe/click 看起来一切正常，
//     只有滚轮整条命令卡死。实测同一页同一 target：
//     裸发       → mouseWheel 5s 预算打满、页面 scrollY 纹丝不动；
//     先 bringToFront → mouseWheel 10~15ms 返回、页面正常滚动。
//  3. 放大器在 executeScroll：一次 `scroll down` 展开成 ceil(viewportH*0.9/120)
//     个脉冲（1080 视口 = 9 个）逐个串行等 ack，任何单脉冲的慢/挂都被乘以 9，
//     所以滚动比只发 1~2 个事件的 click 早得多地撞上看门狗。
//
// 因此这里做两件事，而不是把超时改长：
//   - 派发滚轮前先把目标 target 提到前台（根因），
//   - 给 ack 等待一个上界作为兜底，且超预算时把剩余位移合并成一个事件继续等，
//     位移一分不丢 —— 这正是真实浏览器在渲染器跟不上时对滚轮做的合并。
const (
	// wheelGestureAckBudget 是一整段滚轮手势能花在等 ack 上的总预算。它明显低于
	// 15s 动作看门狗：滚动要么在预算内完成，要么带着明确的警告返回，不能再以
	// "无输出超时"的形式失败。
	wheelGestureAckBudget = 1200 * time.Millisecond
	// wheelBusyPageAckBudget 是"页面确实在忙"被证实之后放宽到的总预算：这时候
	// 滚轮不是没人处理，只是排在页面后面，提前返回只会让页面少滚一段。仍然明显
	// 低于 15s 看门狗。
	wheelBusyPageAckBudget = 10 * time.Second
	// wheelAckPulseCap 是单个脉冲的等待上界，也是"渲染器跟不上了"的判定阈值：
	// 超过它就不再逐个脉冲发，改为合并剩余位移。
	wheelAckPulseCap = 300 * time.Millisecond
	// wheelAckMinWait 是等待下界，保证上一条命令已经写进连接后才排下一条。
	wheelAckMinWait = 25 * time.Millisecond
	// targetActivationBudget 限制"把 target 提到前台"这步的耗时。它是尽力而为的
	// 前置修复，不该反过来成为新的卡点。
	targetActivationBudget = 250 * time.Millisecond
	// rendererProbeBudget 是"渲染器主线程还在不在干活"这一问的等待上界。
	rendererProbeBudget = 250 * time.Millisecond
)

// rendererLooksBusy 判定滚轮没被 ack 到底是"页面滚不动"还是"页面滚得慢" ——
// 这两者的处置完全相反，不能一刀切：
//
//   - 主线程秒回、滚轮却不 ack ⇒ 这个 target 压根没在处理输入（后台 target 就是
//     这样：Runtime.evaluate 1~6ms，mouseWheel 永久挂）。再等下去不会有任何结果，
//     应当立刻返回。
//   - 主线程也不回 ⇒ 页面真的在忙，我们的滚轮排在它后面迟早会被处理。这时提前
//     返回等于"返回快了、页面少滚了"的静默错报，应当等完。
//
// 探针本身出错（没有可用的 CDP context 等）时返回 false：宁可快速返回，也不要凭
// 猜测去延长等待。
func rendererLooksBusy(ctx context.Context) bool {
	answered, err := dispatchInputWithAckBudget(ctx, rendererProbeBudget,
		chromedp.ActionFunc(func(ctx context.Context) error {
			var alive bool
			return chromedp.Evaluate("true", &alive).Do(ctx)
		}))
	return !answered && err == nil
}

// ensureTargetFrontmost 把当前 page target 提到浏览器前台。
//
// 这是滚轮挂死的根因修复：后台 target 的鼠标/滚轮事件不会被处理，
// Input.dispatchMouseEvent 的 ack 永远不来。命令幂等且只要几毫秒，失败不阻断派发
// —— 它只是把"输入能不能被处理"这个前提摆正，真正的结论仍由后面的 ack 给出。
func ensureTargetFrontmost(ctx context.Context) {
	if _, err := dispatchInputWithAckBudget(ctx, targetActivationBudget,
		chromedp.ActionFunc(func(ctx context.Context) error {
			return page.BringToFront().Do(ctx)
		})); err != nil {
		logger.Warn(inputLogContext(), "could not bring page target to front before wheel dispatch",
			"error", err,
			"note", "a background target silently swallows wheel events; dispatch continues but may not be processed")
	}
}

// wheelPulseAckBudget 把手势剩余预算公平摊给剩余脉冲，并夹在 [下界, 上界] 内。
func wheelPulseAckBudget(deadline time.Time, remaining int) time.Duration {
	if remaining < 1 {
		remaining = 1
	}
	budget := time.Until(deadline) / time.Duration(remaining)
	if budget > wheelAckPulseCap {
		budget = wheelAckPulseCap
	}
	if budget < wheelAckMinWait {
		budget = wheelAckMinWait
	}
	return budget
}

// dispatchInputWithAckBudget 派发一条输入 CDP 命令，最多等 budget 拿渲染器 ack。
// 返回 acked=false 表示命令已按序写进连接，但渲染器还没确认收下。
//
// 注意 acked=false 不等于"事件一定会生效"：实测过，排在浏览器输入队列里等 ack 的
// 事件会在 CLI 进程退出、DevTools 连接断开时被丢弃。所以放弃等待前必须先弄清
// 渲染器到底是在忙还是根本没在处理输入（见 rendererLooksBusy），不能一律不等。
//
// 刻意不用 context.WithTimeout 包 chromedp.Run：取消那个派生 context 会连带拆掉
// chromedp 对 target 的附着，之后同一会话的所有命令都返回 context canceled（实测）。
// 改成"命令跑在原 context 的 goroutine 里，调用方只限时等结果"：超预算时 goroutine
// 随本次动作 context 结束而回收，chan 有缓冲，不泄漏。
func dispatchInputWithAckBudget(ctx context.Context, budget time.Duration, action chromedp.Action) (bool, error) {
	return awaitDispatchWithBudget(ctx, budget, func() error {
		return chromedp.Run(ctx, action)
	})
}

// awaitDispatchWithBudget 是 dispatchInputWithAckBudget 的传输无关内核：跑 dispatch，
// 但最多等 budget 拿它的返回。超预算返回 (false, nil)。
func awaitDispatchWithBudget(ctx context.Context, budget time.Duration, dispatch func() error) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if budget <= 0 {
		budget = wheelAckMinWait
	}
	done := make(chan error, 1)
	go func() { done <- dispatch() }()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case err := <-done:
		return true, err
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, nil
	}
}

// dispatchMouseWheel 在 (x,y) 派发真实滚轮：先 move 使指针落在目标上，再发 wheel。
// echarts dataZoom:'inside'、Univer/地图缩放、canvas 内滚动均由 deltaX/deltaY 驱动；
// deltaY<0 一般为向上滚/放大，deltaY>0 向下滚/缩小（最终方向由页面逻辑决定）。
func dispatchMouseWheel(ctx context.Context, x, y, deltaX, deltaY float64) error {
	_, err := dispatchMouseWheelSteps(ctx, x, y, deltaX, deltaY, 1)
	return err
}

// runWheelGesture 派发一段滚轮手势的脉冲序列。
//
// 正常情况下逐个脉冲派发，保持真人滚轮一格一格的 tick 语义。一旦某个脉冲在预算内
// 没等到 ack（= 渲染器跟不上，或者根本没在处理输入），就把剩余脉冲的位移合并成
// 一个事件发出去并等它的 ack —— 位移一分不丢，而 N 次串行往返被压成 2 次。
// 合并本身不是妥协：真实浏览器的 MouseWheelEventQueue 在渲染器跟不上时做的就是
// 把排队的滚轮事件合并成一个大 delta。
//
// 返回 acked=false 表示这段手势最终仍没等到 ack：事件已按序发出，但动作返回时
// 页面可能还没滚到位。
func runWheelGesture(
	steps int,
	deltaX, deltaY float64,
	rendererBusy func() bool,
	pulse func(deltaX, deltaY float64, budget time.Duration) (bool, error),
) (bool, error) {
	if steps <= 0 {
		steps = 1
	}
	deadline := time.Now().Add(wheelGestureAckBudget)
	for i := 0; i < steps; i++ {
		remaining := steps - i
		acked, err := pulse(deltaX, deltaY, wheelPulseAckBudget(deadline, remaining))
		if err != nil {
			return false, fmt.Errorf("step %d/%d: %w", i+1, steps, err)
		}
		if acked {
			continue
		}
		if remaining == 1 {
			// 位移已经全部发出去了，只剩这一条的 ack 没回来 —— 没有可合并的余量，
			// 继续等也不会让页面多滚一格。
			return false, nil
		}
		// 背压：先分清是"滚不动"还是"滚得慢"。证实页面在忙才放宽预算，
		// 否则合并完就走，不为一个不会被处理的事件干等。
		if rendererBusy != nil && rendererBusy() {
			if extended := time.Now().Add(wheelBusyPageAckBudget); extended.After(deadline) {
				deadline = extended
			}
		}
		merged := float64(remaining - 1)
		mergedAcked, err := pulse(deltaX*merged, deltaY*merged, time.Until(deadline))
		if err != nil {
			return false, fmt.Errorf("coalesced tail of %d steps: %w", remaining-1, err)
		}
		return mergedAcked, nil
	}
	return true, nil
}

// dispatchMouseWheelSteps 派发一次滚轮手势：先把 target 提到前台并把指针移到落点，
// 再按 runWheelGesture 的节奏发出 steps 个脉冲。返回 acked=false 表示手势发完时
// 渲染器还没 ack。
func dispatchMouseWheelSteps(ctx context.Context, x, y, deltaX, deltaY float64, steps int) (bool, error) {
	if steps <= 0 {
		steps = 1
	}
	ensureTargetFrontmost(ctx)
	// 指针 move 同样要等渲染器 ack（后台 target 上实测一并挂死），所以也走预算。
	if _, err := dispatchInputWithAckBudget(ctx, wheelAckPulseCap,
		chromedp.ActionFunc(func(ctx context.Context) error {
			return input.DispatchMouseEvent(input.MouseMoved, x, y).Do(ctx)
		})); err != nil {
		return false, err
	}
	acked, err := runWheelGesture(steps, deltaX, deltaY,
		func() bool { return rendererLooksBusy(ctx) },
		func(dx, dy float64, budget time.Duration) (bool, error) {
			return dispatchInputWithAckBudget(ctx, budget, chromedp.ActionFunc(func(ctx context.Context) error {
				return input.DispatchMouseEvent(input.MouseWheel, x, y).
					WithDeltaX(dx).
					WithDeltaY(dy).
					Do(ctx)
			}))
		})
	if err != nil {
		return false, err
	}
	if !acked {
		warnWheelAckPending(steps, x, y)
	}
	return acked, nil
}

// warnWheelAckPending 让"滚轮已派发但渲染器没在预算内 ack"这件事是响的，而不是被
// success:true 吞掉：滚动本身没有失败（事件按序发出去了），但紧接着的 observe
// 有可能看到还没滚到位的页面。
func warnWheelAckPending(steps int, x, y float64) {
	logger.Warn(inputLogContext(), "wheel dispatched without renderer ack within budget",
		"steps", steps,
		"x", x,
		"y", y,
		"budget", wheelGestureAckBudget.String(),
		"note", "remaining scroll delta was coalesced into one event, but the renderer never acknowledged it: the page may still be catching up, or may not have scrolled at all — re-observe before trusting the scroll position")
}

// mouseDragInterpolationSteps 是拖拽时起点→终点之间插入的中间 move 段数。
// canvas 类 UI（echarts dataZoom/brush、Univer 选区）靠逐帧 mousemove 增量识别
// 拖拽轨迹，单次跳变（press→直接 release 在终点）无法触发，必须分段移动。
const mouseDragInterpolationSteps = 12

// dispatchMouseMove 派发单次真实鼠标移动事件。
// canvas 命中测试（echarts tooltip/axisPointer）依赖真实 clientX/Y，
// 与 DOM 合成事件不同，必须经 CDP input 派发。
func dispatchMouseMove(ctx context.Context, x, y float64) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return input.DispatchMouseEvent(input.MouseMoved, x, y).Do(ctx)
	}))
}

// dispatchMouseDrag 派发真实鼠标拖拽：起点 move→press → 多段插值 move（左键按住）→ 终点 release。
// steps<1 时归一为 1。
func dispatchMouseDrag(ctx context.Context, x1, y1, x2, y2 float64, steps int) error {
	if steps < 1 {
		steps = 1
	}
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		if err := input.DispatchMouseEvent(input.MouseMoved, x1, y1).Do(ctx); err != nil {
			return err
		}
		time.Sleep(18 * time.Millisecond)
		if err := input.DispatchMouseEvent(input.MousePressed, x1, y1).
			WithButton(input.Left).
			WithButtons(1).
			WithClickCount(1).
			Do(ctx); err != nil {
			return err
		}
		time.Sleep(18 * time.Millisecond)
		for i := 1; i <= steps; i++ {
			t := float64(i) / float64(steps)
			mx := x1 + (x2-x1)*t
			my := y1 + (y2-y1)*t
			// 拖拽过程中左键保持按下：buttons=1，button=None。
			if err := input.DispatchMouseEvent(input.MouseMoved, mx, my).
				WithButtons(1).
				Do(ctx); err != nil {
				return err
			}
			time.Sleep(16 * time.Millisecond)
		}
		time.Sleep(18 * time.Millisecond)
		return input.DispatchMouseEvent(input.MouseReleased, x2, y2).
			WithButton(input.Left).
			WithClickCount(1).
			Do(ctx)
	}))
}

func dispatchTouchTapAt(ctx context.Context, x, y float64) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		points := []*input.TouchPoint{{
			X:  x,
			Y:  y,
			ID: 1,
		}}
		if err := input.DispatchTouchEvent(input.TouchStart, points).Do(ctx); err != nil {
			return err
		}
		time.Sleep(60 * time.Millisecond)
		return input.DispatchTouchEvent(input.TouchEnd, []*input.TouchPoint{}).Do(ctx)
	}))
}

// dispatchTouchSwipe 派发真实触控滑动：TouchStart → 多段插值 TouchMove → TouchEnd。
// 作为 dragat 的触控对偶，服务移动端 canvas 平移（echarts mobile pan、轮播）。steps<1 归一为 1。
func dispatchTouchSwipe(ctx context.Context, x1, y1, x2, y2 float64, steps int) error {
	if steps < 1 {
		steps = 1
	}
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		if err := input.DispatchTouchEvent(input.TouchStart,
			[]*input.TouchPoint{{X: x1, Y: y1, ID: 1}}).Do(ctx); err != nil {
			return err
		}
		time.Sleep(18 * time.Millisecond)
		for i := 1; i <= steps; i++ {
			t := float64(i) / float64(steps)
			mx := x1 + (x2-x1)*t
			my := y1 + (y2-y1)*t
			if err := input.DispatchTouchEvent(input.TouchMove,
				[]*input.TouchPoint{{X: mx, Y: my, ID: 1}}).Do(ctx); err != nil {
				return err
			}
			time.Sleep(16 * time.Millisecond)
		}
		time.Sleep(18 * time.Millisecond)
		return input.DispatchTouchEvent(input.TouchEnd, []*input.TouchPoint{}).Do(ctx)
	}))
}

// executeClickAt 对目标元素相对坐标执行真实鼠标左键单击。
// 设计意图:
//   - 保持现有 click 语义不变，避免把宿主 DOM 自动化和人类指针输入混为一谈
//   - canvas 类图表(echarts/Univer)无子 DOM，点选具体图元只能靠真实坐标命中
func (e *actionEngine) executeClickAt(ctx context.Context, ref string, relX, relY float64) error {
	x, y, err := e.resolvePoint(ctx, ref, relX, relY)
	if err != nil {
		return err
	}
	if err := e.guardPoint(ctx, x, y); err != nil {
		return err
	}
	e.recordCoordinateHit(ctx, x, y)
	return dispatchMouseClick(ctx, x, y, input.Left, 1)
}

// viewportSize 读取当前视口像素尺寸（CSS 像素）。
// 语义 = 布局视口（等价 CDP Page.getLayoutMetrics 的 cssLayoutViewport），DPR 无关。
// 高度不可直接用 window.innerHeight：chrome 仿真下 innerHeight 被 shim 成小视口
// （页面事实），而 tapxy/clickat 的比例基准必须是布局/截图高（工具真相）——
// 经 LayoutViewportHeightJSExpr 取 lvh，非仿真会话回退 innerHeight（原契约）。
func (e *actionEngine) viewportSize(ctx context.Context) (float64, float64, error) {
	var dims struct {
		W float64 `json:"w"`
		H float64 `json:"h"`
	}
	js := `(() => JSON.stringify({w: window.innerWidth || 0, h: ` + LayoutViewportHeightJSExpr + `}))()`
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &raw)); err != nil {
		return 0, 0, fmt.Errorf("%w: read viewport size: %v", ErrActFailed, err)
	}
	if raw == "" || raw == "null" {
		return 0, 0, fmt.Errorf("%w: viewport size unavailable", ErrActFailed)
	}
	if err := json.Unmarshal([]byte(raw), &dims); err != nil {
		return 0, 0, fmt.Errorf("%w: parse viewport size: %v", ErrActFailed, err)
	}
	if dims.W <= 0 || dims.H <= 0 {
		return 0, 0, fmt.Errorf("%w: viewport size invalid %.0fx%.0f", ErrActFailed, dims.W, dims.H)
	}
	return dims.W, dims.H, nil
}

// executeClickXY clicks an absolute viewport point in CSS pixels. It is the
// direct bridge from screenshot vision coordinates to real mouse input.
func (e *actionEngine) executeClickXY(ctx context.Context, x, y float64, seeToClick bool) error {
	w, h, err := e.viewportSize(ctx)
	if err != nil {
		return err
	}
	if x < 0 || y < 0 || x >= w || y >= h {
		return e.failedBeforeDispatch(fmt.Errorf("%w: click point (%.1f,%.1f) is outside the %.1fx%.1f CSS-pixel viewport", ErrActFailed, x, y, w, h))
	}
	if err := e.guardPoint(ctx, x, y); err != nil {
		return err
	}
	// 坐标点击绕过 a11y 模型 —— 但不绕过证据: 派发前问页面"这个像素归谁",
	// 结果如实写进 hit。不拦截(人类确实可能点在遮罩上), 由 agent 自行断言
	// [BUG-COORD-SILENT-FALSE-SUCCESS]。
	e.recordCoordinateHit(ctx, x, y)
	if seeToClick {
		return e.dispatchHumanMouseClick(ctx, x, y)
	}
	return dispatchMouseClickAt(ctx, x, y)
}

// executeTapXY 对视口比例坐标 (xfrac, yfrac ∈ 0..1) 执行真实鼠标左键单击。
// 完全绕过 a11y/locator 模型：不解析任何 ref，直接 viewport_width*xfrac / viewport_height*yfrac
// 得到视口像素，经 CDP Input.dispatchMouseEvent 发真实左键 press/release。
// 服务自定义 Vue/canvas UI 中不进 a11y 树、css= 选择器无法命中的可见控件。
func (e *actionEngine) executeTapXY(ctx context.Context, fracX, fracY float64) error {
	w, h, err := e.viewportSize(ctx)
	if err != nil {
		return err
	}
	x := w * fracX
	y := h * fracY
	if err := e.guardPoint(ctx, x, y); err != nil {
		return err
	}
	e.recordCoordinateHit(ctx, x, y)
	return dispatchMouseClick(ctx, x, y, input.Left, 1)
}

// executeTypeText 向当前聚焦元素一次性插入文本，完全绕过 a11y/locator 模型：
// 不解析任何 ref/selector，直接经 CDP Input.insertText 把 text 插入当前焦点（聚焦元素）。
// 服务自定义 Vue/canvas input —— 配合 tapxy 先聚焦不进 a11y 树、css= 无法命中的输入框，
// 再用本动作填入文本。与逐字键盘事件不同，insertText 等价于 IME/粘贴提交，单次生效。
func (e *actionEngine) executeTypeText(ctx context.Context, text string) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return input.InsertText(text).Do(ctx)
	}))
}

// executeDoubleClickAt 对目标元素相对坐标执行真实鼠标左键双击（Univer 单元格编辑 / 缩放复位）。
func (e *actionEngine) executeDoubleClickAt(ctx context.Context, ref string, relX, relY float64) error {
	x, y, err := e.resolvePoint(ctx, ref, relX, relY)
	if err != nil {
		return err
	}
	if err := e.guardPoint(ctx, x, y); err != nil {
		return err
	}
	e.recordCoordinateHit(ctx, x, y)
	return dispatchMouseClick(ctx, x, y, input.Left, 2)
}

// executeRightClickAt 对目标元素相对坐标执行真实鼠标右键点击（上下文菜单）。
func (e *actionEngine) executeRightClickAt(ctx context.Context, ref string, relX, relY float64) error {
	x, y, err := e.resolvePoint(ctx, ref, relX, relY)
	if err != nil {
		return err
	}
	if err := e.guardPoint(ctx, x, y); err != nil {
		return err
	}
	e.recordCoordinateHit(ctx, x, y)
	return dispatchMouseClick(ctx, x, y, input.Right, 1)
}

func (e *actionEngine) executeTap(ctx context.Context, ref string) error {
	return e.executeTapAt(ctx, ref, 0.5, 0.5)
}

// guardBoxCenter 对"无显式坐标"的点击路径（chromedp.Click 系）做遮挡守卫：
// 取元素 box 中心投影判定。box 解析失败不阻断（守卫只拦"确证被遮"，
// 解析异常交由后续点击路径自身报错）。
func (e *actionEngine) guardBoxCenter(ctx context.Context, boxFn func() (actionElementBox, error)) error {
	if e.pointerGuard == nil {
		return nil
	}
	box, err := boxFn()
	if err != nil || box.Width <= 0 || box.Height <= 0 {
		return nil
	}
	return e.guardPoint(ctx, box.Left+box.Width*0.5, box.Top+box.Height*0.5)
}

// parseZoomFactor 解析 zoom 参数：1..5 的倍率或 "reset"（=1.0）。
// 范围下限 1 = Safari 语义（捏合不缩小于 1）。
func parseZoomFactor(value string) (float64, error) {
	switch value {
	case "reset", "1", "1.0":
		return 1, nil
	}
	v, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: zoom expects factor 1..5 or 'reset', got %q", ErrActFailed, value)
	}
	if v < 1 || v > 5 {
		return 0, fmt.Errorf("%w: zoom factor %.2f out of range [1,5] (Safari pinch zoom cannot go below 1)", ErrActFailed, v)
	}
	return v, nil
}

// executeZoom 设置页面缩放（视口状态空间的缩放维；browser chrome 仿真下
// chrome 层与遮挡带在屏幕坐标系恒定不动，复刻真机捏合/聚焦放大语义）。
// 确定性：同参数同结果。
func (e *actionEngine) executeZoom(ctx context.Context, value string) error {
	scale, err := parseZoomFactor(value)
	if err != nil {
		return err
	}
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(actCtx context.Context) error {
		return emulation.SetPageScaleFactor(scale).Do(actCtx)
	})); err != nil {
		return fmt.Errorf("%w: set page scale %.2f: %v", ErrActFailed, scale, err)
	}
	e.psMu.Lock()
	e.pageScale = scale
	e.psMu.Unlock()
	return nil
}

// executeTapAt 对目标元素的相对坐标执行真实触控点击。
// 设计意图:
//   - 作为 clickat 的触控对偶，支撑 iOS / Android / coarse pointer 测试
//   - 在 liveview 宿主页上触发 BrowserPanel 的 touch* 链路，而不是退化成 mouse 事件
func (e *actionEngine) executeTapAt(ctx context.Context, ref string, relX, relY float64) error {
	x, y, err := e.resolvePoint(ctx, ref, relX, relY)
	if err != nil {
		return err
	}
	if err := e.guardPoint(ctx, x, y); err != nil {
		return err
	}
	e.recordCoordinateHit(ctx, x, y)
	return dispatchTouchTapAt(ctx, x, y)
}

// executeHoverAt 对目标元素的相对坐标执行真实鼠标悬停（仅 move，不按下）。
// 设计意图:
//   - 作为 clickat 的悬停对偶，覆盖 canvas 类图表的 hover 反馈
//   - echarts tooltip / axisPointer 十字线 / 图表联动 hover 由真实 mousemove 的
//     clientX/Y 命中测试驱动；hover 选择器只能落到元素几何中心，无法定位具体数据点
func (e *actionEngine) executeHoverAt(ctx context.Context, ref string, relX, relY float64) error {
	x, y, err := e.resolveCoordinateActionPoint(ctx, "hoverat", ref, relX, relY)
	if err != nil {
		return err
	}
	e.recordCoordinateHit(ctx, x, y)
	return dispatchMouseMove(ctx, x, y)
}

// executeWheelAt 对目标元素相对坐标执行真实滚轮（echarts dataZoom:'inside'、Univer/地图缩放）。
// ref 为空时，x/y 是视口 CSS 像素绝对坐标。
func (e *actionEngine) executeWheelAt(ctx context.Context, ref string, relX, relY, deltaX, deltaY float64, steps int) error {
	x, y, err := e.resolveCoordinateActionPoint(ctx, "wheelat", ref, relX, relY)
	if err != nil {
		return err
	}
	if steps <= 0 {
		steps = 1
	}
	e.recordCoordinateHit(ctx, x, y)
	if _, err := dispatchMouseWheelSteps(ctx, x, y, deltaX, deltaY, steps); err != nil {
		return fmt.Errorf("%w: wheelat: %v", ErrActFailed, err)
	}
	return nil
}

// executeDragAt 在目标元素内从相对坐标起点拖拽到终点（真实 press→move→release）。
// 设计意图:
//   - 作为 clickat 的拖拽对偶，覆盖 canvas 类 UI 的区域交互
//   - echarts dataZoom 缩放/平移、brush 框选、Univer 单元格选区只能由真实拖拽手势
//     驱动，无法用 DOM 选择器表达；坐标按元素 box 归一，分辨率无关
func (e *actionEngine) executeDragAt(ctx context.Context, ref string, relX1, relY1, relX2, relY2 float64) error {
	box, err := e.resolveValidBox(ctx, ref)
	if err != nil {
		return err
	}
	x1 := box.Left + box.Width*relX1
	y1 := box.Top + box.Height*relY1
	x2 := box.Left + box.Width*relX2
	y2 := box.Top + box.Height*relY2
	return dispatchMouseDrag(ctx, x1, y1, x2, y2, mouseDragInterpolationSteps)
}

// executeSwipeAt 在目标元素内执行真实触控滑动（dragat 的触控对偶，移动端 canvas 平移）。
func (e *actionEngine) executeSwipeAt(ctx context.Context, ref string, relX1, relY1, relX2, relY2 float64) error {
	box, err := e.resolveValidBox(ctx, ref)
	if err != nil {
		return err
	}
	x1 := box.Left + box.Width*relX1
	y1 := box.Top + box.Height*relY1
	x2 := box.Left + box.Width*relX2
	y2 := box.Top + box.Height*relY2
	return dispatchTouchSwipe(ctx, x1, y1, x2, y2, mouseDragInterpolationSteps)
}

var activeElementChainFunction = `function() {
	const target = this;
	if (!target || typeof target.getRootNode !== 'function') return {focused:false};
	let current = target;
	let root = current.getRootNode();
	for (let depth = 0; depth < 32; depth++) {
		if (!root || root.activeElement !== current) return {focused:false};
		if (root.nodeType === 11 && root.host) {
			current = root.host;
			root = current.getRootNode();
			continue;
		}
		if (root.nodeType === 9) {
			const win = root.defaultView;
			if (!win || win === win.top) return {focused:true};
			let frame = null;
			try { frame = win.frameElement; } catch (_) { return {focused:false}; }
			if (!frame) return {focused:false};
			current = frame;
			root = current.getRootNode();
			continue;
		}
		return {focused:false};
	}
	return {focused:false};
}`

func activeElementMatchesMeta(ctx context.Context, meta *ElementRef) (bool, int64, error) {
	var (
		focused       bool
		backendNodeID int64
	)
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(execCtx context.Context) error {
		obj, err := resolveElementObjectForMeta(execCtx, meta)
		if err != nil {
			return err
		}
		defer func() { _ = runtime.ReleaseObject(obj.ObjectID).Do(execCtx) }()
		result, exc, err := runtime.CallFunctionOn(activeElementChainFunction).
			WithObjectID(obj.ObjectID).
			WithReturnByValue(true).
			Do(execCtx)
		if err != nil {
			return err
		}
		if exc != nil {
			return fmt.Errorf("activeElement chain probe failed: %s", exc.Text)
		}
		var probe struct {
			Focused bool `json:"focused"`
		}
		if result == nil || len(result.Value) == 0 || json.Unmarshal(result.Value, &probe) != nil {
			return fmt.Errorf("activeElement chain probe returned no usable value")
		}
		focused = probe.Focused
		if meta != nil {
			backendNodeID = meta.BackendNodeID
		}
		if backendNodeID == 0 {
			node, describeErr := dom.DescribeNode().WithObjectID(obj.ObjectID).Do(execCtx)
			if describeErr != nil {
				return describeErr
			}
			if node != nil {
				backendNodeID = int64(node.BackendNodeID)
			}
		}
		return nil
	}))
	return focused, backendNodeID, err
}

type strictEditableState struct {
	Focused        bool   `json:"focused"`
	Editable       bool   `json:"editable"`
	Password       bool   `json:"password"`
	Tag            string `json:"tag"`
	Value          string `json:"value"`
	Prefix         string `json:"prefix"`
	Suffix         string `json:"suffix"`
	SelectionKnown bool   `json:"selection_known"`
}

var strictEditableStateFunction = `function() {
	const el = this;
	const target = el;
	let current = target;
	let root = current && typeof current.getRootNode === 'function' ? current.getRootNode() : null;
	let focused = !!root;
	for (let depth = 0; focused && depth < 32; depth++) {
		if (!root || root.activeElement !== current) { focused = false; break; }
		if (root.nodeType === 11 && root.host) {
			current = root.host; root = current.getRootNode(); continue;
		}
		if (root.nodeType === 9) {
			const win = root.defaultView;
			if (!win || win === win.top) break;
			try { current = win.frameElement; } catch (_) { focused = false; break; }
			if (!current) { focused = false; break; }
			root = current.getRootNode(); continue;
		}
		focused = false;
	}
	const tag = String(el && el.tagName || '').toUpperCase();
	const editable = tag === 'INPUT' || tag === 'TEXTAREA';
	const type = tag === 'INPUT' ? String(el.type || '').toLowerCase() : '';
	const ac = String(el && el.autocomplete || '').toLowerCase();
	const name = String(el && el.name || '').toLowerCase();
	const password = type === 'password' || ac === 'current-password' || ac === 'new-password' ||
		name === 'passwd' || name === 'password' || name === 'pwd';
	const value = editable ? String(el.value ?? '') : '';
	const selectionKnown = editable && Number.isInteger(el.selectionStart) && Number.isInteger(el.selectionEnd);
	return {
		focused, editable, password, tag, value, selection_known: selectionKnown,
		prefix: selectionKnown ? value.slice(0, el.selectionStart) : value,
		suffix: selectionKnown ? value.slice(el.selectionEnd) : ''
	};
}`

func strictEditableStateForMeta(ctx context.Context, meta *ElementRef) (strictEditableState, error) {
	var state strictEditableState
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(execCtx context.Context) error {
		obj, err := resolveElementObjectForMeta(execCtx, meta)
		if err != nil {
			return err
		}
		defer func() { _ = runtime.ReleaseObject(obj.ObjectID).Do(execCtx) }()
		result, exc, err := runtime.CallFunctionOn(strictEditableStateFunction).
			WithObjectID(obj.ObjectID).
			WithReturnByValue(true).
			Do(execCtx)
		if err != nil {
			return err
		}
		if exc != nil {
			return fmt.Errorf("editable focus probe failed: %s", exc.Text)
		}
		if result == nil || len(result.Value) == 0 {
			return fmt.Errorf("editable focus probe returned no value")
		}
		return json.Unmarshal(result.Value, &state)
	}))
	return state, err
}

func (e *actionEngine) strictTargetMeta(ref string) (*ElementRef, error) {
	if isCSSSelector(ref) {
		return nil, fmt.Errorf("%w: strict-human target %q was not granted by the most recent visible observation", ErrRefNotFound, ref)
	}
	meta, ok := e.snapEngine.LookupRefMeta(ref)
	if !ok || !meta.Observed || !meta.VisibleInViewport {
		return nil, fmt.Errorf("%w: strict-human target %q is not in the visible set from the most recent observe", ErrRefNotFound, ref)
	}
	return meta, nil
}

func (e *actionEngine) executeStrictHumanFocus(ctx context.Context, ref string) error {
	meta, err := e.strictTargetMeta(ref)
	if err != nil {
		return err
	}
	if err := e.executeSeeToClick(ctx, ref); err != nil {
		return err
	}
	focused, backendNodeID, err := activeElementMatchesMeta(ctx, meta)
	if err != nil || !focused {
		e.recordHumanFocus(0)
		return fmt.Errorf("%w: focus not acquired by real click — human cannot type here", ErrActFailed)
	}
	e.recordHumanFocus(backendNodeID)
	e.updateActionFidelity(func(report *ActionFidelityReport) {
		report.HumanPath = []string{"mouse_click", "active_element_verified"}
	})
	return nil
}

func dispatchStrictSelectAllAndBackspace(ctx context.Context) error {
	modifier := input.ModifierCtrl
	if goruntime.GOOS == "darwin" {
		modifier = input.ModifierMeta
	}
	if err := dispatchModifierCombo(ctx, modifier, "a"); err != nil {
		return err
	}
	return chromedp.Run(ctx, chromedp.KeyEvent(mapKeyName("Backspace")))
}

func insertTextPerCharacter(ctx context.Context, value string) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(execCtx context.Context) error {
		for _, char := range value {
			if err := input.InsertText(string(char)).Do(execCtx); err != nil {
				return err
			}
			time.Sleep(humanJitter())
		}
		return nil
	}))
}

func waitForStrictEditableValue(ctx context.Context, meta *ElementRef, want string) error {
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		state, err := strictEditableStateForMeta(ctx, meta)
		if err == nil && state.Focused && state.Editable && state.Value == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: strict-human editable value mismatch", ErrActFailed)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (e *actionEngine) executeStrictHumanText(ctx context.Context, ref, value string, clear, allowPassword bool) error {
	meta, err := e.strictTargetMeta(ref)
	if err != nil {
		return err
	}
	if err := e.executeStrictHumanFocus(ctx, ref); err != nil {
		return err
	}
	state, err := strictEditableStateForMeta(ctx, meta)
	if err != nil {
		return fmt.Errorf("%w: inspect strict-human editable %q: %v", ErrActFailed, ref, err)
	}
	if !state.Focused {
		return fmt.Errorf("%w: focus not acquired by real click — human cannot type here", ErrActFailed)
	}
	if !state.Editable {
		return fmt.Errorf("%w: target %q is not an editable INPUT/TEXTAREA", ErrActFailed, ref)
	}
	if state.Password && !allowPassword {
		return ErrPasswordField
	}

	want := state.Prefix + value + state.Suffix
	path := []string{"mouse_click", "active_element_verified"}
	if clear {
		if err := dispatchStrictSelectAllAndBackspace(ctx); err != nil {
			return fmt.Errorf("%w: clear %q through keyboard: %v", ErrActFailed, ref, err)
		}
		if err := waitForStrictEditableValue(ctx, meta, ""); err != nil {
			return err
		}
		want = value
		path = append(path, "keyboard_select_all", "keyboard_backspace")
	}
	if err := insertTextPerCharacter(ctx, value); err != nil {
		return fmt.Errorf("%w: Input.insertText %q: %v", ErrActFailed, ref, err)
	}
	if err := waitForStrictEditableValue(ctx, meta, want); err != nil {
		return err
	}
	path = append(path, "Input.insertText_per_character", "value_verified")
	e.updateActionFidelity(func(report *ActionFidelityReport) {
		report.HumanPath = path
	})
	return nil
}

func (e *actionEngine) executeStrictHumanPress(ctx context.Context, ref, key string) error {
	if ref != "" {
		meta, err := e.strictTargetMeta(ref)
		if err != nil {
			return err
		}
		focused, backendNodeID, err := activeElementMatchesMeta(ctx, meta)
		if err != nil || !focused || backendNodeID == 0 || backendNodeID != e.currentHumanFocus() {
			return fmt.Errorf("%w: focus not acquired by real click — run click %s first", ErrActFailed, ref)
		}
		state, stateErr := strictEditableStateForMeta(ctx, meta)
		if stateErr == nil && state.Tag == "SELECT" {
			return fmt.Errorf("%w: strict-human press on native <select> is blocked; use select %s '<label-or-value>' (reported synthetic)", ErrActFailed, ref)
		}
		// A verified key event does not revoke the focus established by the
		// earlier real click. Renew the persisted proof against the caller's
		// post-action snapshot epoch so consecutive press actions remain valid.
		e.recordHumanFocus(backendNodeID)
		e.updateActionFidelity(func(report *ActionFidelityReport) {
			report.HumanPath = []string{"human_focus_proof_verified", "Input.dispatchKeyEvent"}
		})
	} else {
		e.updateActionFidelity(func(report *ActionFidelityReport) {
			report.HumanPath = []string{"Input.dispatchKeyEvent"}
		})
	}
	if mods, baseKey, hasMod := parseKeyCombo(key); hasMod {
		return dispatchModifierCombo(ctx, mods, baseKey)
	}
	return chromedp.Run(ctx, chromedp.KeyEvent(mapKeyName(key)))
}

// executeType 执行文本输入操作，密码字段拒绝 [TC-09-U-07, TC-09-U-08]。
func (e *actionEngine) executeType(ctx context.Context, ref string, value string) error {
	// CSS 选择器模式
	if isCSSSelector(ref) {
		return chromedp.Run(ctx,
			chromedp.WaitVisible(ref, chromedp.ByQuery),
			chromedp.SendKeys(ref, value, chromedp.ByQuery),
		)
	}

	nodeID, err := e.resolveBackendNodeID(ref)
	if err != nil {
		return err
	}

	// 检查是否为密码字段 [IR-03, TC-09-U-07, TC-09-U-08]
	isPassword, err := checkPasswordField(ctx, nodeID)
	if err != nil {
		// 检查失败时保守处理（不执行）
		return fmt.Errorf("%w: failed to check field type: %v", ErrActFailed, err)
	}
	if isPassword {
		return ErrPasswordField
	}

	// 使用 chromedp.SendKeys 发送键盘输入
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		nodeIDs, err := dom.PushNodesByBackendIDsToFrontend([]cdp.BackendNodeID{cdp.BackendNodeID(nodeID)}).Do(ctx)
		if err != nil || len(nodeIDs) == 0 {
			return fmt.Errorf("%w: node not found for ref %q", ErrRefNotFound, ref)
		}
		return chromedp.Run(ctx,
			chromedp.Focus([]cdp.NodeID{nodeIDs[0]}, chromedp.ByNodeID),
			chromedp.SendKeys([]cdp.NodeID{nodeIDs[0]}, value, chromedp.ByNodeID),
		)
	}))
}

// executeFillSecret 安全填充：focus + 清空 + CDP Input.insertText（可信输入事件）。
// 这是对 password/敏感字段的**显式 opt-in 安全路径** —— 默认 fill/type 仍拒绝 password
// 字段 [IR-03]（防 AI 误入密码经明文通道），只有调用方明确选用 fillsecret 才放行。
// 相比 fill 的 JS 合成事件（isTrusted:false），CDP Input.insertText 产生**可信输入事件**，
// 穿透 Vue3/React 受控输入的响应式（合成 input 事件会被受控输入在重渲染时重置回旧 ref 值）。
// 值经安全输入通道注入，不回显、不写入动作结果（IR-03 secure input channel）。
func (e *actionEngine) executeFillSecret(ctx context.Context, ref string, value string) error {
	if isCSSSelector(ref) {
		// 先经 native setter 清空并触发 input（让受控 v-model 归零），再聚焦。
		clearJS := fmt.Sprintf(`(() => {
			const el = document.querySelector(%q);
			if (!el) return false;
			const tag = (el.tagName || '').toUpperCase();
			if (tag !== 'INPUT' && tag !== 'TEXTAREA') return false;
			el.focus();
			const proto = tag === 'TEXTAREA'
				? window.HTMLTextAreaElement.prototype
				: window.HTMLInputElement.prototype;
			const desc = Object.getOwnPropertyDescriptor(proto, 'value');
			if (desc && desc.set) { desc.set.call(el, ''); } else { el.value = ''; }
			el.dispatchEvent(new Event('input', { bubbles: true }));
			return true;
		})()`, ref)
		var editable bool
		if err := chromedp.Run(ctx,
			chromedp.WaitVisible(ref, chromedp.ByQuery),
			chromedp.Evaluate(clearJS, &editable),
		); err != nil {
			return err
		}
		if !editable {
			return fmt.Errorf("%w: element %q not found or not an editable INPUT/TEXTAREA", ErrRefNotFound, ref)
		}
		// CDP Input.insertText：可信输入事件，穿透 Vue/React 受控输入与 password 保护。
		if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			return input.InsertText(value).Do(ctx)
		})); err != nil {
			return err
		}
		// 补 change 事件（lazy v-model / 校验）。
		var changed bool
		changeJS := fmt.Sprintf(`(() => { const el = document.querySelector(%q); if (!el) return false; el.dispatchEvent(new Event('change', { bubbles: true })); return true; })()`, ref)
		return chromedp.Run(ctx, chromedp.Evaluate(changeJS, &changed))
	}

	nodeID, err := e.resolveBackendNodeID(ref)
	if err != nil {
		return err
	}
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		nodeIDs, err := dom.PushNodesByBackendIDsToFrontend([]cdp.BackendNodeID{cdp.BackendNodeID(nodeID)}).Do(ctx)
		if err != nil || len(nodeIDs) == 0 {
			return fmt.Errorf("%w: node not found for ref %q", ErrRefNotFound, ref)
		}
		return chromedp.Run(ctx,
			chromedp.Focus([]cdp.NodeID{nodeIDs[0]}, chromedp.ByNodeID),
			chromedp.ActionFunc(func(ctx context.Context) error {
				return input.InsertText(value).Do(ctx)
			}),
		)
	}))
}

// executeHover 执行真实鼠标悬停操作（通过 CDP input.DispatchMouseEvent）。
func (e *actionEngine) executeHover(ctx context.Context, ref string) error {
	// CSS 选择器模式
	if isCSSSelector(ref) {
		// For CSS selector: use chromedp.MouseClickXY after getting bounding box via JS
		var boxJSON string
		js := fmt.Sprintf(`(() => {
			const el = document.querySelector(%q);
			if (!el) return null;
			const r = el.getBoundingClientRect();
			return JSON.stringify({x: r.left + r.width/2, y: r.top + r.height/2});
		})()`, ref)
		if err := chromedp.Run(ctx, chromedp.Evaluate(js, &boxJSON)); err != nil || boxJSON == "" || boxJSON == "null" {
			return fmt.Errorf("%w: element %q not found for hover", ErrRefNotFound, ref)
		}
		var pos struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
		}
		if err := json.Unmarshal([]byte(boxJSON), &pos); err != nil {
			return fmt.Errorf("%w: hover position parse: %v", ErrActFailed, err)
		}
		return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
			return input.DispatchMouseEvent(input.MouseMoved, pos.X, pos.Y).Do(ctx)
		}))
	}

	backendNodeID, err := e.resolveBackendNodeID(ref)
	if err != nil {
		return err
	}

	// Use CDP getBoxModel to get element bounding box (safe path — no DOM state mutation)
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		nodeIDs, err := dom.PushNodesByBackendIDsToFrontend([]cdp.BackendNodeID{cdp.BackendNodeID(backendNodeID)}).Do(ctx)
		if err != nil || len(nodeIDs) == 0 {
			return fmt.Errorf("%w: node not found for ref %q", ErrRefNotFound, ref)
		}

		// Get box model for the node
		boxModel, err := dom.GetBoxModel().WithNodeID(nodeIDs[0]).Do(ctx)
		if err != nil || boxModel == nil || len(boxModel.Content) < 8 {
			// Fallback: just focus the element
			_ = chromedp.Run(ctx, chromedp.Focus([]cdp.NodeID{nodeIDs[0]}, chromedp.ByNodeID))
			return nil
		}

		// Calculate center from content quad (4 points: x1,y1, x2,y2, x3,y3, x4,y4)
		x := (boxModel.Content[0] + boxModel.Content[2]) / 2
		y := (boxModel.Content[1] + boxModel.Content[5]) / 2

		return input.DispatchMouseEvent(input.MouseMoved, x, y).Do(ctx)
	}))
}

// executeFill 执行 fill 操作：清空后输入（适合已有值的输入框）。
func (e *actionEngine) executeFill(ctx context.Context, ref string, value string) error {
	// CSS 选择器模式
	if isCSSSelector(ref) {
		var result fillSelectorResult
		setJS := fmt.Sprintf(`(() => {
			const el = document.querySelector(%q);
			if (!el) return { found: false };
			const tag = (el.tagName || '').toUpperCase();
			if (tag !== 'INPUT' && tag !== 'TEXTAREA') {
				return { found: true, editable: false };
			}
			const type = tag === 'INPUT' ? String(el.type || '').toLowerCase() : '';
			if (type === 'password') {
				return { found: true, editable: true, password: true };
			}
			el.focus();
			const proto = tag === 'TEXTAREA'
				? window.HTMLTextAreaElement.prototype
				: window.HTMLInputElement.prototype;
			const desc = Object.getOwnPropertyDescriptor(proto, 'value');
			if (desc && desc.set) {
				desc.set.call(el, %q);
			} else {
				el.value = %q;
			}
			el.dispatchEvent(new Event('input', { bubbles: true }));
			el.dispatchEvent(new Event('change', { bubbles: true }));
			return { found: true, editable: true, applied: true, exact: String(el.value ?? '') === %q };
		})()`, ref, value, value, value)
		if err := chromedp.Run(ctx,
			chromedp.WaitVisible(ref, chromedp.ByQuery),
			chromedp.Evaluate(setJS, &result),
		); err != nil {
			return err
		}
		if result.Password {
			return ErrPasswordField
		}
		if result.Applied && e.waitForSelectorEditableValue(ctx, ref, value) == nil {
			return nil
		}
		if !result.Found {
			return fmt.Errorf("%w: element %q not found", ErrRefNotFound, ref)
		}
		if result.Editable {
			if err := chromedp.Run(ctx, chromedp.SetValue(ref, value, chromedp.ByQuery)); err == nil {
				if err := e.waitForSelectorEditableValue(ctx, ref, value); err == nil {
					return nil
				}
			}
		}
		if value == "" {
			return fmt.Errorf("%w: fill %q did not converge to empty value", ErrActFailed, ref)
		}
		if err := chromedp.Run(ctx,
			chromedp.Focus(ref, chromedp.ByQuery),
			chromedp.SendKeys(ref, value, chromedp.ByQuery),
		); err != nil {
			return err
		}
		return e.waitForSelectorEditableValue(ctx, ref, value)
	}

	nodeID, err := e.resolveBackendNodeID(ref)
	if err != nil {
		return err
	}

	// 检查是否为密码字段
	isPassword, err := checkPasswordField(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("%w: failed to check field type: %v", ErrActFailed, err)
	}
	if isPassword {
		return ErrPasswordField
	}

	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		nodeIDs, err := dom.PushNodesByBackendIDsToFrontend([]cdp.BackendNodeID{cdp.BackendNodeID(nodeID)}).Do(ctx)
		if err != nil || len(nodeIDs) == 0 {
			return fmt.Errorf("%w: node not found for ref %q", ErrRefNotFound, ref)
		}
		// BUG-1 修复: KeyEvent("\u0001") 不能可靠地触发 Ctrl+A 选全，会插入控制字符导致 URL 重复拼接。
		// 对 fill 直接设置最终值，而不是依赖逐键输入，避免 Vue/React 受控输入把旧值重新拼回去。
		// BUG-FIX (TH-0419-q5b, baidu textarea): 必须按 active.tagName 选 proto。
		// 旧实现用 `INPUT desc || TEXTAREA desc`，对 TEXTAREA 仍命中 INPUT setter，
		// `desc.set.call(textarea, '')` → TypeError "Illegal invocation"。
		clearJS := `(() => {
			const active = document.activeElement;
			if (!active || (active.tagName !== 'INPUT' && active.tagName !== 'TEXTAREA')) {
				return;
			}
			const proto = active.tagName === 'TEXTAREA'
				? window.HTMLTextAreaElement.prototype
				: window.HTMLInputElement.prototype;
			const desc = Object.getOwnPropertyDescriptor(proto, 'value');
			try {
				if (desc && desc.set) {
					desc.set.call(active, '');
				} else {
					active.value = '';
				}
			} catch (e) {
				active.value = '';
			}
			active.dispatchEvent(new Event('input', { bubbles: true }));
			active.dispatchEvent(new Event('change', { bubbles: true }));
		})()`
		setJS := fmt.Sprintf(`((value) => {
			const active = document.activeElement;
			if (!active || (active.tagName !== 'INPUT' && active.tagName !== 'TEXTAREA')) {
				return false;
			}
			const proto = active.tagName === 'TEXTAREA'
				? window.HTMLTextAreaElement.prototype
				: window.HTMLInputElement.prototype;
			const desc = Object.getOwnPropertyDescriptor(proto, 'value');
			if (desc && desc.set) {
				desc.set.call(active, value);
			} else {
				active.value = value;
			}
			active.dispatchEvent(new Event('input', { bubbles: true }));
			active.dispatchEvent(new Event('change', { bubbles: true }));
			return true;
		})(%q)`, value)
		var applied bool
		if err := chromedp.Run(ctx,
			chromedp.Focus([]cdp.NodeID{nodeIDs[0]}, chromedp.ByNodeID),
			chromedp.Evaluate(clearJS, nil),
			chromedp.Evaluate(setJS, &applied),
		); err != nil {
			return err
		}
		if applied {
			if err := e.waitForActiveEditableValue(ctx, value); err == nil {
				return nil
			}
		}
		if err := chromedp.Run(ctx, chromedp.SetValue([]cdp.NodeID{nodeIDs[0]}, value, chromedp.ByNodeID)); err == nil {
			if err := e.waitForActiveEditableValue(ctx, value); err == nil {
				return nil
			}
		}
		if value == "" {
			return fmt.Errorf("%w: fill %q did not converge to empty value", ErrActFailed, ref)
		}
		if err := chromedp.Run(ctx, chromedp.SendKeys([]cdp.NodeID{nodeIDs[0]}, value, chromedp.ByNodeID)); err != nil {
			return err
		}
		return e.waitForActiveEditableValue(ctx, value)
	}))
}

func (e *actionEngine) waitForSelectorEditableValue(ctx context.Context, ref string, want string) error {
	js := fmt.Sprintf(`(() => {
		const el = document.querySelector(%q);
		if (!el || (el.tagName !== 'INPUT' && el.tagName !== 'TEXTAREA')) {
			return { present: false, value: '' };
		}
		return { present: true, value: String(el.value ?? '') };
	})()`, ref)
	return waitForEditableProbe(ctx, js, want)
}

func (e *actionEngine) waitForActiveEditableValue(ctx context.Context, want string) error {
	js := `(() => {
		const el = document.activeElement;
		if (!el || (el.tagName !== 'INPUT' && el.tagName !== 'TEXTAREA')) {
			return { present: false, value: '' };
		}
		return { present: true, value: String(el.value ?? '') };
	})()`
	return waitForEditableProbe(ctx, js, want)
}

func waitForEditableProbe(ctx context.Context, js string, want string) error {
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		var probe editableValueProbe
		if err := chromedp.Run(ctx, chromedp.Evaluate(js, &probe)); err == nil {
			if probe.Present && probe.Value == want {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: editable value mismatch (want=%q)", ErrActFailed, want)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// withResolvedActionElement resolves a CSS/ref target to one JS object and runs
// fn inside a single chromedp action. In particular, fn must call cdproto
// methods directly rather than re-entering chromedp.Run from ActionFunc; the
// latter can strand a CDP response and was the common primitive behind native
// select focus/press hangs.
func (e *actionEngine) withResolvedActionElement(
	ctx context.Context,
	ref string,
	fn func(context.Context, *runtime.RemoteObject) error,
) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(execCtx context.Context) error {
		var (
			obj *runtime.RemoteObject
			err error
		)
		if isCSSSelector(ref) {
			expr := `document.querySelector(` + strconv.Quote(ref) + `)`
			obj, err = resolveElementObjectByExpression(execCtx, expr)
			if err != nil {
				return fmt.Errorf("%w: element %q not found: %v", ErrRefNotFound, ref, err)
			}
		} else {
			backendNodeID, resolveErr := e.resolveBackendNodeID(ref)
			if resolveErr != nil {
				return resolveErr
			}
			obj, err = resolveElementObjectByBackendID(execCtx, backendNodeID)
			if err != nil {
				return fmt.Errorf("%w: node not found for ref %q: %v", ErrRefNotFound, ref, err)
			}
		}
		defer func() { _ = runtime.ReleaseObject(obj.ObjectID).Do(execCtx) }()
		return fn(execCtx, obj)
	}))
}

type nativeSelectKeyResult struct {
	IsSelect  bool   `json:"is_select"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
	Changed   bool   `json:"changed"`
	Value     string `json:"value"`
}

func callNativeSelectKey(ctx context.Context, obj *runtime.RemoteObject, key string, out *nativeSelectKeyResult) error {
	fn := `function() {
		const key = ` + strconv.Quote(key) + `;
		if (!this || String(this.tagName || '').toUpperCase() !== 'SELECT') {
			return {is_select:false, supported:false, reason:'', changed:false, value:''};
		}
		if (this.disabled) {
			return {is_select:true, supported:false, reason:'disabled', changed:false, value:String(this.value || '')};
		}
		const supported = key === 'ArrowDown' || key === 'ArrowUp' || key === 'Home' || key === 'End';
		if (!supported) {
			return {is_select:true, supported:false, reason:'native-picker-key', changed:false, value:String(this.value || '')};
		}
		try { this.focus({preventScroll:true}); } catch (_) { this.focus(); }
		if (document.activeElement !== this) {
			return {is_select:true, supported:false, reason:'focus-failed', changed:false, value:String(this.value || '')};
		}
		const options = Array.from(this.options || []);
		const enabled = options.map((option, index) => ({option, index})).filter(entry =>
			!entry.option.disabled && !(entry.option.parentElement && entry.option.parentElement.disabled));
		let next = this.selectedIndex;
		if (enabled.length > 0) {
			if (key === 'Home') next = enabled[0].index;
			if (key === 'End') next = enabled[enabled.length - 1].index;
			if (key === 'ArrowDown') {
				const candidate = enabled.find(entry => entry.index > this.selectedIndex);
				if (candidate) next = candidate.index;
			}
			if (key === 'ArrowUp') {
				for (let i = enabled.length - 1; i >= 0; i--) {
					if (enabled[i].index < this.selectedIndex || this.selectedIndex < 0) { next = enabled[i].index; break; }
				}
			}
		}
		this.dispatchEvent(new KeyboardEvent('keydown', {key, bubbles:true}));
		const changed = next >= 0 && next !== this.selectedIndex;
		if (changed) {
			this.selectedIndex = next;
			this.dispatchEvent(new Event('input', {bubbles:true}));
			this.dispatchEvent(new Event('change', {bubbles:true}));
		}
		this.dispatchEvent(new KeyboardEvent('keyup', {key, bubbles:true}));
		return {is_select:true, supported:true, reason:'', changed, value:String(this.value || '')};
	}`
	res, exc, err := runtime.CallFunctionOn(fn).
		WithObjectID(obj.ObjectID).
		WithReturnByValue(true).
		Do(ctx)
	if err != nil {
		return err
	}
	if exc != nil {
		return fmt.Errorf("native select key handler failed: %s", exc.Text)
	}
	if res == nil || len(res.Value) == 0 {
		return fmt.Errorf("native select key handler returned no value")
	}
	return json.Unmarshal([]byte(res.Value), out)
}

// executePress 执行按键操作，支持组合键（Ctrl+A, Shift+Enter 等）。
func (e *actionEngine) executePress(ctx context.Context, ref string, key string) error {
	// Focus element first if ref is specified. Native <select> is handled by a
	// non-popup DOM path: directional keys update selectedIndex and dispatch
	// input/change; keys that would summon the browser picker fail immediately.
	if ref != "" {
		var selectResult nativeSelectKeyResult
		canonicalKey := canonicalKeyName(key)
		if err := e.withResolvedActionElement(ctx, ref, func(execCtx context.Context, obj *runtime.RemoteObject) error {
			if err := callNativeSelectKey(execCtx, obj, canonicalKey, &selectResult); err != nil {
				return fmt.Errorf("%w: inspect native select %q for press: %v", ErrActFailed, ref, err)
			}
			if selectResult.IsSelect {
				return nil
			}
			return dom.Focus().WithObjectID(obj.ObjectID).Do(execCtx)
		}); err != nil {
			return err
		}
		if selectResult.IsSelect {
			switch selectResult.Reason {
			case "disabled":
				return fmt.Errorf("%w: cannot press %q on disabled native <select> %q", ErrActFailed, key, ref)
			case "native-picker-key":
				return fmt.Errorf("%w: press %q on native <select> %q is blocked to avoid opening the browser picker; use select %s '<label-or-value>'",
					ErrActFailed, key, ref, ref)
			case "focus-failed":
				return fmt.Errorf("%w: native <select> %q could not be focused for press %q", ErrActFailed, ref, key)
			}
			return nil
		}
	}

	// Modifier combos (Ctrl+B, Shift+Enter, Control+Shift+1, ...) must be
	// dispatched as a real CDP keyDown/keyUp pair carrying the Modifiers
	// bitmask. chromedp.KeyEvent encodes Selenium-style PUA modifier runes
	// ( ...) as *unknown printable chars* — Chrome inserts them as text
	// and never sets ctrlKey/metaKey, so "press Control+b" used to insert a
	// literal 'b'. We bypass that path entirely for combos.
	if mods, baseKey, hasMod := parseKeyCombo(key); hasMod {
		return dispatchModifierCombo(ctx, mods, baseKey)
	}

	// Map key name to chromedp SendKeys string (single key / plain text path —
	// behavior unchanged).
	keyStr := mapKeyName(key)
	return chromedp.Run(ctx, chromedp.KeyEvent(keyStr))
}

// parseKeyCombo 解析含修饰符的按键串（如 "Control+b", "Ctrl+Shift+1"）。
// 返回 CDP Modifiers 位掩码（Alt=1/Ctrl=2/Meta=4/Shift=8）、基础键名、是否含修饰符。
// 不含 '+' 的单键返回 hasMod=false（走原有 chromedp.KeyEvent 路径，行为不变）。
func parseKeyCombo(key string) (mods input.Modifier, baseKey string, hasMod bool) {
	parts := strings.Split(key, "+")
	if len(parts) < 2 {
		return 0, "", false
	}
	baseKey = canonicalKeyName(parts[len(parts)-1])
	for _, mod := range parts[:len(parts)-1] {
		switch strings.ToLower(strings.TrimSpace(mod)) {
		case "ctrl", "control":
			mods |= input.ModifierCtrl // 2
		case "shift":
			mods |= input.ModifierShift // 8
		case "alt", "option":
			mods |= input.ModifierAlt // 1
		case "meta", "cmd", "command", "win", "super":
			mods |= input.ModifierMeta // 4
		default:
			// Unknown token before the last '+' — not a recognized modifier.
			// Treat the whole string as a non-combo to avoid silently dropping it.
			return 0, "", false
		}
	}
	return mods, baseKey, true
}

// dispatchModifierCombo 派发一个修饰键和弦：keyDown(带 Modifiers + key/code/vk) → keyUp。
// 关键：组合键不发 keyChar/Text 事件，因此 Ctrl+B 不会向输入框插入字符。
func dispatchModifierCombo(ctx context.Context, mods input.Modifier, baseKey string) error {
	keyName := keyEventKeyName(baseKey)
	code := codeForKey(baseKey)
	vk := getVirtualKeyCode(keyName)

	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		down := input.DispatchKeyEvent(input.KeyDown).
			WithModifiers(mods).
			WithKey(keyName).
			WithCode(code)
		up := input.DispatchKeyEvent(input.KeyUp).
			WithModifiers(mods).
			WithKey(keyName).
			WithCode(code)
		if vk != 0 {
			down = down.WithWindowsVirtualKeyCode(int64(vk)).WithNativeVirtualKeyCode(int64(vk))
			up = up.WithWindowsVirtualKeyCode(int64(vk)).WithNativeVirtualKeyCode(int64(vk))
		}
		if err := down.Do(ctx); err != nil {
			return fmt.Errorf("%w: dispatch keyDown %q: %v", ErrActFailed, keyName, err)
		}
		return up.Do(ctx)
	}))
}

// keyEventKeyName 返回 CDP keyDown 的 `key` 字段值（KeyboardEvent.key）。
// 特殊键用 canonical 名（Enter/Escape/ArrowUp/F5...）；单字母统一用小写
// （CDP 的 key 字段为不带 shift 的字符值，shift 由 Modifiers 表达）。
func keyEventKeyName(baseKey string) string {
	switch baseKey {
	case "Enter", "Tab", "Escape", "Backspace", "Delete",
		"ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight",
		"Home", "End", "PageUp", "PageDown":
		return baseKey
	}
	if len(baseKey) >= 2 && (baseKey[0] == 'F' || baseKey[0] == 'f') {
		if _, isFn := keyVirtualCodeMap[strings.ToUpper(baseKey)]; isFn {
			return strings.ToUpper(baseKey)
		}
	}
	if len(baseKey) == 1 {
		r := baseKey[0]
		if r >= 'A' && r <= 'Z' {
			return strings.ToLower(baseKey)
		}
	}
	return baseKey
}

// codeForKey 返回 CDP keyDown 的 `code` 字段值（KeyboardEvent.code，物理键位）。
func codeForKey(baseKey string) string {
	switch baseKey {
	case "Enter", "Tab", "Escape", "Backspace", "Delete",
		"ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight",
		"Home", "End", "PageUp", "PageDown":
		return baseKey
	}
	if len(baseKey) >= 2 && (baseKey[0] == 'F' || baseKey[0] == 'f') {
		if _, isFn := keyVirtualCodeMap[strings.ToUpper(baseKey)]; isFn {
			return strings.ToUpper(baseKey)
		}
	}
	if len(baseKey) == 1 {
		c := baseKey[0]
		switch {
		case c >= 'a' && c <= 'z':
			return "Key" + strings.ToUpper(baseKey)
		case c >= 'A' && c <= 'Z':
			return "Key" + baseKey
		case c >= '0' && c <= '9':
			return "Digit" + baseKey
		}
	}
	return baseKey
}

// mapKeyName 将按键名称（如 "Enter", "Ctrl+A"）映射到 chromedp 可用的按键字符串。
func mapKeyName(key string) string {
	// Handle combos like Ctrl+A, Shift+Enter
	// For simple keys, use chromedp key event symbols
	keyMap := map[string]string{
		"Enter":      "\r",
		"Tab":        "\t",
		"Space":      " ", // 关键: 空格键须发真空格 rune(' ')→CDP code=Space/VK32, 才能 toggle 已聚焦的原生 checkbox
		"Escape":     "\u001b",
		"Backspace":  "\u0008",
		"Delete":     "\u007f",
		"ArrowUp":    "\ue012",
		"ArrowDown":  "\ue015",
		"ArrowLeft":  "\ue011",
		"ArrowRight": "\ue014",
		"Home":       "\ue011",
		"End":        "\ue010",
		"PageUp":     "\ue00e",
		"PageDown":   "\ue00f",
		"F1":         "\ue031",
		"F2":         "\ue032",
		"F3":         "\ue033",
		"F4":         "\ue034",
		"F5":         "\ue035",
		"F12":        "\ue03c",
	}

	// Handle modifier combos
	parts := strings.Split(key, "+")
	if len(parts) > 1 {
		// Last part is the key
		baseKey := canonicalKeyName(parts[len(parts)-1])
		modifiers := parts[:len(parts)-1]

		// Build key with modifiers
		var modStr string
		for _, mod := range modifiers {
			switch strings.ToLower(mod) {
			case "ctrl", "control":
				modStr += "\ue009"
			case "shift":
				modStr += "\ue008"
			case "alt":
				modStr += "\ue00a"
			case "meta", "cmd", "command":
				modStr += "\ue03d"
			}
		}
		if mapped, ok := keyMap[baseKey]; ok {
			return modStr + mapped
		}
		return modStr + baseKey
	}

	key = canonicalKeyName(key)
	if mapped, ok := keyMap[key]; ok {
		return mapped
	}
	return key
}

func canonicalKeyName(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "enter", "return":
		return "Enter"
	case "tab":
		return "Tab"
	case "space", "spacebar", " ":
		return "Space"
	case "escape", "esc":
		return "Escape"
	case "backspace":
		return "Backspace"
	case "delete", "del":
		return "Delete"
	case "arrowup", "up":
		return "ArrowUp"
	case "arrowdown", "down":
		return "ArrowDown"
	case "arrowleft", "left":
		return "ArrowLeft"
	case "arrowright", "right":
		return "ArrowRight"
	case "home":
		return "Home"
	case "end":
		return "End"
	case "pageup":
		return "PageUp"
	case "pagedown":
		return "PageDown"
	default:
		upper := strings.ToUpper(strings.TrimSpace(key))
		if len(upper) >= 2 && upper[0] == 'F' {
			return upper
		}
		return strings.TrimSpace(key)
	}
}

// executeBack 导航后退。
// setTimeout(0) 延迟导航：让 Evaluate CDP 响应先返回，避免页面 unload 销毁 JS context
// 导致 chromedp.Evaluate 收到 context destroyed 错误。
func (e *actionEngine) executeBack(ctx context.Context) error {
	return chromedp.Run(ctx, chromedp.Evaluate("setTimeout(() => history.back(), 0)", nil))
}

// executeForward 导航前进。
func (e *actionEngine) executeForward(ctx context.Context) error {
	return chromedp.Run(ctx, chromedp.Evaluate("setTimeout(() => history.forward(), 0)", nil))
}

// executeFocusSelector 聚焦元素。
func (e *actionEngine) executeFocusSelector(ctx context.Context, ref string) error {
	return e.withResolvedActionElement(ctx, ref, func(execCtx context.Context, obj *runtime.RemoteObject) error {
		// HTMLElement.focus() changes focus without asking Chrome to open native
		// picker UI. It also avoids re-entering chromedp.Run from ActionFunc.
		fn := `function() {
			if (!this || typeof this.focus !== 'function') return {focusable:false, focused:false};
			try { this.focus({preventScroll:true}); } catch (_) { this.focus(); }
			return {focusable:true, focused:document.activeElement === this};
		}`
		res, exc, err := runtime.CallFunctionOn(fn).
			WithObjectID(obj.ObjectID).
			WithReturnByValue(true).
			Do(execCtx)
		if err != nil {
			return fmt.Errorf("%w: focus %q: %v", ErrActFailed, ref, err)
		}
		if exc != nil {
			return fmt.Errorf("%w: focus %q: %s", ErrActFailed, ref, exc.Text)
		}
		var result struct {
			Focusable bool `json:"focusable"`
			Focused   bool `json:"focused"`
		}
		if res == nil || len(res.Value) == 0 || json.Unmarshal([]byte(res.Value), &result) != nil {
			return fmt.Errorf("%w: focus %q returned no usable result", ErrActFailed, ref)
		}
		if !result.Focusable || !result.Focused {
			return fmt.Errorf("%w: element %q could not be focused", ErrActFailed, ref)
		}
		return nil
	})
}

// executeScrollIntoView 滚动元素到可见区域。
func (e *actionEngine) executeScrollIntoView(ctx context.Context, ref string) error {
	if isCSSSelector(ref) {
		// 先做一次即时存在性探测再交给 chromedp [BUG-SCROLLINTO-OPAQUE]。
		// chromedp.ByQuery 语义是"等元素出现"，选择器打错 → 干等到 ctx 超时(实测 45s)，
		// 且只吐 "context deadline exceeded" —— agent 会误判成时序问题去重试，
		// 而不是意识到自己选择器写错了。这里把"不存在"翻译成人话，且立刻返回。
		var exists bool
		probeCtx, cancel := context.WithTimeout(ctx, selectorExistsProbeTimeout)
		err := chromedp.Run(probeCtx, chromedp.Evaluate(
			`!!document.querySelector(`+strconv.Quote(ref)+`)`, &exists))
		cancel()
		// 探测本身失败（如选择器语法非法）不武断否定，交给下游 chromedp 处理
		if err == nil && !exists {
			return fmt.Errorf("%w: scrollinto: CSS 选择器 %q 在当前页面匹配 0 个元素 "+
				"(不是超时；先 observe 确认元素真的在 DOM 里)", ErrRefNotFound, ref)
		}
		return chromedp.Run(ctx, chromedp.ScrollIntoView(ref, chromedp.ByQuery))
	}
	// DOM-only custom controls discovered through data-testid have no
	// BackendNodeID. Their explicit scrollto escape hatch can still use the
	// stable testid selector; this is the only explore path allowed to invoke
	// scrollIntoView, and click still requires a subsequent observe.
	if meta, ok := e.snapEngine.LookupRefMeta(ref); ok && meta.BackendNodeID == 0 && meta.TestID != "" {
		selector := `[data-testid="` + meta.TestID + `"]`
		return chromedp.Run(ctx, chromedp.ScrollIntoView(selector, chromedp.ByQuery))
	}
	backendNodeID, err := e.resolveBackendNodeID(ref)
	if err != nil {
		return err
	}
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		nodeIDs, err := dom.PushNodesByBackendIDsToFrontend([]cdp.BackendNodeID{cdp.BackendNodeID(backendNodeID)}).Do(ctx)
		if err != nil || len(nodeIDs) == 0 {
			return fmt.Errorf("%w: node not found for ref %q", ErrRefNotFound, ref)
		}
		return chromedp.Run(ctx, chromedp.ScrollIntoView([]cdp.NodeID{nodeIDs[0]}, chromedp.ByNodeID))
	}))
}

// executeCheck 勾选复选框（若未勾选则点击）。
func (e *actionEngine) executeCheck(ctx context.Context, ref string, seeToClick, elementMode bool) error {
	return e.executeToggleCheckbox(ctx, ref, true, seeToClick, elementMode)
}

// executeUncheck 取消勾选复选框（若已勾选则点击）。
func (e *actionEngine) executeUncheck(ctx context.Context, ref string, seeToClick, elementMode bool) error {
	return e.executeToggleCheckbox(ctx, ref, false, seeToClick, elementMode)
}

// checkedProbe 是"读取元素勾选状态"探针的三态结果。
// Found=false → 元素不存在；Checkable=false → 元素不是可勾选控件（无 .checked 且无 aria-checked）。
type checkedProbe struct {
	Found     bool `json:"found"`
	Checkable bool `json:"checkable"`
	Checked   bool `json:"checked"`
}

// checkedClassifyBody 是"判定元素是否可勾选 + 当前勾选态"的共享 JS 函数体，操作变量 el。
// CSS 选择器路径（document.querySelector）与语义/element ref 路径（CallFunctionOn on node）
// 共用同一判定逻辑，消灭 isCSSSelector 分叉里的重复。
// native <input type=checkbox|radio> 读 el.checked（boolean）；ARIA 控件回退读 aria-checked。
const checkedClassifyBody = `
	if (!el) return { found: false, checkable: false, checked: false };
	if (typeof el.checked === 'boolean') return { found: true, checkable: true, checked: el.checked === true };
	var ac = (typeof el.getAttribute === 'function') ? el.getAttribute('aria-checked') : null;
	if (ac === 'true' || ac === 'false') return { found: true, checkable: true, checked: ac === 'true' };
	return { found: true, checkable: false, checked: false };
`

// executeToggleCheckbox 切换复选框状态。
// 终局实现（去除 no-op 假阳）：读真实 checked 状态走"与 executeClick 同源的节点解析路径"，
// 仅当当前态 != 目标态时才复用 executeClick 点击（保持可信事件链不变）。
// 状态读取失败 / 目标非可勾选控件 → 返回显式错误（fail-loud），绝不静默返回 nil 冒充成功。
func (e *actionEngine) executeToggleCheckbox(ctx context.Context, ref string, wantChecked, seeToClick, elementMode bool) error {
	isChecked, err := e.readCheckedState(ctx, ref)
	if err != nil {
		return err
	}
	if isChecked == wantChecked {
		return nil
	}
	return e.executeClick(ctx, ref, seeToClick, elementMode)
}

// readCheckedState 读取 ref 指向元素的真实勾选态。
// CSS 选择器 → document.querySelector；element/session ref（eN/@rN）→ 解析为已推入前端的
// 节点后 CallFunctionOn（与 executeClick 的 BackendNodeID 解析同源）。两路径共用 checkedClassifyBody。
func (e *actionEngine) readCheckedState(ctx context.Context, ref string) (bool, error) {
	if isCSSSelector(ref) {
		return e.readCheckedStateBySelector(ctx, ref)
	}
	return e.readCheckedStateByRef(ctx, ref)
}

// readCheckedStateBySelector 通过 CSS 选择器读取勾选态。
func (e *actionEngine) readCheckedStateBySelector(ctx context.Context, selector string) (bool, error) {
	js := fmt.Sprintf(`(() => { var el = document.querySelector(%q);%s})()`, selector, checkedClassifyBody)
	var probe checkedProbe
	if err := chromedp.Run(ctx, chromedp.Evaluate(js, &probe)); err != nil {
		return false, fmt.Errorf("%w: 读取 %q 勾选状态失败: %v", ErrActFailed, selector, err)
	}
	return interpretCheckedProbe(selector, probe)
}

// readCheckedStateByRef 通过 element/session ref 读取勾选态：解析 BackendNodeID → dom.ResolveNode
// 拿到 JS 对象句柄 → CallFunctionOn 在该节点上执行 checkedClassifyBody。与 executeClick 同源，
// 不再把语义 locator 字符串塞进 document.querySelector（原病根）。
func (e *actionEngine) readCheckedStateByRef(ctx context.Context, ref string) (bool, error) {
	backendNodeID, err := e.resolveBackendNodeID(ref)
	if err != nil {
		return false, err
	}
	var probe checkedProbe
	err = chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		obj, err := dom.ResolveNode().
			WithBackendNodeID(cdp.BackendNodeID(backendNodeID)).
			Do(ctx)
		if err != nil {
			return fmt.Errorf("%w: 解析 ref %q 节点失败: %v", ErrActFailed, ref, err)
		}
		if obj == nil || obj.ObjectID == "" {
			return fmt.Errorf("%w: node not found for ref %q", ErrRefNotFound, ref)
		}
		defer func() { _ = runtime.ReleaseObject(obj.ObjectID).Do(ctx) }()

		fn := `function() { var el = this;` + checkedClassifyBody + `}`
		res, exc, callErr := runtime.CallFunctionOn(fn).
			WithObjectID(obj.ObjectID).
			WithReturnByValue(true).
			Do(ctx)
		if callErr != nil {
			return fmt.Errorf("%w: 读取 ref %q 勾选状态失败: %v", ErrActFailed, ref, callErr)
		}
		if exc != nil {
			return fmt.Errorf("%w: 读取 ref %q 勾选状态异常: %s", ErrActFailed, ref, exc.Text)
		}
		if res == nil || len(res.Value) == 0 {
			return fmt.Errorf("%w: ref %q 勾选状态探针返回空", ErrActFailed, ref)
		}
		if err := json.Unmarshal([]byte(res.Value), &probe); err != nil {
			return fmt.Errorf("%w: 解析 ref %q 勾选状态探针失败: %v", ErrActFailed, ref, err)
		}
		return nil
	}))
	if err != nil {
		return false, err
	}
	return interpretCheckedProbe(ref, probe)
}

// interpretCheckedProbe 将探针三态结果转为 (checked, error)。
// 未找到 → ErrRefNotFound；非可勾选控件 → 显式错误"目标不是可勾选控件"。
func interpretCheckedProbe(ref string, p checkedProbe) (bool, error) {
	if !p.Found {
		return false, fmt.Errorf("%w: ref %q 未找到", ErrRefNotFound, ref)
	}
	if !p.Checkable {
		return false, fmt.Errorf("%w: 目标 %q 不是可勾选控件（既非 checkbox/radio，也无 aria-checked）", ErrActFailed, ref)
	}
	return p.Checked, nil
}

const humanWheelStepCSSPixels = 120.0

// executeScroll uses a Human wheel in the visual default. The legacy JS path is
// reachable only outside a recognized scenario or from an explicit element
// step. `scroll <ref> ...` moves the pointer to the target first so nested
// scroll containers receive the event naturally.
func (e *actionEngine) executeScroll(ctx context.Context, targetRef, dir string, steps int, seeToClick bool) error {
	dir = strings.ToLower(strings.TrimSpace(dir))
	if dir != "down" && dir != "up" {
		return fmt.Errorf("%w: scroll direction must be up or down, got %q", ErrActFailed, dir)
	}
	if !seeToClick && targetRef == "" && steps == 0 {
		deltaY := 300
		if dir == "up" {
			deltaY = -deltaY
		}
		return chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf("window.scrollBy(0, %d)", deltaY), nil))
	}

	viewportW, viewportH, err := e.viewportSize(ctx)
	if err != nil {
		return err
	}
	if steps == 0 {
		steps = int(math.Ceil((viewportH * 0.9) / humanWheelStepCSSPixels))
		if steps < 1 {
			steps = 1
		}
	}
	x, y := viewportW*0.5, viewportH*0.5
	if targetRef != "" {
		if seeToClick {
			var (
				meta            *ElementRef
				probe           elementVisibilityProbe
				requireObserved = !isCSSSelector(targetRef)
			)
			if requireObserved {
				var ok bool
				meta, ok = e.snapEngine.LookupRefMeta(targetRef)
				if !ok {
					return fmt.Errorf("%w: scroll target %q is not from the most recent observe", ErrRefNotFound, targetRef)
				}
				probe, err = probeMetaVisibilityNow(ctx, meta)
			} else {
				probe, err = probeSelectorVisibilityNow(ctx, targetRef)
			}
			if err != nil {
				return fmt.Errorf("%w: resolve scroll target %q: %v", ErrRefNotFound, targetRef, err)
			}
			if err := validateSeeToClickProbe("scroll", targetRef, meta, requireObserved, probe); err != nil {
				return err
			}
			x = probe.Box.X + probe.Box.Width*0.5
			y = probe.Box.Y + probe.Box.Height*0.5
		} else {
			x, y, err = e.resolvePoint(ctx, targetRef, 0.5, 0.5)
			if err != nil {
				return err
			}
		}
	}
	if err := e.guardPoint(ctx, x, y); err != nil {
		return err
	}
	deltaY := humanWheelStepCSSPixels
	if dir == "up" {
		deltaY = -deltaY
	}
	if seeToClick {
		return e.dispatchHumanMouseWheel(ctx, x, y, deltaY, steps)
	}
	// 一次 move + steps 个脉冲 = 一段手势，共享同一份 ack 预算。逐个 steps 调
	// dispatchMouseWheel 会让每个脉冲各自重发一次 move、各自吃一份预算。
	_, err = dispatchMouseWheelSteps(ctx, x, y, 0, deltaY, steps)
	return err
}

// dispatchHumanMouseWheel 走人类输入网关派发滚轮手势。先把 target 提到前台（后台
// target 会把滚轮事件整条吞掉，见 wheelGestureAckBudget 处的根因说明），再按
// runWheelGesture 的节奏发脉冲，等 ack 的时间有硬上界。
func (e *actionEngine) dispatchHumanMouseWheel(ctx context.Context, x, y, deltaY float64, steps int) error {
	if e.humanInput == nil {
		e.humanInput = NewInputGateway(nil, nil)
	}
	if steps <= 0 {
		steps = 1
	}
	ensureTargetFrontmost(ctx)
	if _, err := e.humanInput.dispatchMouseEventWithAckBudget(ctx,
		&InputEvent{Type: "mouse", Event: "mouseMoved", X: x, Y: y},
		wheelAckPulseCap); err != nil {
		return fmt.Errorf("%w: move mouse before scroll: %v", ErrActFailed, err)
	}
	acked, err := runWheelGesture(steps, 0, deltaY,
		func() bool { return rendererLooksBusy(ctx) },
		func(dx, dy float64, budget time.Duration) (bool, error) {
			event := &InputEvent{Type: "mouse", Event: "mouseWheel", X: x, Y: y, DeltaX: dx, DeltaY: dy}
			return e.humanInput.dispatchMouseEventWithAckBudget(ctx, event, budget)
		})
	if err != nil {
		return fmt.Errorf("%w: dispatch human wheel: %v", ErrActFailed, err)
	}
	if !acked {
		warnWheelAckPending(steps, x, y)
	}
	return nil
}

type nativeSelectOption struct {
	Label    string `json:"label"`
	Value    string `json:"value"`
	Disabled bool   `json:"disabled"`
}

type nativeSelectProbe struct {
	IsSelect      bool                 `json:"is_select"`
	Disabled      bool                 `json:"disabled"`
	SelectedIndex int                  `json:"selected_index"`
	Options       []nativeSelectOption `json:"options"`
}

type nativeSelectMatch struct {
	Index int
	Kind  string
}

func formatNativeSelectOptions(options []nativeSelectOption) string {
	if len(options) == 0 {
		return "<none>"
	}
	items := make([]string, 0, len(options))
	for _, option := range options {
		item := fmt.Sprintf("%q→%q", option.Label, option.Value)
		if option.Disabled {
			item += " (disabled)"
		}
		items = append(items, item)
	}
	return strings.Join(items, ", ")
}

// matchNativeSelectOption implements the CLI contract in strict priority:
// exact value, exact visible label, then unique visible-label prefix.
func matchNativeSelectOption(options []nativeSelectOption, query string) (nativeSelectMatch, error) {
	for index, option := range options {
		if option.Value == query {
			if option.Disabled {
				return nativeSelectMatch{}, fmt.Errorf("%w: option %q→%q is disabled; options: %s",
					ErrActFailed, option.Label, option.Value, formatNativeSelectOptions(options))
			}
			return nativeSelectMatch{Index: index, Kind: "value"}, nil
		}
	}
	for index, option := range options {
		if option.Label == query {
			if option.Disabled {
				return nativeSelectMatch{}, fmt.Errorf("%w: option %q→%q is disabled; options: %s",
					ErrActFailed, option.Label, option.Value, formatNativeSelectOptions(options))
			}
			return nativeSelectMatch{Index: index, Kind: "label"}, nil
		}
	}
	prefixMatches := make([]int, 0, 2)
	for index, option := range options {
		if strings.HasPrefix(option.Label, query) {
			prefixMatches = append(prefixMatches, index)
		}
	}
	if len(prefixMatches) == 1 {
		index := prefixMatches[0]
		if options[index].Disabled {
			return nativeSelectMatch{}, fmt.Errorf("%w: option %q→%q is disabled; options: %s",
				ErrActFailed, options[index].Label, options[index].Value, formatNativeSelectOptions(options))
		}
		return nativeSelectMatch{Index: index, Kind: "label-prefix"}, nil
	}
	if len(prefixMatches) > 1 {
		matched := make([]nativeSelectOption, 0, len(prefixMatches))
		for _, index := range prefixMatches {
			matched = append(matched, options[index])
		}
		return nativeSelectMatch{}, fmt.Errorf("%w: label prefix %q is ambiguous (%s); options: %s",
			ErrActFailed, query, formatNativeSelectOptions(matched), formatNativeSelectOptions(options))
	}
	return nativeSelectMatch{}, fmt.Errorf("%w: no option matched %q by exact value, exact label, or unique label prefix; options: %s",
		ErrActFailed, query, formatNativeSelectOptions(options))
}

func probeNativeSelect(ctx context.Context, obj *runtime.RemoteObject, out *nativeSelectProbe) error {
	fn := `function() {
		if (!this || String(this.tagName || '').toUpperCase() !== 'SELECT') {
			return {is_select:false, disabled:false, selected_index:-1, options:[]};
		}
		const options = Array.from(this.options || []).map(option => ({
			label:String(option.label || option.textContent || '').trim(),
			value:String(option.value ?? ''),
			disabled:!!option.disabled || !!(option.parentElement && option.parentElement.disabled)
		}));
		return {is_select:true, disabled:!!this.disabled, selected_index:this.selectedIndex, options};
	}`
	res, exc, err := runtime.CallFunctionOn(fn).
		WithObjectID(obj.ObjectID).
		WithReturnByValue(true).
		Do(ctx)
	if err != nil {
		return err
	}
	if exc != nil {
		return fmt.Errorf("native select probe failed: %s", exc.Text)
	}
	if res == nil || len(res.Value) == 0 {
		return fmt.Errorf("native select probe returned no value")
	}
	return json.Unmarshal([]byte(res.Value), out)
}

type nativeSelectApplyResult struct {
	Applied       bool   `json:"applied"`
	SelectedIndex int    `json:"selected_index"`
	Label         string `json:"label"`
	Value         string `json:"value"`
}

func applyNativeSelectIndex(ctx context.Context, obj *runtime.RemoteObject, index int, out *nativeSelectApplyResult) error {
	fn := `function() {
		const index = ` + strconv.Itoa(index) + `;
		if (!this || String(this.tagName || '').toUpperCase() !== 'SELECT' || index < 0 || index >= this.options.length) {
			return {applied:false, selected_index:-1, label:'', value:''};
		}
		this.selectedIndex = index;
		this.dispatchEvent(new Event('input', {bubbles:true}));
		this.dispatchEvent(new Event('change', {bubbles:true}));
		const selected = this.options[this.selectedIndex];
		return {
			applied:true,
			selected_index:this.selectedIndex,
			label:String(selected ? (selected.label || selected.textContent || '') : '').trim(),
			value:String(this.value ?? '')
		};
	}`
	res, exc, err := runtime.CallFunctionOn(fn).
		WithObjectID(obj.ObjectID).
		WithReturnByValue(true).
		Do(ctx)
	if err != nil {
		return err
	}
	if exc != nil {
		return fmt.Errorf("native select apply failed: %s", exc.Text)
	}
	if res == nil || len(res.Value) == 0 {
		return fmt.Errorf("native select apply returned no value")
	}
	return json.Unmarshal([]byte(res.Value), out)
}

// executeSelect changes native <select> state entirely through DOM state and
// bubbling input/change events. It never focuses or opens the browser picker.
func (e *actionEngine) executeSelect(ctx context.Context, ref string, query string) error {
	return e.withResolvedActionElement(ctx, ref, func(execCtx context.Context, obj *runtime.RemoteObject) error {
		var probe nativeSelectProbe
		if err := probeNativeSelect(execCtx, obj, &probe); err != nil {
			return fmt.Errorf("%w: inspect select %q: %v", ErrActFailed, ref, err)
		}
		if !probe.IsSelect {
			return fmt.Errorf("%w: target %q is not a native <select>", ErrActFailed, ref)
		}
		if probe.Disabled {
			return fmt.Errorf("%w: native <select> %q is disabled; options: %s",
				ErrActFailed, ref, formatNativeSelectOptions(probe.Options))
		}
		match, err := matchNativeSelectOption(probe.Options, query)
		if err != nil {
			return err
		}
		var applied nativeSelectApplyResult
		if err := applyNativeSelectIndex(execCtx, obj, match.Index, &applied); err != nil {
			return fmt.Errorf("%w: apply select %q (%s match): %v", ErrActFailed, ref, match.Kind, err)
		}
		want := probe.Options[match.Index]
		if !applied.Applied || applied.SelectedIndex != match.Index || applied.Value != want.Value {
			return fmt.Errorf("%w: select %q did not converge after %s match (want %q→%q, got %q→%q); options: %s",
				ErrActFailed, ref, match.Kind, want.Label, want.Value, applied.Label, applied.Value,
				formatNativeSelectOptions(probe.Options))
		}
		return nil
	})
}

// checkPasswordField 通过 CDP 检查节点是否为密码字段 [TC-09-U-07, TC-09-U-08]。
func checkPasswordField(ctx context.Context, backendNodeID int64) (bool, error) {
	var isPassword bool
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		// 将 BackendNodeID 转换为 NodeID
		nodeIDs, err := dom.PushNodesByBackendIDsToFrontend(
			[]cdp.BackendNodeID{cdp.BackendNodeID(backendNodeID)},
		).Do(ctx)
		if err != nil || len(nodeIDs) == 0 {
			return nil // 无法获取时，保守不拦截
		}

		attrs, err := dom.GetAttributes(nodeIDs[0]).Do(ctx)
		if err != nil {
			return nil
		}

		// attrs 是 key-value 交替的 []string: ["type", "password", "name", "pwd", ...]
		for i := 0; i+1 < len(attrs); i += 2 {
			key := strings.ToLower(attrs[i])
			val := strings.ToLower(attrs[i+1])
			if key == "type" && val == "password" {
				isPassword = true
				return nil
			}
			if key == "autocomplete" && (val == "current-password" || val == "new-password") {
				isPassword = true
				return nil
			}
			if key == "name" && (val == "passwd" || val == "password" || val == "pwd") {
				isPassword = true
				return nil
			}
		}
		return nil
	}))

	return isPassword, err
}
