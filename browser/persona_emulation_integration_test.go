//go:build personacheck

// 独立 build tag(非 integration):预存 integration_test.go 有 dead import(chromedp
// 未用,commit ad5639d 遗留)使 `-tags integration` 整体构建失败;本 persona 系统
// oracle 用独立 tag 隔离,不被无关 rot 阻塞。跑法见文件尾注释。
package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// 系统 oracle(REQ-04/06/07 + REQ-01 UA):真 chromium headless 施加 persona 的
// device-metrics+touch + Env/Network/Shell facet,加载 selfcheck 夹具,断言运行时
// 实测信号 == persona 声明。低层直连 chromium,隔离验 applyPersonaEmulation 机制。
//
// 跑法:go test -tags integration ./browser/ -run TestPersonaEmulation_Integration -v
func findChromiumForTest(t *testing.T) string {
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "/snap/bin/chromium"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("no chromium/chrome binary found for integration test")
	return ""
}

func TestPersonaEmulation_Integration(t *testing.T) {
	chromePath := findChromiumForTest(t)

	allocCtx, cancelA := chromedp.NewExecAllocator(context.Background(),
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.ExecPath(chromePath),
			chromedp.Flag("headless", "new"),
			chromedp.NoSandbox,
			chromedp.DisableGPU,
		)...)
	defer cancelA()
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	ctx, tcancel := context.WithTimeout(ctx, 45*time.Second)
	defer tcancel()

	persona := &Persona{
		ID:          "test-wechat-iphone-cn-dark",
		Fingerprint: PresetIPhone15Pro,
		Shell:       ShellProfile{Kind: ShellWeChat},
		Network:     NetworkProfile{Kind: "slow-3g"},
		Env:         EnvProfile{ColorScheme: "dark", Timezone: "Asia/Shanghai", Locale: "zh-CN"},
	}
	fp := persona.fingerprint()
	if fp == nil {
		t.Fatal("persona fingerprint nil")
	}

	dataURL := "data:text/html;base64," + base64.StdEncoding.EncodeToString([]byte(PersonaSelfCheckHTML))

	var dump string
	err := chromedp.Run(ctx,
		// device-metrics + touch 必须在导航前设(驱动 hover/pointer 媒体查询)
		chromedp.ActionFunc(func(ctx context.Context) error {
			return applyViewportProfile(ctx, fp.ViewportW, fp.ViewportH, fp.DeviceScaleFactor, fp.Mobile, fp.Touch, int64(fp.MaxTouchPoints))
		}),
		// persona facet realization (Shell UA / Env / Network)
		chromedp.ActionFunc(func(ctx context.Context) error {
			return applyPersonaEmulation(ctx, persona)
		}),
		chromedp.Navigate(dataURL),
		chromedp.Text("#dump", &dump, chromedp.ByID, chromedp.NodeVisible),
	)
	if err != nil {
		t.Fatalf("chromedp run: %v", err)
	}

	var sig struct {
		UserAgent string `json:"userAgent"`
		Screen    struct {
			W   int     `json:"w"`
			H   int     `json:"h"`
			DPR float64 `json:"dpr"`
		} `json:"screen"`
		Media struct {
			HoverNone     bool `json:"hoverNone"`
			PointerCoarse bool `json:"pointerCoarse"`
			Dark          bool `json:"dark"`
		} `json:"media"`
		TZ             string `json:"tz"`
		Locale         string `json:"locale"`
		MaxTouchPoints int    `json:"maxTouchPoints"`
		ServiceWorker  bool   `json:"serviceWorker"`
		Clipboard      bool   `json:"clipboard"`
		WeixinJSBridge bool   `json:"weixinJSBridge"`
	}
	if err := json.Unmarshal([]byte(dump), &sig); err != nil {
		t.Fatalf("parse dump %q: %v", dump, err)
	}
	t.Logf("selfcheck dump: %s", dump)

	// REQ-01 Shell UA:含 iPhone + MicroMessenger(触发微信遮罩的前提)
	if !strings.Contains(sig.UserAgent, "iPhone") || !strings.Contains(sig.UserAgent, "MicroMessenger") {
		t.Errorf("REQ-01 UA 应含 iPhone+MicroMessenger,实得 %q", sig.UserAgent)
	}
	// REQ-04 触摸设备:hover:none + pointer:coarse(hover-only UI 塌陷前提)
	if !sig.Media.HoverNone {
		t.Errorf("REQ-04 (hover:none) 应为 true(移动设备无 hover)")
	}
	if !sig.Media.PointerCoarse {
		t.Errorf("REQ-04 (pointer:coarse) 应为 true")
	}
	// REQ-07 Env:暗色 / 时区 / locale
	if !sig.Media.Dark {
		t.Errorf("REQ-07 prefers-color-scheme:dark 应为 true")
	}
	if sig.TZ != "Asia/Shanghai" {
		t.Errorf("REQ-07 timezone 应为 Asia/Shanghai,实得 %q", sig.TZ)
	}
	if !strings.HasPrefix(sig.Locale, "zh") {
		t.Errorf("REQ-07 locale 应为 zh*,实得 %q", sig.Locale)
	}
	// 设备几何(来自 vendored Playwright:iPhone 15 Pro dpr=3)
	if sig.Screen.DPR != 3 {
		t.Errorf("devicePixelRatio 应为 3,实得 %v", sig.Screen.DPR)
	}
	if sig.MaxTouchPoints < 1 {
		t.Errorf("maxTouchPoints 应 >=1(触摸设备),实得 %d", sig.MaxTouchPoints)
	}
	// REQ-05 Shell:bridge 对象注入(WeixinJSBridge 非原生 → 存在即证 shim 生效)
	if !sig.WeixinJSBridge {
		t.Errorf("REQ-05 WeixinJSBridge 应存在(壳 shim 注入)")
	}
	// REQ-05 能力破损:忠实 webview 无 Service Worker / Clipboard
	if sig.ServiceWorker {
		t.Errorf("REQ-05 'serviceWorker' in navigator 应为 false(壳破损)")
	}
	if sig.Clipboard {
		t.Errorf("REQ-05 navigator.clipboard 应不可用(壳破损)")
	}
}
