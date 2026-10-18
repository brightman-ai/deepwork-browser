package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"regexp"
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
	// browserInputKey 标识"这个引擎坐在哪个浏览器实例上"(ws_url 优先,退到 chrome
	// pid)。两个会话可能共享同一个浏览器实例(mux-host 多 tab),输入派发窗口要按它
	// 取第二把锁。空 = 构造不出键,输入临界区退化为不加锁(见 AcquireBrowserInputLock)。
	browserInputKey string
	authorityCheck  func(context.Context, bool, string) error
	// targetActivation is injectable for attached headed sessions. Those
	// sessions must validate display containment but never call Page.bringToFront.
	targetActivation func(context.Context) (bool, error)
	operationID      string
}

// setBrowserInputKey 由 browserCoreImpl 在构造完成后注入。单写者:构造期写,执行期只读。
func (e *actionEngine) setBrowserInputKey(wsURL string, chromePID int) {
	e.bindBrowserInputKey(BrowserInputKey(wsURL, chromePID))
}

func (e *actionEngine) bindBrowserInputKey(key string) {
	e.browserInputKey = key
	if e.browserInputKey != "" {
		digest := sha256.Sum256([]byte(e.browserInputKey))
		e.snapEngine.runtimeRunID = hex.EncodeToString(digest[:])
	}
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
	if report.Selection != nil {
		selection := *report.Selection
		report.Selection = &selection
	}
	return report
}

func (e *actionEngine) restoreHumanFocus(state HumanFocusState) {
	e.fidelityMu.Lock()
	e.humanFocusBackendNodeID = state.BackendNodeID
	e.snapEngine.humanFocusBackendID.Store(state.BackendNodeID)
	e.fidelityMu.Unlock()
}

