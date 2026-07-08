//go:build personacheck

package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// CP-C 系统 oracle:证明 open 命令的 attach 路径(NewBrowserCoreFromSession)也施加
// persona facet —— 不只 once/test 的直连路径。这堵住了 D-6 的双路径分裂:persona 经
// personaID rails 到达 FromSession(1545 applyPersonaEmulation),Env(dark/tz/locale)
// 与 Shell(serviceWorker=false/WeixinJSBridge=true)在 open 路径生效。
//
// 跑法:go test -tags personacheck ./browser/ -run TestPersonaOpenPath_Integration -v
//
// 注:device-metrics(hover:none/pointer:coarse)由 replayViewportProfile 另加(main.go),
// 不在 FromSession 内 → 本测试断言 FromSession 直接负责的 Env/Shell facet。

func launchChromiumForOpenPath(t *testing.T, chromePath string, port int) (wsURL string, kill func()) {
	t.Helper()
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, chromePath,
		"--headless=new", "--no-sandbox", "--disable-gpu",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--user-data-dir="+t.TempDir(), "about:blank")
	if err := cmd.Start(); err != nil {
		t.Fatalf("launch chromium: %v", err)
	}
	kill = func() { _ = cmd.Process.Kill() }
	// poll /json/version for webSocketDebuggerUrl
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			var v map[string]interface{}
			if json.Unmarshal(body, &v) == nil {
				if u, ok := v["webSocketDebuggerUrl"].(string); ok && u != "" {
					return u, kill
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	kill()
	t.Fatal("chromium devtools ws url not ready")
	return "", kill
}

func openPathSelfcheck(t *testing.T, chromePath string, port int, presetID, personaID string) string {
	t.Helper()
	wsURL, kill := launchChromiumForOpenPath(t, chromePath, port)
	defer kill()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sc, err := NewBrowserCoreFromSession(ctx, wsURL, "", presetID, personaID, ModeHeadless)
	if err != nil {
		t.Fatalf("NewBrowserCoreFromSession(persona=%q): %v", personaID, err)
	}
	impl, ok := sc.(*browserCoreImpl)
	if !ok {
		t.Fatal("SessionCore 非 *browserCoreImpl")
	}
	dataURL := "data:text/html;base64," + base64.StdEncoding.EncodeToString([]byte(PersonaSelfCheckHTML))
	if _, err := impl.Navigate(ctx, dataURL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	var dump string
	if err := impl.EvalJS(ctx, PersonaSelfCheckJS, &dump); err != nil {
		t.Fatalf("EvalJS selfcheck: %v", err)
	}
	_ = impl.Close(context.Background())
	return dump
}

func TestPersonaOpenPath_Integration(t *testing.T) {
	chromePath := findChromiumForTest(t)

	// Shell facet(wechat-iphone)经 open 路径 → SW 破损 + bridge
	shellDump := openPathSelfcheck(t, chromePath, 9455, PresetIPhone15Pro, "wechat-iphone")
	t.Logf("wechat-iphone open-path dump: %s", shellDump)
	var sh struct {
		UserAgent      string `json:"userAgent"`
		ServiceWorker  bool   `json:"serviceWorker"`
		WeixinJSBridge bool   `json:"weixinJSBridge"`
	}
	if err := json.Unmarshal([]byte(shellDump), &sh); err != nil {
		t.Fatalf("parse shell dump: %v", err)
	}
	if !strings.Contains(sh.UserAgent, "MicroMessenger") {
		t.Errorf("open 路径 UA 应含 MicroMessenger,实得 %q", sh.UserAgent)
	}
	if !sh.WeixinJSBridge {
		t.Error("open 路径 Shell 未生效:WeixinJSBridge 应存在")
	}
	if sh.ServiceWorker {
		t.Error("open 路径 Shell 未生效:serviceWorker 应为 false")
	}

	// Env facet(desktop-cn-dark)经 open 路径 → dark + timezone + locale
	envDump := openPathSelfcheck(t, chromePath, 9456, PresetMacOSChrome, "desktop-cn-dark")
	t.Logf("desktop-cn-dark open-path dump: %s", envDump)
	var en struct {
		Media struct {
			Dark bool `json:"dark"`
		} `json:"media"`
		TZ     string `json:"tz"`
		Locale string `json:"locale"`
	}
	if err := json.Unmarshal([]byte(envDump), &en); err != nil {
		t.Fatalf("parse env dump: %v", err)
	}
	if !en.Media.Dark {
		t.Error("open 路径 Env 未生效:prefers-color-scheme:dark 应为 true")
	}
	if en.TZ != "Asia/Shanghai" {
		t.Errorf("open 路径 Env 未生效:timezone 应为 Asia/Shanghai,实得 %q", en.TZ)
	}
	if !strings.HasPrefix(en.Locale, "zh") {
		t.Errorf("open 路径 Env 未生效:locale 应为 zh*,实得 %q", en.Locale)
	}
}
