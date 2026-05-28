package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// ============================================================
// § ActionEngine [Ref: CAP-BS09-C2, T5-B5]
// ============================================================

// actionEngine 实现操作解析与执行。
type actionEngine struct {
	snapEngine *snapshotEngine
}

// newActionEngine 创建 ActionEngine 实例。
func newActionEngine(snapEngine *snapshotEngine) *actionEngine {
	return &actionEngine{snapEngine: snapEngine}
}

// ParsedAction 是解析后的操作结构。
type ParsedAction struct {
	Op     string  // "click" | "clickat" | "tap" | "tapat" | "type" | "scroll" | "hover" | "select"
	Ref    string  // Element Ref（如 "e3"）或语义选择器（如 "#testid", "button:'name'"）
	Value  string  // type/select 的值
	CoordX float64 // clickat 的相对 X 坐标（0..1）
	CoordY float64 // clickat 的相对 Y 坐标（0..1）
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
//   - "clickat #canvas 92% 8%" — 对元素相对坐标执行真实鼠标点击
//   - "tap button:'接管'" | "tapat #browser-liveview 92% 8%" — 对元素执行真实触控点击
//   - "fill #input 'text'" — 清空后输入
//   - "type textbox:'名称' 'hello'"
//   - "press Enter" | "press Ctrl+A" | "press #btn Ctrl+K"
//   - "hover button:'名称'"
//   - "scroll down" | "scroll up"
//   - "select e4 'opt2'"
//   - "back" | "forward"
//   - "focus #selector"
//   - "scrollinto #selector"
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
	switch op {
	case "click", "tap", "hover", "focus", "scrollinto", "check", "uncheck":
		if len(parts) < 2 {
			return nil, fmt.Errorf("%w: %s requires selector argument", ErrActFailed, op)
		}
		return &ParsedAction{Op: op, Ref: parts[1]}, nil

	case "clickat":
		if len(parts) < 4 {
			return nil, fmt.Errorf("%w: clickat requires selector, x, and y", ErrActFailed)
		}
		x, err := parseNormalizedCoordinate(parts[2])
		if err != nil {
			return nil, err
		}
		y, err := parseNormalizedCoordinate(parts[3])
		if err != nil {
			return nil, err
		}
		return &ParsedAction{Op: "clickat", Ref: parts[1], CoordX: x, CoordY: y}, nil

	case "tapat":
		if len(parts) < 4 {
			return nil, fmt.Errorf("%w: tapat requires selector, x, and y", ErrActFailed)
		}
		x, err := parseNormalizedCoordinate(parts[2])
		if err != nil {
			return nil, err
		}
		y, err := parseNormalizedCoordinate(parts[3])
		if err != nil {
			return nil, err
		}
		return &ParsedAction{Op: "tapat", Ref: parts[1], CoordX: x, CoordY: y}, nil

	case "fill":
		if len(parts) < 3 {
			return nil, fmt.Errorf("%w: fill requires selector and value", ErrActFailed)
		}
		value, quoted := extractQuotedValue(action, parts[1])
		if !quoted {
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
			value = strings.Join(parts[2:], " ")
		}
		return &ParsedAction{Op: "type", Ref: parts[1], Value: value}, nil

	case "press":
		// press <key>  OR  press <selector> <key>
		if len(parts) < 2 {
			return nil, fmt.Errorf("%w: press requires key argument", ErrActFailed)
		}
		if len(parts) == 2 {
			// press <key> — page-level
			return &ParsedAction{Op: "press", Ref: "", Value: parts[1]}, nil
		}
		// press <selector> <key>
		return &ParsedAction{Op: "press", Ref: parts[1], Value: parts[2]}, nil

	case "scroll":
		if len(parts) < 2 {
			return nil, fmt.Errorf("%w: scroll requires direction or ref", ErrActFailed)
		}
		dir := strings.ToLower(parts[1])
		return &ParsedAction{Op: "scroll", Ref: dir}, nil

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
		return &ParsedAction{Op: "back"}, nil

	case "forward":
		return &ParsedAction{Op: "forward"}, nil

	default:
		return nil, fmt.Errorf("%w: unknown operation %q", ErrActFailed, op)
	}
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
	parsed, err := ParseAction(action)
	if err != nil {
		return nil, err
	}

	// page-level 操作不需要选择器
	noSelectorOps := map[string]bool{"scroll": true, "back": true, "forward": true}

	var resolvedRef string
	if !noSelectorOps[parsed.Op] && parsed.Ref != "" {
		ref, err := e.resolveSemanticSelectorWithSession(parsed.Ref, sessionMode)
		if err != nil {
			return nil, err
		}
		resolvedRef = ref
	} else {
		resolvedRef = parsed.Ref
	}

	// 直接使用调用方 context（不派生子 context）。
	// 原因: chromedp 在 NewRemoteAllocator 模式下，context.WithTimeout 派生的 context
	// 会导致 CDP 响应路由异常（Run 挂起直到超时）。调用方 context 已包含合理超时。
	// 诊断证据: EvalJS(browserCtx) 370µs OK, Run(WithTimeout(browserCtx, 5s)) 5s 超时。
	switch parsed.Op {
	case "click":
		if err := e.executeClick(ctx, resolvedRef); err != nil {
			return nil, err
		}
	case "clickat":
		if err := e.executeClickAt(ctx, resolvedRef, parsed.CoordX, parsed.CoordY); err != nil {
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
		if err := e.executeFill(ctx, resolvedRef, parsed.Value); err != nil {
			return nil, err
		}
	case "type":
		if err := e.executeType(ctx, resolvedRef, parsed.Value); err != nil {
			return nil, err
		}
	case "press":
		if err := e.executePress(ctx, resolvedRef, parsed.Value); err != nil {
			return nil, err
		}
	case "hover":
		if err := e.executeHover(ctx, resolvedRef); err != nil {
			return nil, err
		}
	case "scroll":
		if err := e.executeScroll(ctx, resolvedRef); err != nil {
			return nil, err
		}
	case "select":
		if err := e.executeSelect(ctx, resolvedRef, parsed.Value); err != nil {
			return nil, err
		}
	case "back":
		if err := e.executeBack(ctx); err != nil {
			return nil, err
		}
	case "forward":
		if err := e.executeForward(ctx); err != nil {
			return nil, err
		}
	case "focus":
		if err := e.executeFocusSelector(ctx, resolvedRef); err != nil {
			return nil, err
		}
	case "scrollinto":
		if err := e.executeScrollIntoView(ctx, resolvedRef); err != nil {
			return nil, err
		}
	case "check":
		if err := e.executeCheck(ctx, resolvedRef); err != nil {
			return nil, err
		}
	case "uncheck":
		if err := e.executeUncheck(ctx, resolvedRef); err != nil {
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
	domSettleOps := map[string]bool{"click": true, "tap": true, "type": true, "fill": true, "select": true, "check": true, "uncheck": true}
	if domSettleOps[parsed.Op] {
		_ = waitForDOMSettle(ctx, 500, 5000)
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
			return "", fmt.Errorf("%w: %s not found in current snapshot (run snap first)", ErrRefNotFound, refKey)
		}
		// Return as internal ref string so executeClick etc. can use it
		return refKey, nil

	case SelectorTestID:
		meta, ok := e.snapEngine.LookupByTestID(sel.TestID)
		if ok && meta.BackendNodeID != 0 {
			return meta.Ref, nil
		}
		// 回退到 CSS 选择器
		return `[data-testid="` + sel.TestID + `"]`, nil

	case SelectorCanonical:
		return e.resolveCanonicalSelector(sel, sessionMode)

	case SelectorRoleName, SelectorRoleNameExact:
		allMeta := e.snapEngine.LookupAllByRoleName(sel.Role, sel.Name, sel.NameOp)
		if len(allMeta) == 0 {
			byRole := e.snapEngine.AllByRole(sel.Role)
			if len(byRole) == 0 {
				return "", fmt.Errorf("%w: 元素 %s:'%s' 未找到。当前页面无 %s 元素",
					ErrRefNotFound, sel.Role, sel.Name, sel.Role)
			}
			return "", fmt.Errorf("%w: 元素 %s:'%s' 未找到。可用的 %s: %v",
				ErrRefNotFound, sel.Role, sel.Name, sel.Role, byRole)
		}
		if len(allMeta) > 1 {
			return "", buildAmbiguousError(sel.Raw, allMeta)
		}
		return allMeta[0].Ref, nil

	case SelectorRole:
		meta, ok := e.snapEngine.LookupByRoleName(sel.Role, "")
		if !ok {
			return "", fmt.Errorf("%w: role=%s 的元素未找到", ErrRefNotFound, sel.Role)
		}
		return meta.Ref, nil

	default:
		// SelectorCSS: return as-is
		return sel.Raw, nil
	}
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
			if ok && meta.BackendNodeID != 0 {
				return meta.Ref, nil
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
		return "", fmt.Errorf("%w: canonical selector %q matched no elements", ErrRefNotFound, sel.Raw)
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
	backendNodeID, ok := e.snapEngine.LookupRef(ref)
	if !ok {
		return 0, fmt.Errorf("%w: ref %q not found in current snapshot", ErrRefNotFound, ref)
	}
	return backendNodeID, nil
}

// executeClick 执行点击操作（支持 element ref、@rN ref 和 CSS 选择器）。
func (e *actionEngine) executeClick(ctx context.Context, ref string) error {
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
		return dispatchMouseClickAt(ctx, x, y)
	}

	// 对 DOM 发现的 clickable 类型元素，用 data-testid CSS 选择器点击
	if meta, ok := e.snapEngine.LookupRefMeta(ref); ok && meta.Role == "clickable" && meta.Name != "" {
		selector := `[data-testid="` + meta.Name + `"]`
		return chromedp.Run(ctx, chromedp.Click(selector, chromedp.ByQuery))
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
	if err := chromedp.Run(ctx, chromedp.WaitVisible(ref, chromedp.ByQuery), chromedp.Evaluate(js, &boxJSON)); err != nil {
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

func dispatchMouseClickAt(ctx context.Context, x, y float64) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		if err := input.DispatchMouseEvent(input.MouseMoved, x, y).Do(ctx); err != nil {
			return err
		}
		time.Sleep(18 * time.Millisecond)
		if err := input.DispatchMouseEvent(input.MousePressed, x, y).
			WithButton(input.Left).
			WithButtons(1).
			WithClickCount(1).
			Do(ctx); err != nil {
			return err
		}
		time.Sleep(42 * time.Millisecond)
		return input.DispatchMouseEvent(input.MouseReleased, x, y).
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

// executeClickAt 对目标元素的相对坐标执行真实鼠标点击。
// 设计意图:
//   - 保持现有 click 语义不变，避免把宿主 DOM 自动化和人类指针输入混为一谈
//   - 为 liveview/takeover 这类需要真实 clientX/clientY 的桌面鼠标交互提供稳定入口
func (e *actionEngine) executeClickAt(ctx context.Context, ref string, relX, relY float64) error {
	box, err := e.resolveElementBox(ctx, ref)
	if err != nil {
		return err
	}
	if box.Width <= 0 || box.Height <= 0 {
		return fmt.Errorf("%w: target %q has invalid box %.1fx%.1f", ErrActFailed, ref, box.Width, box.Height)
	}
	x := box.Left + box.Width*relX
	y := box.Top + box.Height*relY
	return dispatchMouseClickAt(ctx, x, y)
}

func (e *actionEngine) executeTap(ctx context.Context, ref string) error {
	return e.executeTapAt(ctx, ref, 0.5, 0.5)
}

// executeTapAt 对目标元素的相对坐标执行真实触控点击。
// 设计意图:
//   - 作为 clickat 的触控对偶，支撑 iOS / Android / coarse pointer 测试
//   - 在 liveview 宿主页上触发 BrowserPanel 的 touch* 链路，而不是退化成 mouse 事件
func (e *actionEngine) executeTapAt(ctx context.Context, ref string, relX, relY float64) error {
	box, err := e.resolveElementBox(ctx, ref)
	if err != nil {
		return err
	}
	if box.Width <= 0 || box.Height <= 0 {
		return fmt.Errorf("%w: target %q has invalid box %.1fx%.1f", ErrActFailed, ref, box.Width, box.Height)
	}
	x := box.Left + box.Width*relX
	y := box.Top + box.Height*relY
	return dispatchTouchTapAt(ctx, x, y)
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

// executePress 执行按键操作，支持组合键（Ctrl+A, Shift+Enter 等）。
func (e *actionEngine) executePress(ctx context.Context, ref string, key string) error {
	// Focus element first if ref is specified
	if ref != "" {
		if isCSSSelector(ref) {
			if err := chromedp.Run(ctx, chromedp.Focus(ref, chromedp.ByQuery)); err != nil {
				return fmt.Errorf("%w: focus %q for press: %v", ErrActFailed, ref, err)
			}
		} else {
			backendNodeID, err := e.resolveBackendNodeID(ref)
			if err != nil {
				return err
			}
			if err := chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
				nodeIDs, err := dom.PushNodesByBackendIDsToFrontend([]cdp.BackendNodeID{cdp.BackendNodeID(backendNodeID)}).Do(ctx)
				if err != nil || len(nodeIDs) == 0 {
					return fmt.Errorf("%w: node not found for ref %q", ErrRefNotFound, ref)
				}
				return chromedp.Run(ctx, chromedp.Focus([]cdp.NodeID{nodeIDs[0]}, chromedp.ByNodeID))
			})); err != nil {
				return err
			}
		}
	}

	// Map key name to chromedp SendKeys string
	keyStr := mapKeyName(key)
	return chromedp.Run(ctx, chromedp.KeyEvent(keyStr))
}

// mapKeyName 将按键名称（如 "Enter", "Ctrl+A"）映射到 chromedp 可用的按键字符串。
func mapKeyName(key string) string {
	// Handle combos like Ctrl+A, Shift+Enter
	// For simple keys, use chromedp key event symbols
	keyMap := map[string]string{
		"Enter":      "\r",
		"Tab":        "\t",
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
	if isCSSSelector(ref) {
		return chromedp.Run(ctx, chromedp.Focus(ref, chromedp.ByQuery))
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
		return chromedp.Run(ctx, chromedp.Focus([]cdp.NodeID{nodeIDs[0]}, chromedp.ByNodeID))
	}))
}

// executeScrollIntoView 滚动元素到可见区域。
func (e *actionEngine) executeScrollIntoView(ctx context.Context, ref string) error {
	if isCSSSelector(ref) {
		return chromedp.Run(ctx, chromedp.ScrollIntoView(ref, chromedp.ByQuery))
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
func (e *actionEngine) executeCheck(ctx context.Context, ref string) error {
	return e.executeToggleCheckbox(ctx, ref, true)
}

// executeUncheck 取消勾选复选框（若已勾选则点击）。
func (e *actionEngine) executeUncheck(ctx context.Context, ref string) error {
	return e.executeToggleCheckbox(ctx, ref, false)
}

// executeToggleCheckbox 切换复选框状态。
func (e *actionEngine) executeToggleCheckbox(ctx context.Context, ref string, wantChecked bool) error {
	// Get current checked state via JS
	var checkedSelector string
	if isCSSSelector(ref) {
		checkedSelector = ref
	} else {
		// Use ref as a fallback CSS selector stub
		checkedSelector = ref
	}

	var isChecked bool
	checkJS := fmt.Sprintf(`(() => {
		const el = document.querySelector(%q);
		if (!el) return false;
		return el.checked === true;
	})()`, checkedSelector)

	if isCSSSelector(ref) {
		if err := chromedp.Run(ctx, chromedp.Evaluate(checkJS, &isChecked)); err != nil {
			isChecked = false
		}
		if isChecked != wantChecked {
			return e.executeClick(ctx, ref)
		}
		return nil
	}

	// For node refs, check via JS using data attribute approach — just click and check
	if wantChecked && !isChecked {
		return e.executeClick(ctx, ref)
	} else if !wantChecked && isChecked {
		return e.executeClick(ctx, ref)
	}
	return nil
}

// executeScroll 执行滚动操作。
// 使用 JS window.scrollBy 替代 CDP Input.dispatchMouseEvent(mouseWheel)。
// 原因: CDP mouseWheel 在 headless Chrome + RemoteAllocator 场景下不可靠（响应延迟/超时）。
// JS 路径经诊断验证 370µs 即时完成，且跨所有 Chrome 模式一致可靠。
func (e *actionEngine) executeScroll(ctx context.Context, dir string) error {
	var deltaY int
	switch strings.ToLower(dir) {
	case "down":
		deltaY = 300
	case "up":
		deltaY = -300
	default:
		deltaY = 300
	}

	js := fmt.Sprintf("window.scrollBy(0, %d)", deltaY)
	return chromedp.Run(ctx, chromedp.Evaluate(js, nil))
}

// executeSelect 执行下拉选择操作。
func (e *actionEngine) executeSelect(ctx context.Context, ref string, value string) error {
	if isCSSSelector(ref) {
		return e.executeSelectBySelector(ctx, ref, value)
	}

	if meta, ok := e.snapEngine.LookupRefMeta(ref); ok && meta.TestID != "" {
		if err := e.executeSelectBySelector(ctx, `[data-testid="`+meta.TestID+`"]`, value); err == nil {
			return nil
		}
	}

	nodeID, ok := e.snapEngine.LookupRef(ref)
	if !ok {
		return fmt.Errorf("%w: ref %q not found", ErrRefNotFound, ref)
	}

	return chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		nodeIDs, err := dom.PushNodesByBackendIDsToFrontend([]cdp.BackendNodeID{cdp.BackendNodeID(nodeID)}).Do(ctx)
		if err != nil || len(nodeIDs) == 0 {
			return fmt.Errorf("%w: node not found for ref %q", ErrRefNotFound, ref)
		}
		if err := chromedp.Run(ctx,
			chromedp.Focus([]cdp.NodeID{nodeIDs[0]}, chromedp.ByNodeID),
			chromedp.SetValue([]cdp.NodeID{nodeIDs[0]}, value, chromedp.ByNodeID),
		); err != nil {
			return err
		}
		var selected string
		if err := chromedp.Run(ctx, chromedp.Evaluate(`(() => {
			const el = document.activeElement;
			if (!el || el.tagName !== 'SELECT') return '';
			el.dispatchEvent(new Event('input', { bubbles: true }));
			el.dispatchEvent(new Event('change', { bubbles: true }));
			return String(el.value || '');
		})()`, &selected)); err != nil {
			return err
		}
		if selected != "" && selected != value {
			return fmt.Errorf("%w: select value mismatch for %q (want %q got %q)", ErrActFailed, ref, value, selected)
		}
		return nil
	}))
}

func (e *actionEngine) executeSelectBySelector(ctx context.Context, selector string, value string) error {
	var result struct {
		Found bool   `json:"found"`
		Value string `json:"value"`
	}
	js := fmt.Sprintf(`(() => {
		const el = document.querySelector(%q);
		if (!el) return { found: false, value: '' };
		if (el.tagName !== 'SELECT') return { found: true, value: String(el.value || '') };
		el.focus();
		el.value = %q;
		el.dispatchEvent(new Event('input', { bubbles: true }));
		el.dispatchEvent(new Event('change', { bubbles: true }));
		return { found: true, value: String(el.value || '') };
	})()`, selector, value)
	if err := chromedp.Run(ctx,
		chromedp.WaitVisible(selector, chromedp.ByQuery),
		chromedp.Evaluate(js, &result),
	); err != nil {
		return err
	}
	if !result.Found {
		return fmt.Errorf("%w: element %q not found", ErrRefNotFound, selector)
	}
	if result.Value != value {
		return fmt.Errorf("%w: select value mismatch for %q (want %q got %q)", ErrActFailed, selector, value, result.Value)
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
