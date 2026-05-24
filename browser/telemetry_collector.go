package browser

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/network"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// ============================================================
// § TelemetryCollector — CDP 遥测事件采集
// ============================================================

// ConsoleError 是单条控制台错误/警告记录。
type ConsoleError struct {
	Level  string `json:"level"`
	Text   string `json:"text"`
	Source string `json:"source,omitempty"`
	URL    string `json:"url,omitempty"`
	Line   int    `json:"line,omitempty"`
}

// NetworkFailure 是单条网络失败记录（加载失败或 HTTP 4xx/5xx）。
type NetworkFailure struct {
	URL        string `json:"url"`
	Method     string `json:"method"`
	StatusCode int    `json:"status_code,omitempty"`
	Error      string `json:"error,omitempty"`
}

// TelemetryCollector 通过 CDP 事件监听持续采集 console errors 和 network failures。
//
// 使用方式:
//
//	tc := NewTelemetryCollector(cdpCtx)
//	// ... 执行页面操作 ...
//	errs := tc.GetConsoleErrors()
//	fails := tc.GetNetworkFailures()
//	tc.Stop()
type TelemetryCollector struct {
	mu              sync.Mutex
	consoleErrors   []ConsoleError
	networkFailures []NetworkFailure
	cancel          context.CancelFunc

	// requestMethods 缓存 requestID → method，供 EventResponseReceived 使用
	requestMethods map[network.RequestID]string
}

// NewTelemetryCollector 创建 TelemetryCollector 并通过 ctx 注册 CDP 事件监听。
//
// ctx 必须是有效的 chromedp target context（即 chromedp.NewContext 返回的 context，
// 或其子 context）。监听器随 ctx 取消而自动停止。
//
// 调用方负责在合适时机调用 Stop()，或依赖 ctx 取消来终止。
func NewTelemetryCollector(ctx context.Context) *TelemetryCollector {
	tc := &TelemetryCollector{
		consoleErrors:   make([]ConsoleError, 0),
		networkFailures: make([]NetworkFailure, 0),
		requestMethods:  make(map[network.RequestID]string),
	}

	// 激活 Runtime 和 Network CDP 域事件
	// 忽略错误（若 ctx 已取消或 CDP 不可用，ListenTarget 也不会 panic）
	_ = chromedp.Run(ctx,
		cdpruntime.Enable(),
		network.Enable(),
	)

	// 注册 CDP 事件监听器
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch event := ev.(type) {
		case *cdpruntime.EventConsoleAPICalled:
			tc.handleConsoleEvent(event)
		case *cdpruntime.EventExceptionThrown:
			tc.handleExceptionEvent(event)
		case *network.EventRequestWillBeSent:
			tc.handleRequestSent(event)
		case *network.EventResponseReceived:
			tc.handleResponseReceived(event)
		case *network.EventLoadingFailed:
			tc.handleLoadingFailed(event)
		}
	})

	return tc
}

// GetConsoleErrors 返回已采集的 console errors 副本（线程安全）。
func (tc *TelemetryCollector) GetConsoleErrors() []ConsoleError {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	out := make([]ConsoleError, len(tc.consoleErrors))
	copy(out, tc.consoleErrors)
	return out
}

// GetNetworkFailures 返回已采集的 network failures 副本（线程安全）。
func (tc *TelemetryCollector) GetNetworkFailures() []NetworkFailure {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	out := make([]NetworkFailure, len(tc.networkFailures))
	copy(out, tc.networkFailures)
	return out
}

// Reset 清空已采集的事件（线程安全）。
func (tc *TelemetryCollector) Reset() {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.consoleErrors = make([]ConsoleError, 0)
	tc.networkFailures = make([]NetworkFailure, 0)
	tc.requestMethods = make(map[network.RequestID]string)
}

// Stop 停止监听（取消内部 context）。
// 若 TelemetryCollector 是通过 NewTelemetryCollector 创建的，监听器随 ctx 取消自动停止，
// 此方法用于提前终止。若未设置 cancel，此方法为空操作。
func (tc *TelemetryCollector) Stop() {
	if tc.cancel != nil {
		tc.cancel()
	}
}

// ============================================================
// § 内部事件处理
// ============================================================

func (tc *TelemetryCollector) handleConsoleEvent(ev *cdpruntime.EventConsoleAPICalled) {
	level := string(ev.Type)
	// 只记录 error 和 warning 级别
	if level != "error" && level != "warning" {
		return
	}

	text := consoleArgsToText(ev.Args)
	if text == "" {
		return
	}

	entry := ConsoleError{
		Level: level,
		Text:  text,
	}

	// 提取调用栈顶部位置信息（若有）
	if ev.StackTrace != nil && len(ev.StackTrace.CallFrames) > 0 {
		frame := ev.StackTrace.CallFrames[0]
		entry.URL = frame.URL
		entry.Line = int(frame.LineNumber)
	}

	tc.mu.Lock()
	tc.consoleErrors = append(tc.consoleErrors, entry)
	tc.mu.Unlock()
}

