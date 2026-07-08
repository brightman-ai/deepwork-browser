package browser

import (
	"context"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// networkConditions — 命名网络档 → CDP 节流参数(bytes/sec, ms)。
// 数值对齐 Chrome DevTools 的标准 3G/4G 档。
type networkConditions struct {
	offline  bool
	latency  float64 // ms
	download float64 // bytes/sec, -1 = 不限
	upload   float64 // bytes/sec, -1 = 不限
}

func networkConditionsFor(kind string) (networkConditions, bool) {
	switch kind {
	case "offline":
		return networkConditions{offline: true, latency: 0, download: 0, upload: 0}, true
	case "slow-3g":
		return networkConditions{latency: 400, download: 400 * 1024 / 8, upload: 400 * 1024 / 8}, true
	case "fast-3g":
		return networkConditions{latency: 150, download: 1500 * 1024 / 8, upload: 750 * 1024 / 8}, true
	case "4g":
		return networkConditions{latency: 20, download: 4000 * 1024 / 8, upload: 3000 * 1024 / 8}, true
	default:
		return networkConditions{}, false
	}
}

// applyPersonaEmulation 用 CDP 原生 realize 一个 persona 的 Shell/Network/Env facet。
// 这是 facet 的运行时"实现事实"层,补在 applyFingerprintEmulation / 视口模拟之后。
//
// 覆盖:
//   - Shell:壳存在时,用 persona 的 EffectiveUserAgent(含 in-app token)覆写 UA
//     —— 修复 applyFingerprintEmulation 对 Chrome-UA 指纹会剥掉壳后缀的问题(REQ-01/02)。
//   - Env:prefers-color-scheme / timezone / locale(REQ-07),以及暗色 persona 的 AcceptLanguage。
//   - Network:节流/离线(REQ-06)。
//
// 注:hover:none / pointer:coarse(REQ-04)由 device-metrics(mobile)+ touch 模拟
// 自动产生,不在此重复设置。
func applyPersonaEmulation(ctx context.Context, p *Persona) error {
	if p == nil {
		return nil
	}
	var actions []chromedp.Action

	// Shell:壳 UA(含 in-app token)覆写。仅当有壳时才覆写,避免动无壳 persona 的 UA。
	if p.Shell.Kind != ShellNone {
		ua := p.EffectiveUserAgent()
		uaParams := emulation.SetUserAgentOverride(ua)
		if fp := p.fingerprint(); fp != nil && fp.Platform != "" {
			uaParams = uaParams.WithPlatform(fp.Platform)
		}
		if p.Env.Locale != "" {
			uaParams = uaParams.WithAcceptLanguage(acceptLanguageFor(p.Env.Locale))
		}
		actions = append(actions, uaParams)
	} else if p.Env.Locale != "" {
		// 无壳但要覆写 locale/AcceptLanguage(如 desktop-cn-dark)。
		actions = append(actions, emulation.SetUserAgentOverride(p.EffectiveUserAgent()).
			WithAcceptLanguage(acceptLanguageFor(p.Env.Locale)))
	}

	// Env:prefers-color-scheme。
	if p.Env.ColorScheme != "" {
		actions = append(actions, emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{
			{Name: "prefers-color-scheme", Value: p.Env.ColorScheme},
		}))
	}
	// Env:timezone。
	if p.Env.Timezone != "" {
		actions = append(actions, emulation.SetTimezoneOverride(p.Env.Timezone))
	}
	// Env:locale(影响 Intl/toLocaleString)。CDP 用 ICU C locale 形式(zh_CN)。
	if p.Env.Locale != "" {
		actions = append(actions, emulation.SetLocaleOverride().WithLocale(icuLocale(p.Env.Locale)))
	}

	// Network:节流/离线。ByRule.Do 返回 ([]string, error),包进 ActionFunc 适配。
	if nc, ok := networkConditionsFor(p.Network.Kind); ok {
		actions = append(actions,
			network.Enable(),
			chromedp.ActionFunc(func(ctx context.Context) error {
				_, err := network.EmulateNetworkConditionsByRule(nc.offline, []*network.Conditions{{
					URLPattern:         "", // 空 = 匹配所有请求
					Latency:            nc.latency,
					DownloadThroughput: nc.download,
					UploadThroughput:   nc.upload,
				}}).Do(ctx)
				return err
			}),
		)
	}

	// Shell:bridge 对象 + 能力破损 JS shim(每个新 document 注入)。
	if shell := GenerateShellScript(p); shell != "" {
		actions = append(actions, chromedp.ActionFunc(func(ctx context.Context) error {
			_, err := cdppage.AddScriptToEvaluateOnNewDocument(shell).Do(ctx)
			return err
		}))
	}

	if len(actions) == 0 {
		return nil
	}
	return runCDPWithSoftTimeout(ctx, BrowserPoolCDPActionTimeout, actions...)
}

// resolvePersonaOrNil 从 personaID 解析 persona;空/未知返回 nil(纯指纹,向后兼容)。
// 让 personaID 字符串走与 presetID 同一条 rails,在各身份应用站点统一解析施加。
func resolvePersonaOrNil(personaID string) *Persona {
	if personaID == "" {
		return nil
	}
	if p, err := ResolvePersona(personaID); err == nil {
		return p
	}
	return nil
}

// acceptLanguageFor 把 locale 映射为 Accept-Language 头值。
func acceptLanguageFor(locale string) string {
	switch locale {
	case "zh-CN":
		return "zh-CN,zh;q=0.9,en;q=0.8"
	case "":
		return "en-US,en"
	default:
		return locale + "," + splitLangBase(locale) + ";q=0.9,en;q=0.8"
	}
}

// icuLocale 把 BCP-47(zh-CN)转 ICU C locale(zh_CN),供 setLocaleOverride 用。
func icuLocale(locale string) string {
	out := make([]byte, 0, len(locale))
	for i := 0; i < len(locale); i++ {
		if locale[i] == '-' {
			out = append(out, '_')
		} else {
			out = append(out, locale[i])
		}
	}
	return string(out)
}

func splitLangBase(locale string) string {
	for i := 0; i < len(locale); i++ {
		if locale[i] == '-' {
			return locale[:i]
		}
	}
	return locale
}