func (e *actionEngine) recordHumanFocus(backendNodeID int64) {
	e.fidelityMu.Lock()
	e.humanFocusBackendNodeID = backendNodeID
	e.snapEngine.humanFocusBackendID.Store(backendNodeID)
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

// actionRequest contains validated dispatch fields derived from Action.
type actionRequest struct {
	ObservationID string
	Op            string // "click" | "clickxy" | "clickat" | "hoverat" | "dragat" | "tap" | "tapat" | "type" | "scroll" | "hover" | "select"
	Ref           string // Element Ref（如 "e3"）或语义选择器（如 "#testid", "button:'name'"）
	Value         string // text/key/zoom/keyboard value
	Option        *SelectOptionSelector
	CoordX        float64 // 有 Ref: 元素内相对 X(0..1)；无 Ref: 视口 CSS px
	CoordY        float64 // 有 Ref: 元素内相对 Y(0..1)；无 Ref: 视口 CSS px
	CoordX2       float64 // dragat/swipeat 终点相对 X 坐标（0..1）
	CoordY2       float64 // dragat/swipeat 终点相对 Y 坐标（0..1）
	DeltaX        float64 // wheelat 横向滚轮位移（带符号像素）
	DeltaY        float64 // wheelat 纵向滚轮位移（带符号像素）
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

	if strings.HasPrefix(selector, "@o") {
		return &ParsedSelector{SType: SelectorSessionRef, Raw: selector}, nil
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
			return nil, fmt.Errorf("%w: trailing canonical selector data", ErrActFailed)
		}
		end := -1
		quote := byte(0)
		escaped := false
		for index := 1; index < len(filterStr); index++ {
			char := filterStr[index]
			if escaped {
				escaped = false
				continue
			}
			if char == '\\' && quote != 0 {
				escaped = true
				continue
			}
			if quote != 0 {
				if char == quote {
					quote = 0
				}
				continue
			}
			if char == '"' || char == '\'' {
				quote = char
				continue
			}
			if char == ']' {
				end = index
				break
			}
		}
		if end < 0 {
			return nil, fmt.Errorf("%w: unterminated canonical filter", ErrActFailed)
		}

		inner := filterStr[1:end]
		filterStr = filterStr[end+1:]

		// Parse inner: field op "value" or field=N (for nth)
		inner = strings.TrimSpace(inner)
		if strings.HasPrefix(inner, "nth=") {
			n, err := strconv.Atoi(strings.TrimSpace(inner[4:]))
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("%w: nth must be a positive integer", ErrActFailed)
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
				if strings.HasPrefix(val, "\"") {
					decoded, err := strconv.Unquote(val)
					if err != nil {
						return nil, fmt.Errorf("%w: invalid quoted filter value", ErrActFailed)
					}
					val = decoded
				} else if len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'' {
					val = strings.ReplaceAll(strings.ReplaceAll(val[1:len(val)-1], "\\'", "'"), "\\\\", "\\")
				} else {
					return nil, fmt.Errorf("%w: canonical filter value must be quoted", ErrActFailed)
				}
				break
			}
		}
		if field != "name" && field != "testid" && field != "placeholder" {
			return nil, fmt.Errorf("%w: unsupported canonical filter field", ErrActFailed)
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
		"Home", "End", "PageUp", "PageDown", "Insert":
		return nil
	}
	if _, ok := keyVirtualCodeMap[strings.ToUpper(canonical)]; ok && strings.HasPrefix(strings.ToUpper(canonical), "F") {
		return nil
	}
	return fmt.Errorf("%w: press key %q is not a supported single key or modifier chord", ErrActFailed, key)
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

func (e *actionEngine) executeRequestWithInteractionMode(ctx context.Context, parsed *actionRequest, observe, sessionMode bool, mode InteractionMode) (*Snapshot, error) {
	e.beginActionFidelity()
	if parsed.Op == "select" {
		e.updateActionFidelity(func(report *ActionFidelityReport) { report.Execution = "none" })
	}
	normalizedMode, err := NormalizeInteractionMode(string(mode))
	if err != nil {
		return nil, e.failedBeforeDispatch(err)
	}
	e.updateActionFidelity(func(report *ActionFidelityReport) { report.InteractionMode = normalizedMode })
	elementMode := normalizedMode == InteractionModeElement
	if elementMode && e.interactionPolicy.StrictHuman() {
		return nil, e.failedBeforeDispatch(fmt.Errorf("%w: mode element is unavailable in strict-human scenario", ErrActFailed))
	}
	seeToClick := e.seeToClick && !elementMode
	strictHuman := e.interactionPolicy.StrictHuman() || (e.interactionPolicy.Fidelity == InteractionFidelityDual && !elementMode)

	// page-level 操作不需要选择器
	// tapxy 是绝对视口坐标点击，刻意不解析 ref（绕过 a11y/locator）。
	// typetext 向当前聚焦元素插入文本，同样刻意不解析 ref。
	noSelectorOps := map[string]bool{"scroll": true, "back": true, "forward": true, "clickxy": true, "dblclickxy": true, "dragxy": true, "tapxy": true, "typetext": true, "zoom": true, "keyboard": true, "navigate": true, "reload": true}

	var resolvedRef string
	if !noSelectorOps[parsed.Op] && parsed.Ref != "" {
		ref, err := e.resolveActionTarget(ctx, parsed.Ref, sessionMode, elementMode, parsed.Op)
		if err != nil {
			return nil, e.failedBeforeDispatch(err)
		}
		resolvedRef = ref
	} else {
		resolvedRef = parsed.Ref
	}
	if parsed.ObservationID == "" && strings.HasPrefix(resolvedRef, "@o") {
		id, err := observationIDFromRef(resolvedRef)
		if err != nil {
			return nil, e.failedBeforeDispatch(err)
		}
		parsed.ObservationID = id
	}
	var resolvedScrollTarget string
	if parsed.Op == "scroll" && parsed.ScrollTarget != "" {
		resolvedScrollTarget, err = e.resolveActionTarget(ctx, parsed.ScrollTarget, sessionMode, elementMode, "scrollinto")
		if err != nil {
			return nil, e.failedBeforeDispatch(err)
		}
		if parsed.ObservationID == "" && strings.HasPrefix(resolvedScrollTarget, "@o") {
			parsed.ObservationID, err = observationIDFromRef(resolvedScrollTarget)
			if err != nil {
				return nil, e.failedBeforeDispatch(err)
			}
		}
	}

	// — 输入派发窗口 —— 所有真实输入动作的单一公共入口 [ARCH-INPUT-FRONTMOST]。
	//
	// 校验期到此为止，下面开始真的碰页面。前台确保(bringToFront)与浏览器级输入
	// 临界区都收口在这一处：每个输入动作只经过一次，滚轮路径原来那两处调用已经
	// 撤掉。窗口在 switch 结束时关闭 —— DOM settle / 快照不占前台归属。

	releaseInputWindow, brought, contention, windowErr := e.enterInputDispatchWindowForRequest(ctx, actionRequiresObservation(parsed, elementMode, e.seeToClick), parsed.ObservationID, inputDispatchOps[parsed.Op])
	if windowErr != nil {
		var staged *ActionStageError
		if errors.As(windowErr, &staged) {
			return nil, e.failedBeforeDispatch(windowErr)
		}
		return nil, e.failedBeforeDispatch(&ActionStageError{Phase: "activation", State: "not-dispatched", Cause: windowErr})
	}
	if brought || contention != "" {
		e.updateActionFidelity(func(report *ActionFidelityReport) {
			report.BroughtToFront = brought
			report.InputContention = contention
		})
	}

	// 直接使用调用方 context（不派生子 context）。
	// 原因: chromedp 在 NewRemoteAllocator 模式下，context.WithTimeout 派生的 context
	// 会导致 CDP 响应路由异常（Run 挂起直到超时）。调用方 context 已包含合理超时。
	// 诊断证据: EvalJS(browserCtx) 370µs OK, Run(WithTimeout(browserCtx, 5s)) 5s 超时。
	dispatchErr := func() error {
		defer releaseInputWindow()
		return e.dispatchActionRequest(ctx, parsed, resolvedRef, resolvedScrollTarget, seeToClick, elementMode, strictHuman)
	}()
	e.snapEngine.revokeObservationAuthority()
	if dispatchErr != nil {
		return nil, dispatchErr
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
		return e.snapEngine.captureSnapshot(ctx)
	}
	return e.snapEngine.captureSnapshot(ctx)
}

// dispatchActionRequest 是动作动词的分派表。它被单独切出来，是为了让"输入派发窗口"
// 有一个精确的边界：窗口在进入这里之前打开、在离开时立刻关闭，覆盖且只覆盖
// bringToFront→dispatch→等 ack 这一段。
func (e *actionEngine) dispatchActionRequest(
	ctx context.Context,
	parsed *actionRequest,
	resolvedRef string,
	resolvedScrollTarget string,
	seeToClick bool,
	elementMode bool,
	strictHuman bool,
) error {
	var err error
	switch parsed.Op {
	case "navigate":
		return chromedp.Run(ctx, chromedp.Navigate(parsed.Value))
	case "reload":
		return chromedp.Run(ctx, chromedp.Reload())
	case "click":
		if err := e.executeClick(ctx, resolvedRef, seeToClick, elementMode); err != nil {
			return err
		}
	case "clickxy":
		if err := e.executeClickXY(ctx, parsed.CoordX, parsed.CoordY, seeToClick); err != nil {
			return err
		}
	case "dblclickxy":
		if err := e.executeDoubleClickXY(ctx, parsed.CoordX, parsed.CoordY); err != nil {
			return err
		}
	case "dragxy":
		if err := e.executeDragXY(ctx, parsed.CoordX, parsed.CoordY, parsed.CoordX2, parsed.CoordY2); err != nil {
			return err
		}
	case "clickat":
		if err := e.executeClickAt(ctx, resolvedRef, parsed.CoordX, parsed.CoordY); err != nil {
			return err
		}
	case "tapxy":
		if err := e.executeTapXY(ctx, parsed.CoordX, parsed.CoordY); err != nil {
			return err
		}
	case "typetext":
		if err := e.executeTypeText(ctx, parsed.Value, strictHuman); err != nil {
			return err
		}
	case "zoom":
		if err := e.executeZoom(ctx, parsed.Value); err != nil {
			return err
		}
	case "keyboard":
		if err := e.executeKeyboard(ctx, parsed.Value); err != nil {
			return err
		}
	case "dblclickat":
		if err := e.executeDoubleClickAt(ctx, resolvedRef, parsed.CoordX, parsed.CoordY); err != nil {
			return err
		}
	case "rclickat":
		if err := e.executeRightClickAt(ctx, resolvedRef, parsed.CoordX, parsed.CoordY); err != nil {
			return err
		}
	case "hoverat":
		if err := e.executeHoverAt(ctx, resolvedRef, parsed.CoordX, parsed.CoordY); err != nil {
			return err
		}
	case "dragat":
		if err := e.executeDragAt(ctx, resolvedRef, parsed.CoordX, parsed.CoordY, parsed.CoordX2, parsed.CoordY2); err != nil {
			return err
		}
	case "wheelat":
		if err := e.executeWheelAt(ctx, resolvedRef, parsed.CoordX, parsed.CoordY, parsed.DeltaX, parsed.DeltaY, parsed.Steps); err != nil {
			return err
		}
	case "swipeat":
		if err := e.executeSwipeAt(ctx, resolvedRef, parsed.CoordX, parsed.CoordY, parsed.CoordX2, parsed.CoordY2); err != nil {
			return err
		}
	case "tap":
		if err := e.executeTap(ctx, resolvedRef); err != nil {
			return err
		}
	case "tapat":
		if err := e.executeTapAt(ctx, resolvedRef, parsed.CoordX, parsed.CoordY); err != nil {
			return err
		}
	case "fill":
		state, stateErr := e.editableStateForActionTarget(ctx, resolvedRef)
		if stateErr != nil {
			return e.failedBeforeDispatch(stateErr)
		}
		if kind := temporalInputKind(state.Tag, state.InputType); kind != "" {
			if !elementMode || e.interactionPolicy.Fidelity != InteractionFidelityDual {
				return e.failedBeforeDispatch(fmt.Errorf("%w: temporal input requires explicit element mode in baseline; no deletion dispatched", ErrActFailed))
			}
			return e.executeElementTemporal(ctx, &ElementRef{Ref: resolvedRef}, resolvedRef, kind, parsed.Value)
		}

		if strictHuman {
			err = e.executeStrictHumanText(ctx, resolvedRef, parsed.Value, true, false)
		} else {
			err = e.executeFill(ctx, resolvedRef, parsed.Value)
		}
		if err != nil {
			return err
		}
	case "fillsecret":
		state, stateErr := e.editableStateForActionTarget(ctx, resolvedRef)
		if stateErr != nil {
			return e.failedBeforeDispatch(stateErr)
		}
		if err := validateMaskedSensitiveDestination(state); err != nil {
			return e.failedBeforeDispatch(err)
		}
		err = e.executeStrictHumanText(ctx, resolvedRef, parsed.Value, true, true)
		if err != nil {
			return err
		}
	case "type":
		state, stateErr := e.editableStateForActionTarget(ctx, resolvedRef)
		if stateErr != nil {
			return e.failedBeforeDispatch(stateErr)
		}
		if temporalInputKind(state.Tag, state.InputType) != "" {
			return e.failedBeforeDispatch(fmt.Errorf("%w: temporal insertion unavailable; use explicit baseline element fill", ErrActFailed))
		}
		if strictHuman {
			err = e.executeStrictHumanText(ctx, resolvedRef, parsed.Value, false, false)
		} else {
			err = e.executeType(ctx, resolvedRef, parsed.Value)
		}
		if err != nil {
			return err
		}
	case "press":
		if strictHuman {
			err = e.executeStrictHumanPress(ctx, resolvedRef, parsed.Value)
		} else {
			err = e.executePress(ctx, resolvedRef, parsed.Value)
		}
		if err != nil {
			return err
		}
	case "hover":
		if err := e.executeHover(ctx, resolvedRef); err != nil {
			return err
		}
	case "scroll":
		if err := e.executeScroll(ctx, resolvedScrollTarget, parsed.Direction, parsed.Steps, seeToClick); err != nil {
			return err
		}
	case "select":
		if err := e.executeSelect(ctx, resolvedRef, parsed.Option, elementMode && e.interactionPolicy.Fidelity == InteractionFidelityDual); err != nil {
			return err
		}
	case "back":
		if err := e.executeBack(ctx); err != nil {
			return err
		}
	case "forward":
		if err := e.executeForward(ctx); err != nil {
			return err
		}
	case "focus":
		if strictHuman {
			err = e.executeStrictHumanFocus(ctx, resolvedRef)
		} else {
			err = e.executeFocusSelector(ctx, resolvedRef)
		}
		if err != nil {
			return err
		}
	case "scrollinto":
		if err := e.executeScrollIntoView(ctx, resolvedRef); err != nil {
			return err
		}
	case "scrollto":
		if err := e.executeScrollIntoView(ctx, resolvedRef); err != nil {
			return err
		}
	case "check":
		if err := e.executeCheck(ctx, resolvedRef, seeToClick, elementMode); err != nil {
			return err
		}
	case "uncheck":
		if err := e.executeUncheck(ctx, resolvedRef, seeToClick, elementMode); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: unsupported op %q", ErrActFailed, parsed.Op)
	}
	return nil
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

var scrollIntoViewOps = map[string]bool{"scrollinto": true, "scrollto": true}

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
	return e.resolveSemanticSelectorForModeOp(selector, sessionMode, elementMode, "")
}

// resolveSemanticSelectorForModeOp threads the action op so view-only gestures
// (scrollinto/scrollto) may use the DOM fallback even in see-to-click mode.
func (e *actionEngine) resolveSemanticSelectorForModeOp(selector string, sessionMode, elementMode bool, op string) (string, error) {
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
		refKey := sel.Raw
		if _, ok := e.snapEngine.LookupRef(refKey); !ok {
			return "", fmt.Errorf("%w: %s not found in current observation (run observe --id <id> first)", ErrRefNotFound, refKey)
		}
		if meta, ok := e.snapEngine.LookupRefMeta(refKey); !ok || !meta.Observed || !meta.VisibleInViewport {
			return "", fmt.Errorf("%w: ref is no longer authorized; observe before input", ErrRefNotFound)
		}
		// Return as internal ref string so executeClick etc. can use it
		return refKey, nil
	}

	if elementMode {
		return e.resolveElementModeSelector(sel)
	}

	currentOp := op
	switch sel.SType {
	case SelectorTestID:
		meta, ok := e.snapEngine.LookupByTestID(sel.TestID)
		if ok && (meta.BackendNodeID != 0 || e.seeToClick) {
			return meta.Ref, nil
		}
		// scrollinto/scrollto are view-only gestures (no authority mutation):
		// an a11y-clipped target may still be scrolled into view by its DOM
		// selector. The fresh observation after the scroll is what grants the
		// element into the visible set — the fallback mints no capability.
		if e.seeToClick && scrollIntoViewOps[currentOp] {
			return `[data-testid="` + sel.TestID + `"]`, nil
		}
		if e.seeToClick {
			return "", e.failedBeforeDispatch(fmt.Errorf("%w: #%s is not an actionable element from the most recent visible observation — scroll and observe again", ErrRefNotFound, sel.TestID))
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
	for _, m := range matches {
		if m.RecommendedLocator != "" {
			suggestions = append(suggestions, fmt.Sprintf("  - %s", m.RecommendedLocator))
		} else {
			suggestions = append(suggestions, fmt.Sprintf("  - %s", m.Ref))
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
	if strings.HasPrefix(ref, "@") {
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

// ensureTargetFrontmost 保证当前 page target 在浏览器前台，返回是否发生了真实切换。
//
// 这是"后台 target 吞输入"的根因修复：后台 target 的鼠标/滚轮/hover 事件不会被
// 处理，Input.dispatchMouseEvent 的 ack 永远不来（hover 实测挂满 5s 才超时返回）。
// v0.11.3 只在滚轮路径修了这一处；现在它上提到动作派发的公共入口，所有真实输入
// 动作共用同一次前置。
//
// 与 v0.11.3 的两点差别：
//   - 先探一次 visibilityState 再决定要不要切。这既省掉"已经在前台还发一遍"的
//     无谓往返，也让"真的发生了切换"成为可上报的事实（brought_to_front）。
//   - 失败 fail loud，不再只 WARN。理由是它已经不再是滚轮路径上的尽力而为前置，
//     而是所有输入动作的公共前提：前提没摆正就派发，等来的只会是那个我们刚刚
//     根治掉的静默挂死。宁可这一步明确报错，也不要把失败推迟成 ack 超时。
func ensureTargetFrontmost(ctx context.Context) (bool, error) {
	if targetLooksFrontmost(ctx) {
		return false, nil
	}
	acked, err := dispatchInputWithAckBudget(ctx, targetActivationBudget,
		chromedp.ActionFunc(func(ctx context.Context) error {
			return page.BringToFront().Do(ctx)
		}))
	if err != nil {
		return false, fmt.Errorf("%w: bring target to front before input dispatch: %v", ErrActFailed, err)
	}
	if !acked {
		return false, fmt.Errorf("%w: bring target to front did not return within %s;"+
			" target activation is unconfirmed and no input was dispatched",
			ErrActFailed, targetActivationBudget)
	}
	return true, nil
}

// targetLooksFrontmost 探一次"这个 target 现在收不收得到输入"。
//
// 用 visibilityState 而不是 hasFocus()：前者精确对应"这个 tab 是不是它那个窗口的
// 当前 tab"，也就是 CDP 输入能不能被路由过来的那个条件；hasFocus() 还掺进了 OS
// 级窗口焦点，而窗口焦点不是 bringToFront 能保证的东西，拿它当判据会让我们在
// 明明已经修好的情况下反复空切。
//
// 探针本身失败(没有可用的 CDP context / 超预算)时返回 false：宁可多切一次前台
// (幂等)，也不要凭猜测跳过前置。
func targetLooksFrontmost(ctx context.Context) bool {
	var visible bool
	acked, err := dispatchInputWithAckBudget(ctx, targetActivationBudget,
		chromedp.ActionFunc(func(ctx context.Context) error {
			return chromedp.Evaluate(`document.visibilityState === 'visible'`, &visible).Do(ctx)
		}))
	if err != nil || !acked {
		return false
	}
	return visible
}

// currentTargetID 读当前 chromedp target 的 ID，只用于前台切换账本的记账。
// 拿不到就返回空串 —— 账本少一条身份不影响输入本身。
func currentTargetID(ctx context.Context) string {
	c := chromedp.FromContext(ctx)
	if c == nil || c.Target == nil {
		return ""
	}
	return string(c.Target.TargetID)
}

// enterInputDispatchWindow 进入一次输入派发窗口：取浏览器级输入临界区 →
// 把自己的 target 提到前台 → 把控制权交回给调用方去真正派发。
//
// 返回的 release 必须在派发（含等 ack）结束后立刻调用：临界区只覆盖
// bringToFront→dispatch→ack 这一段，毫秒级。DOM settle、快照这些不占前台归属的
// 步骤刻意留在临界区之外，否则两个操作者会在对方的 5s settle 上排队。
func (e *actionEngine) enterInputDispatchWindow(ctx context.Context) (release func(), brought bool, contention string, err error) {
	return e.enterInputDispatchWindowForRequest(ctx, true, "", true)
}

func (e *actionEngine) enterInputDispatchWindowForRequest(ctx context.Context, needsGeometry bool, captureID string, activate bool) (release func(), brought bool, contention string, err error) {
	lock, lockErr := AcquireBrowserInputLock(e.browserInputKey, DefaultBrowserInputLockWait)
	if lockErr != nil {
		return func() {}, false, "", fmt.Errorf("%w: %v", ErrActFailed, lockErr)
	}
	release = func() { lock.Release() }
	if e.authorityCheck != nil {
		if err := e.authorityCheck(ctx, needsGeometry, captureID); err != nil {
			release()
			return func() {}, false, "", &ActionStageError{Phase: "authority", State: "not-dispatched", Cause: err}
		}
	}

	if e.authorityCheck == nil && needsGeometry {
		if err := e.snapEngine.validateObservationAuthority(ctx, captureID); err != nil {
			release()
			return func() {}, false, "", err
		}
	}

	if !activate {
		return release, false, "", nil
	}
	activationFn := e.targetActivation
	if activationFn == nil {
		activationFn = ensureTargetFrontmost
	}
	brought, err = activationFn(ctx)
	if err != nil {
		release()
		return func() {}, false, "", err
	}
	if brought {
		contention = RecordBringToFront(e.browserInputKey, currentTargetID(ctx))
		if contention != "" {
			logger.Warn(inputLogContext(), "front-most target is flipping between operators", "detail", contention)
		}
	}
	return release, brought, contention, nil
}

// inputDispatchOps 是"会向页面派发真实输入"的动作集合 —— 也就是必须先确保
// target 在前台的那些。导航/纯读类动作(back/forward/scrollinto/zoom/keyboard)
// 不派发指针或键盘事件，不需要前台；仍与所有动作共用控制权/运行时临界区。
var inputDispatchOps = map[string]bool{
	"click": true, "clickxy": true, "clickat": true,
	"dblclickat": true, "dblclickxy": true, "rclickat": true,
	"tap": true, "tapat": true, "tapxy": true,
	"hover": true, "hoverat": true,
	"wheelat": true, "scroll": true,
	"drag": true, "dragat": true, "dragxy": true, "swipeat": true,
	"press": true, "fill": true, "fillsecret": true,
	"type": true, "typetext": true, "select": true,
	"check": true, "uncheck": true, "focus": true,
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

// dispatchMouseWheelSteps 派发一次滚轮手势：把指针移到落点，再按 runWheelGesture
// 的节奏发出 steps 个脉冲。target 前台由公共入口保证（见 enterInputDispatchWindow）。
// 返回 acked=false 表示手势发完时渲染器还没 ack。
func dispatchMouseWheelSteps(ctx context.Context, x, y, deltaX, deltaY float64, steps int) (bool, error) {
	if steps <= 0 {
		steps = 1
	}
	// 前台确保已上提到 executeWithInteractionMode 的输入派发公共入口，这里不再重复
	// 调用 —— 单一调用点，避免同一次动作里两次 bringToFront。
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

// executeDoubleClickXY 对视口 CSS 像素坐标执行真实鼠标左键**双击**。
// 与 executeClickXY 同一套前置: 视口越界拒发、guardPoint、命中回报 (坐标动作绕过 a11y
// 模型但不绕过证据)。clickCount=2 由 CDP 携带, 页面收到的是真正的 dblclick。
func (e *actionEngine) executeDoubleClickXY(ctx context.Context, x, y float64) error {
	if err := e.guardViewportPoint(ctx, "double-click", x, y); err != nil {
		return err
	}
	e.recordCoordinateHit(ctx, x, y)
	return dispatchMouseClick(ctx, x, y, input.Left, 2)
}

// executeDragXY 在视口 CSS 像素坐标之间执行真实鼠标拖拽 (press → 插值 move → release)。
// 元素内相对版是 executeDragAt; 这里是免 selector 的视口版 —— canvas 上"从某个像素拖到
// 某个像素"是唯一可行的表达 (拖拽起点常常是个不进 a11y 树的把手)。
func (e *actionEngine) executeDragXY(ctx context.Context, x1, y1, x2, y2 float64) error {
	if err := e.guardViewportPoint(ctx, "drag start", x1, y1); err != nil {
		return err
	}
	if err := e.guardViewportPoint(ctx, "drag end", x2, y2); err != nil {
		return err
	}
	e.recordCoordinateHit(ctx, x1, y1)
	return dispatchMouseDrag(ctx, x1, y1, x2, y2, mouseDragInterpolationSteps)
}

// guardViewportPoint 复用 clickxy 的前置校验: 越界拒发 + 遮罩/门控守卫。
func (e *actionEngine) guardViewportPoint(ctx context.Context, what string, x, y float64) error {
	w, h, err := e.viewportSize(ctx)
	if err != nil {
		return err
	}
	if x < 0 || y < 0 || x >= w || y >= h {
		return e.failedBeforeDispatch(fmt.Errorf("%w: %s point (%.1f,%.1f) is outside the %.1fx%.1f CSS-pixel viewport", ErrActFailed, what, x, y, w, h))
	}
	return e.guardPoint(ctx, x, y)
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
func (e *actionEngine) executeTypeText(ctx context.Context, text string, physical bool) error {
	if physical {
		backend := e.currentHumanFocus()
		if backend == 0 {
			return fmt.Errorf("%w: focused type requires a verified physical focus", ErrActFailed)
		}
		meta := &ElementRef{BackendNodeID: backend}
		state, err := strictEditableStateForMeta(ctx, meta)
		if err != nil {
			return err
		}
		if !state.Focused || !state.Editable || state.Disabled || state.ReadOnly {
			return fmt.Errorf("%w: focused element is not editable", ErrActFailed)
		}
		if state.Password {
			return ErrPasswordField
		}
		if err := insertTextPerCharacter(ctx, text); err != nil {
			return &ActionStageError{Phase: "insert", State: "partial", Cause: err}
		}
		if err := waitForStrictEditableValue(ctx, meta, state.Prefix+text+state.Suffix); err != nil {
			return &ActionStageError{Phase: "verify", State: "partial", Cause: err}
		}
		return nil
	}

	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		return input.InsertText(text).Do(ctx)
	}))
}

// executeDoubleClickAt 对目标元素相对坐标执行真实鼠标左键双击（Univer 单元格编辑 / 缩放复位）。
func (e *actionEngine) executeDoubleClickAt(ctx context.Context, ref string, relX, relY float64) error {
	x, y, err := e.resolveCoordinateActionPoint(ctx, "executeDoubleClickAt", ref, relX, relY)
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
	x, y, err := e.resolveCoordinateActionPoint(ctx, "executeRightClickAt", ref, relX, relY)
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
	x, y, err := e.resolveCoordinateActionPoint(ctx, "executeTapAt", ref, relX, relY)
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
	if ref == "" {
		x1, y1, err := e.resolveCoordinateActionPoint(ctx, "executeDragAt", ref, relX1, relY1)
		if err != nil {
			return err
		}
		x2, y2, err := e.resolveCoordinateActionPoint(ctx, "executeDragAt", ref, relX2, relY2)
		if err != nil {
			return err
		}
		return dispatchMouseDrag(ctx, x1, y1, x2, y2, mouseDragInterpolationSteps)
	}
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
	if ref == "" {
		x1, y1, err := e.resolveCoordinateActionPoint(ctx, "executeSwipeAt", ref, relX1, relY1)
		if err != nil {
			return err
		}
		x2, y2, err := e.resolveCoordinateActionPoint(ctx, "executeSwipeAt", ref, relX2, relY2)
		if err != nil {
			return err
		}
		return dispatchTouchSwipe(ctx, x1, y1, x2, y2, mouseDragInterpolationSteps)
	}
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
	InputType      string `json:"input_type"`
	TextSecurity   string `json:"text_security"`
	Value          string `json:"value"`
	Prefix         string `json:"prefix"`
	Suffix         string `json:"suffix"`
	SelectionKnown bool   `json:"selection_known"`
	Disabled       bool   `json:"disabled"`
	ReadOnly       bool   `json:"readonly"`
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
		focused, editable, password, tag, input_type:type, text_security:String(getComputedStyle(el).webkitTextSecurity||getComputedStyle(el).getPropertyValue("-webkit-text-security")||"none"), value, disabled: !!el.disabled, readonly: !!el.readOnly, selection_known: selectionKnown,
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

func dispatchStrictSelectAll(ctx context.Context) error {
	modifier := input.ModifierCtrl
	if goruntime.GOOS == "darwin" {
		modifier = input.ModifierMeta
	}
	return chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		if err := input.DispatchKeyEvent(input.KeyDown).WithKey("a").WithCode("KeyA").WithWindowsVirtualKeyCode(65).WithModifiers(modifier).WithCommands([]string{"selectAll"}).Do(c); err != nil {
			return err
		}
		return input.DispatchKeyEvent(input.KeyUp).WithKey("a").WithCode("KeyA").WithWindowsVirtualKeyCode(65).WithModifiers(modifier).Do(c)
	}))
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

	preflight, err := strictEditableStateForMeta(ctx, meta)
	if err != nil {
		return err
	}
	if err := validatePhysicalTextDestination(preflight, allowPassword); err != nil {
		return e.failedBeforeDispatch(err)
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
	if !state.Editable || state.Disabled || state.ReadOnly {
		return fmt.Errorf("%w: target %q is not an editable INPUT/TEXTAREA", ErrActFailed, ref)
	}
	if state.Password && !allowPassword {
		return ErrPasswordField
	}

	want := state.Prefix + value + state.Suffix
	path := []string{"mouse_click", "active_element_verified"}
	if clear {
		if err := dispatchStrictSelectAll(ctx); err != nil {
			return fmt.Errorf("%w: clear %q through keyboard: %v", ErrActFailed, ref, err)
		}
		selected, selectErr := strictEditableStateForMeta(ctx, meta)
		if selectErr != nil || !selected.Focused || !selected.SelectionKnown || selected.Value != state.Value || selected.Prefix != "" || selected.Suffix != "" {
			return &ActionStageError{Phase: "selection", State: "dispatched", Cause: fmt.Errorf("%w: full selection was not acquired; no deletion dispatched", ErrActFailed)}
		}
		if err := chromedp.Run(ctx, chromedp.KeyEvent(mapKeyName("Backspace"))); err != nil {
			return &ActionStageError{Phase: "delete", State: "unknown", Cause: err}
		}
		if err := waitForStrictEditableValue(ctx, meta, ""); err != nil {
			return &ActionStageError{Phase: "delete", State: "partial", Cause: err}
		}
		want = value
		path = append(path, "keyboard_select_all", "keyboard_backspace")
	}
	if err := insertTextPerCharacter(ctx, value); err != nil {
		return &ActionStageError{Phase: "insert", State: "partial", Cause: fmt.Errorf("%w: text insertion failed: %v", ErrActFailed, err)}
	}
	if err := waitForStrictEditableValue(ctx, meta, want); err != nil {
		return &ActionStageError{Phase: "verify", State: "partial", Cause: err}
	}
	path = append(path, "Input.insertText_per_character", "value_verified")
	e.updateActionFidelity(func(report *ActionFidelityReport) {
		report.HumanPath = path
	})
	return nil
}

// temporalInputKind reports the temporal input family ("" = not temporal).
func temporalInputKind(tag, inputType string) string {
	if tag != "INPUT" {
		return ""
	}
	switch inputType {
	case "date", "time", "month", "week", "datetime-local":
		return inputType
	}
	return ""
}

var temporalValuePatterns = map[string]*regexp.Regexp{
	"date":           regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`),
	"time":           regexp.MustCompile(`^\d{2}:\d{2}(:\d{2})?$`),
	"month":          regexp.MustCompile(`^\d{4}-\d{2}$`),
	"week":           regexp.MustCompile(`^\d{4}-W\d{2}$`),
	"datetime-local": regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?$`),
}

// normalizeTemporalValue accepts the control's canonical form, plus the
// digits-only spelling for date (20260828) and month (202608) — what a human
// would type — and returns the canonical value= form.
func normalizeTemporalValue(kind, raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if kind == "date" && regexp.MustCompile(`^\d{8}$`).MatchString(v) {
		v = v[0:4] + "-" + v[4:6] + "-" + v[6:8]
	}
	if kind == "month" && regexp.MustCompile(`^\d{6}$`).MatchString(v) {
		v = v[0:4] + "-" + v[4:6]
	}
	if kind == "time" && regexp.MustCompile(`^\d{4}$`).MatchString(v) {
		v = v[0:2] + ":" + v[2:4]
	}
	pattern, ok := temporalValuePatterns[kind]
	if !ok || !pattern.MatchString(v) {
		return "", fmt.Errorf("%w: %s input expects e.g. %q, got %q", ErrActFailed, kind, temporalValueExample(kind), raw)
	}
	return v, nil
}

func temporalValueExample(kind string) string {
	switch kind {
	case "date":
		return "2026-08-28"
	case "time":
		return "18:30"
	case "month":
		return "2026-08"
	case "week":
		return "2026-W35"
	case "datetime-local":
		return "2026-08-28T18:30"
	}
	return ""
}

// executeElementTemporal is available only to explicit element/baseline operations.
// It fills a temporal input through the native value
// setter (prototype setter, so framework value-tracking like Vue's v-model
// still sees the input event) and verifies the canonical value round-trips.
// Focus was already acquired by a real click in executeStrictHumanText.
func (e *actionEngine) executeElementTemporal(ctx context.Context, meta *ElementRef, ref, kind, raw string) error {
	state, stateErr := e.editableStateForActionTarget(ctx, ref)
	if stateErr != nil {
		return stateErr
	}
	if state.Disabled || state.ReadOnly {
		return fmt.Errorf("%w: temporal target is not editable", ErrActFailed)
	}
	want, err := normalizeTemporalValue(kind, raw)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(want)
	if err != nil {
		return fmt.Errorf("%w: temporal value encode: %v", ErrActFailed, err)
	}
	fn := fmt.Sprintf(`function() {
		const v = %s;
		const desc = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value');
		desc.set.call(this, v);
		this.dispatchEvent(new Event('input', {bubbles: true}));
		this.dispatchEvent(new Event('change', {bubbles: true}));
		return String(this.value ?? '');
	}`, string(encoded))
	var got string
	err = e.withResolvedActionElement(ctx, ref, func(execCtx context.Context, obj *runtime.RemoteObject) error {
		result, exc, err := runtime.CallFunctionOn(fn).
			WithObjectID(obj.ObjectID).
			WithReturnByValue(true).
			Do(execCtx)
		if err != nil {
			return err
		}
		if exc != nil {
			return fmt.Errorf("temporal value set failed: %s", exc.Text)
		}
		if result == nil || len(result.Value) == 0 {
			return fmt.Errorf("temporal value set returned no value")
		}
		return json.Unmarshal(result.Value, &got)
	})
	if err != nil {
		return fmt.Errorf("%w: fill %s %q: %v", ErrActFailed, kind, ref, err)
	}
	if got != want {
		// The control rejected the value (min/max/step) — report what stuck.
		return fmt.Errorf("%w: %s input rejected %q (value now %q)", ErrActFailed, kind, want, got)
	}
	e.updateActionFidelity(func(report *ActionFidelityReport) {
		report.Synthetic = true
		report.SyntheticNote = "temporal <input type=" + kind + "> uses native value set plus synthetic input/change events (segment keystroke order is locale-dependent); explicit element mode is functional evidence, not evidence a human can operate the picker"
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
		// Browser-chrome chords act on the browser, not the DOM: Ctrl/Cmd+R is
		// the human reload gesture. A raw CDP key event never reaches Chrome's
		// UI accelerators, so it must be executed as a real navigation reload.
		if isReloadChord(mods, baseKey) {
			return chromedp.Run(ctx, chromedp.Evaluate(`location.reload()`, nil))
		}
		return dispatchModifierCombo(ctx, mods, baseKey)
	}
	// 功能键/方向键/导航键: rune 通道发不出正确的 key (见 needsPreciseKeyDispatch)。
	if canonical := canonicalKeyName(key); needsPreciseKeyDispatch(canonical) {
		return dispatchPlainKey(ctx, canonical)
	}
	return chromedp.Run(ctx, chromedp.KeyEvent(mapKeyName(key)))
}

// isReloadChord reports whether a modifier combo is the browser reload
// gesture (Ctrl+R / Cmd+R; F5 has no modifier and stays a DOM key).
func isReloadChord(mods input.Modifier, baseKey string) bool {
	key := strings.ToLower(strings.TrimSpace(baseKey))
	if key != "r" {
		return false
	}
	return mods&(input.ModifierCtrl|input.ModifierMeta) != 0
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

// executePress 执行按键操作，支持组合键（Ctrl+A, Shift+Enter 等）。
func (e *actionEngine) executePress(ctx context.Context, ref string, key string) error {
	// Baseline targeted press may focus the target, but every key is native.
	// Native SELECT has no setter/event-injection exception.
	if ref != "" {
		if err := e.withResolvedActionElement(ctx, ref, func(execCtx context.Context, obj *runtime.RemoteObject) error {
			return dom.Focus().WithObjectID(obj.ObjectID).Do(execCtx)
		}); err != nil {
			return err
		}
	}

	// Modifier combos (Ctrl+B, Shift+Enter, Control+Shift+1, ...) must be
	// dispatched as a real CDP keyDown/keyUp pair carrying the Modifiers
	// bitmask. chromedp.KeyEvent encodes Selenium-style PUA modifier runes
	// ( ...) as *unknown printable chars* — Chrome inserts them as text
	// and never sets ctrlKey/metaKey, so "press Control+b" used to insert a
	// literal 'b'. We bypass that path entirely for combos.
	if mods, baseKey, hasMod := parseKeyCombo(key); hasMod {
		// Browser-chrome chords act on the browser, not the DOM: Ctrl/Cmd+R is
		// the human reload gesture — raw CDP key events never reach Chrome's UI
		// accelerators, so execute it as a real navigation reload.
		if isReloadChord(mods, baseKey) {
			return chromedp.Run(ctx, chromedp.Evaluate(`location.reload()`, nil))
		}
		return dispatchModifierCombo(ctx, mods, baseKey)
	}

	// 功能键/方向键/导航键走精确派发 —— 同一条 PUA 码点毛病对单键同样成立
	// (见 needsPreciseKeyDispatch 头注)。
	if canonical := canonicalKeyName(key); needsPreciseKeyDispatch(canonical) {
		return dispatchPlainKey(ctx, canonical)
	}

	// Map key name to chromedp SendKeys string (single key / plain text path —
	// behavior unchanged).
	keyStr := mapKeyName(key)
	return chromedp.Run(ctx, chromedp.KeyEvent(keyStr))
}

// needsPreciseKeyDispatch 报告某个规范化键名是否**必须**走 CDP 的显式 key/code/virtualKeyCode
// 派发, 而不能走 chromedp.KeyEvent 的 rune 通道。
//
// 根因与修饰符那条链同源 (见 executePress 里的注释): mapKeyName 给功能键/方向键/导航键映射的是
// **Selenium 私有区码点** (ArrowUp=\ue012 等), 而底层跑的是 chromedp —— 它的键表里没有这些码点,
// 于是发出去的 KeyboardEvent 的 key 是 "Unidentified"。实测 (2026-08-20, probe 页 window keydown
// 记录器): Enter/Escape/Delete/Backspace/Tab/字母 全部正确 (它们映射到的是**真实控制字符**
// \r \u001b \u007f \u0008 \t, chromedp 认得), 而 F1–F12、Arrow*、Home/End/PageUp/PageDown、
// Insert **全部落成 Unidentified** —— 页面上任何 `e.key === 'F2'` / `'ArrowRight'` 的处理器都收不到。
//
// 作者当初为修饰符组合发现过同一个毛病并绕开了, 但单键这条对称路径漏了。这里只接管**确实坏掉的**
// 那批键: 已经正常工作的 (Enter/Tab/Space/Escape/Backspace/Delete) 一律不动 —— 它们依赖 rune 通道
// 附带的 char 事件产生默认编辑行为 (Space toggle checkbox、Enter 在 textarea 里换行), 改道有回归风险。
func needsPreciseKeyDispatch(canonical string) bool {
	switch canonical {
	case "ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight",
		"Home", "End", "PageUp", "PageDown", "Insert":
		return true
	}
	upper := strings.ToUpper(canonical)
	if len(upper) >= 2 && upper[0] == 'F' {
		if _, isFn := keyVirtualCodeMap[upper]; isFn {
			return true
		}
	}
	return false
}

// dispatchPlainKey 以 mods=0 走与修饰符组合完全相同的精确派发路径。
func dispatchPlainKey(ctx context.Context, canonical string) error {
	return dispatchModifierCombo(ctx, 0, canonical)
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

// dispatchHumanMouseWheel 走人类输入网关派发滚轮手势。target 前台由公共入口保证
// （后台 target 会把滚轮事件整条吞掉，见 wheelGestureAckBudget 处的根因说明），
// 这里按 runWheelGesture 的节奏发脉冲，等 ack 的时间有硬上界。
func (e *actionEngine) dispatchHumanMouseWheel(ctx context.Context, x, y, deltaY float64, steps int) error {
	if e.humanInput == nil {
		e.humanInput = NewInputGateway(nil, nil)
	}
	if steps <= 0 {
		steps = 1
	}
	// 前台确保已上提到输入派发公共入口（见 enterInputDispatchWindow），此处不双调。
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

func validateMaskedSensitiveDestination(state strictEditableState) error {
	if state.Tag != "INPUT" || state.InputType != "password" || state.TextSecurity == "none" || state.TextSecurity == "" {
		return fmt.Errorf("%w: sensitive fill requires a visually masked password input; no deletion dispatched", ErrActFailed)
	}
	if state.Disabled || state.ReadOnly {
		return fmt.Errorf("%w: sensitive destination is not editable", ErrActFailed)
	}
	return nil
}
func validatePhysicalTextDestination(state strictEditableState, sensitive bool) error {
	if sensitive {
		return validateMaskedSensitiveDestination(state)
	}
	if temporalInputKind(state.Tag, state.InputType) != "" {
		return fmt.Errorf("%w: temporal input requires explicit element mode in a baseline scenario; no deletion dispatched", ErrActFailed)
	}
	return nil
}
func (e *actionEngine) editableStateForActionTarget(ctx context.Context, ref string) (strictEditableState, error) {
	var state strictEditableState
	err := e.withResolvedActionElement(ctx, ref, func(c context.Context, obj *runtime.RemoteObject) error {
		result, exception, err := runtime.CallFunctionOn(strictEditableStateFunction).WithObjectID(obj.ObjectID).WithReturnByValue(true).Do(c)
		if err != nil {
			return err
		}
		if exception != nil || result == nil {
			return fmt.Errorf("editable metadata unavailable")
		}
		return json.Unmarshal(result.Value, &state)
	})
	return state, err
}
