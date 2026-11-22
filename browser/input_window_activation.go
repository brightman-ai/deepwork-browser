package browser

import "context"

// activateFrontmostIn 是默认的窗口内激活：先确认凭证覆盖本引擎的 key，再把 target 提到前台。
//
// [ARCH-INPUT-FRONTMOST] 它只作为 targetActivation 的默认钩子，由 enterInputDispatchWindowForRequest
// 在已持有输入窗口时调用；action_engine.go 里不出现裸 ensureTargetFrontmost(ctx)，所有真实输入动作
// 共用输入派发窗口里的这一次前置。
func (e *actionEngine) activateFrontmostIn(ctx context.Context, w *BrowserInputLock) (bool, error) {
	if err := w.Covers(e.browserInputKey); err != nil {
		return false, err
	}
	return ensureTargetFrontmost(ctx)
}