func (tc *TelemetryCollector) handleExceptionEvent(ev *cdpruntime.EventExceptionThrown) {
	if ev.ExceptionDetails == nil {
		return
	}

	text := ev.ExceptionDetails.Text
	if ev.ExceptionDetails.Exception != nil && ev.ExceptionDetails.Exception.Description != "" {
		text = ev.ExceptionDetails.Exception.Description
	}
	if text == "" {
		return
	}

	entry := ConsoleError{
		Level:  "error",
		Text:   fmt.Sprintf("Uncaught exception: %s", text),
		Source: "javascript",
		URL:    ev.ExceptionDetails.URL,
		Line:   int(ev.ExceptionDetails.LineNumber),
	}

	tc.mu.Lock()
	tc.consoleErrors = append(tc.consoleErrors, entry)
	tc.mu.Unlock()
}

func (tc *TelemetryCollector) handleRequestSent(ev *network.EventRequestWillBeSent) {
	if ev.Request == nil {
		return
	}
	tc.mu.Lock()
	tc.requestMethods[ev.RequestID] = ev.Request.Method
	tc.mu.Unlock()
}

func (tc *TelemetryCollector) handleResponseReceived(ev *network.EventResponseReceived) {
	if ev.Response == nil {
		return
	}
	statusCode := int(ev.Response.Status)
	if statusCode < 400 {
		return
	}

	tc.mu.Lock()
	method := tc.requestMethods[ev.RequestID]
	tc.mu.Unlock()

	entry := NetworkFailure{
		URL:        ev.Response.URL,
		Method:     method,
		StatusCode: statusCode,
	}

	tc.mu.Lock()
	tc.networkFailures = append(tc.networkFailures, entry)
	tc.mu.Unlock()
}

func (tc *TelemetryCollector) handleLoadingFailed(ev *network.EventLoadingFailed) {
	errText := ev.ErrorText
	if errText == "" || errText == "net::ERR_ABORTED" {
		// 忽略主动取消的请求（如页面导航中断旧请求）
		return
	}

	tc.mu.Lock()
	method := tc.requestMethods[ev.RequestID]
	tc.mu.Unlock()

	entry := NetworkFailure{
		URL:    string(ev.RequestID), // URL 在 LoadingFailed 中不可直接获取，用 requestID 占位
		Method: method,
		Error:  errText,
	}

	tc.mu.Lock()
	tc.networkFailures = append(tc.networkFailures, entry)
	tc.mu.Unlock()
}

// consoleArgsToText 将 RemoteObject 参数列表拼接为可读文本。
func consoleArgsToText(args []*cdpruntime.RemoteObject) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == nil {
			continue
		}
		if arg.Value != nil {
			// JSON-encoded primitive（字符串会被引号包裹，去掉引号）
			s := strings.Trim(string(arg.Value), `"`)
			if s != "" && s != "null" && s != "undefined" {
				parts = append(parts, s)
			}
		} else if arg.Description != "" {
			parts = append(parts, arg.Description)
		}
	}
	return strings.Join(parts, " ")
}

// ============================================================
// § CollectTelemetrySnapshot — 便捷函数（stateless CLI 用）
// ============================================================

// CollectTelemetrySnapshot 在已有 CDP context 上短暂采集遥测事件后返回结果。
//
// 策略（P2 stateless CLI）:
//  1. 使用调用方传入的 cdpCtx（来自 connectSession 底层 context）
//  2. enable Runtime + Network 域，等待 waitMs 毫秒收集新产生的事件
//  3. 返回实际采集到的 ConsoleErrors 和 NetworkFailures
//
// waitMs 建议值: 100（快速扫描），500（等待异步加载后的错误）。
// 注意: stateless CLI 每次连接生命周期很短，只能捕获 enable 后新产生的事件。
func CollectTelemetrySnapshot(ctx context.Context, waitMs int) ([]ConsoleError, []NetworkFailure, error) {
	if waitMs <= 0 {
		waitMs = 100
	}

	tc := NewTelemetryCollector(ctx)

	// 等待事件累积
	select {
	case <-time.After(time.Duration(waitMs) * time.Millisecond):
	case <-ctx.Done():
	}

	return tc.GetConsoleErrors(), tc.GetNetworkFailures(), nil
}
