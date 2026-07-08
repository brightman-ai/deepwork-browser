// Package browser — Persona: 测试保真环境的唯一一等组合对象。
//
// Persona 回答"页面感知到什么"(WHAT the page perceives),与另两轴正交:
//   - scenario  → 会话怎么行为(远程写/确定性/headless) —— 策略/安全轴
//   - profile   → 用哪个存储桶/登录态 —— 存储/隔离轴
//
// Persona 由正交 facet 组合而成,并叠一个 Posture(stealth):
//
//	Persona = compose(Fingerprint, Shell, Network, Env) + Posture(stealth)
//
// SSOT 铁律:
//   - 身份指纹只有一个类型 = FingerprintPreset(browser/fingerprint.go),
//     Persona.Fingerprint 只持有其 ID,设备硬件数据不在此重复(避免第二份表)。
//   - Persona 是组合层,不是"第二个身份类型";它 subsume 旧 DevicePreset(已删)
//     与 FingerprintPreset(= Fingerprint facet)。
//   - stealth 是 Posture(布尔),不是一个 preset 种类。反检测(stealth)与测试
//     保真(fidelity)是两个相反目标:stealth 装成普通健全桌面躲检测;fidelity
//     忠实装成特定/受限环境逼出被测 app 的适配路径。二者共用注入机制、不共用默认。
package browser

import (
	"fmt"
	"sort"
	"strings"
)

// ShellKind — in-app webview 壳身份。空 = 普通浏览器(无壳)。
type ShellKind string

const (
	ShellNone   ShellKind = ""
	ShellWeChat ShellKind = "wechat" // 微信内置浏览器
	ShellWeCom  ShellKind = "wecom"  // 企业微信内置浏览器
)

// shellUASuffix — 壳追加到设备 base UA 的 in-app token。
// 让 Fingerprint 保持纯设备身份(可跨壳复用),壳只当"包装层"(DRY):
// iPhone 指纹 + wechat 壳 = iPhone UA + MicroMessenger。
// 企业微信 UA 同含 wxwork 与 micromessenger(wxwork 需排在前,匹配 detectInAppBrowser 顺序)。
var shellUASuffix = map[ShellKind]string{
	ShellWeChat: " MicroMessenger/8.0.49(0x18003123) NetType/WIFI Language/zh_CN",
	ShellWeCom:  " wxwork/4.1.24 MicroMessenger/8.0.49(0x18003123) NetType/WIFI Language/zh_CN",
}

// ShellProfile — 壳 facet:in-app 身份 + 能力破损(L4/L5)。
// v1 CP1:仅 Kind(驱动 UA 后缀)。能力破损/bridge 对象的实际注入在 CP4 realize。
type ShellProfile struct {
	Kind ShellKind
	// CP4 realize 时补:BreakServiceWorker/BreakClipboard/BreakNotification bool、bridge 对象。
	// 忠实 webview 该让 SW 注册失败/推送/剪贴板不可用,逼出被测 app 降级路径。
}

// NetworkProfile — 网络 facet(L6):节流。空 = 不节流。
// v1 CP3 用 Network.emulateNetworkConditions realize。
type NetworkProfile struct {
	Kind string // "" | "offline" | "slow-3g" | "fast-3g" | "4g"
}

// EnvProfile — 环境 facet(L7):时区/locale/配色。空字段 = 不覆盖。
// v1 CP3 用 Emulation.setTimezoneOverride/setLocaleOverride/setEmulatedMedia realize。
type EnvProfile struct {
	ColorScheme string // "" | "dark" | "light"
	Timezone    string // 如 "Asia/Shanghai"
	Locale      string // 如 "zh-CN"
}

// Persona — 身份轴唯一一等组合对象。
type Persona struct {
	ID          string
	Name        string
	Fingerprint string // FingerprintPreset ID(身份指纹 facet,SSOT = BuiltinPresets)
	Shell       ShellProfile
	Network     NetworkProfile
	Env         EnvProfile
	Stealth     bool // Posture:反检测姿态(正交)。fidelity persona 默认 false。
}

// fingerprint 返回本 persona 的 Fingerprint facet(身份指纹 SSOT 查表)。
func (p *Persona) fingerprint() *FingerprintPreset {
	if p == nil {
		return nil
	}
	return BuiltinPresets[NormalizePresetID(p.Fingerprint)]
}

// EffectiveUserAgent = 设备 base UA + 壳 UA 后缀。
func (p *Persona) EffectiveUserAgent() string {
	fp := p.fingerprint()
	ua := ""
	if fp != nil {
		ua = fp.UserAgent
	}
	return ua + shellUASuffix[p.Shell.Kind]
}

// EffectiveViewport 返回设备 facet 的视口与 DPR。
func (p *Persona) EffectiveViewport() (w, h int, dpr float64) {
	if fp := p.fingerprint(); fp != nil {
		return fp.ViewportW, fp.ViewportH, fp.DeviceScaleFactor
	}
	return 0, 0, 0
}

// Touch 是否触摸设备(来自 Fingerprint facet)。
func (p *Persona) Touch() bool {
	if fp := p.fingerprint(); fp != nil {
		return fp.Touch
	}
	return false
}

// FingerprintID 返回身份指纹 preset ID(流入既有 PresetID 管道)。
func (p *Persona) FingerprintID() string {
	if p == nil {
		return ""
	}
	return NormalizePresetID(p.Fingerprint)
}

// Personas — 命名 persona 注册表(组合元组,不含硬件数据)。
// 三类:① 迁移自旧 6 个 stealth 指纹(Posture:stealth);② 迁移自旧 5 个 DevicePreset
// (fidelity,无 stealth);③ 3 个新保真人格(壳/环境组合)。
var Personas = map[string]*Persona{
	// ① 桌面/移动 stealth 指纹迁移(旧 --preset ID 别名,Posture:stealth,行为零回退)
	PresetWindowsChrome:  {ID: PresetWindowsChrome, Name: "Windows 11 · Chrome (stealth)", Fingerprint: PresetWindowsChrome, Stealth: true},
	PresetLinuxChrome:    {ID: PresetLinuxChrome, Name: "Ubuntu · Chrome (stealth)", Fingerprint: PresetLinuxChrome, Stealth: true},
	PresetMacOSChrome:    {ID: PresetMacOSChrome, Name: "macOS · Chrome (stealth)", Fingerprint: PresetMacOSChrome, Stealth: true},
	PresetAndroidChrome:  {ID: PresetAndroidChrome, Name: "Android · Chrome (stealth)", Fingerprint: PresetAndroidChrome, Stealth: true},
	PresetMacOSSafariUA:  {ID: PresetMacOSSafariUA, Name: "macOS · Safari UA (stealth)", Fingerprint: PresetMacOSSafariUA, Stealth: true},
	PresetIPhoneSafariUA: {ID: PresetIPhoneSafariUA, Name: "iPhone · Safari UA (stealth)", Fingerprint: PresetIPhoneSafariUA, Stealth: true},

	// ② 旧 DevicePreset 迁移 → fidelity 设备人格(无 stealth)
	PresetIPhone14:    {ID: PresetIPhone14, Name: "iPhone 14", Fingerprint: PresetIPhone14},
	PresetIPhone15Pro: {ID: PresetIPhone15Pro, Name: "iPhone 15 Pro", Fingerprint: PresetIPhone15Pro},
	PresetIPadAir:     {ID: PresetIPadAir, Name: "iPad (Pro 11)", Fingerprint: PresetIPadAir},
	PresetPixel7:      {ID: PresetPixel7, Name: "Pixel 7", Fingerprint: PresetPixel7},
	PresetGalaxyS24:   {ID: PresetGalaxyS24, Name: "Galaxy S24", Fingerprint: PresetGalaxyS24},

	// ③ 新保真人格(D3 三人格)
	"wechat-iphone": {
		ID: "wechat-iphone", Name: "微信内置浏览器 · iPhone",
		Fingerprint: PresetIPhone15Pro,
		Shell:       ShellProfile{Kind: ShellWeChat},
	},
	"wecom-android": {
		ID: "wecom-android", Name: "企业微信内置浏览器 · Android",
		Fingerprint: PresetPixel7,
		Shell:       ShellProfile{Kind: ShellWeCom},
	},
	"desktop-cn-dark": {
		ID: "desktop-cn-dark", Name: "桌面 · 简体中文 · 暗色",
		Fingerprint: PresetMacOSChrome,
		Env:         EnvProfile{ColorScheme: "dark", Locale: "zh-CN", Timezone: "Asia/Shanghai"},
	},
}

// PersonaOrder — 展示/列表顺序(fidelity 人格在前,stealth 指纹别名在后)。
var PersonaOrder = []string{
	"wechat-iphone", "wecom-android", "desktop-cn-dark",
	PresetIPhone14, PresetIPhone15Pro, PresetIPadAir, PresetPixel7, PresetGalaxyS24,
	PresetWindowsChrome, PresetLinuxChrome, PresetMacOSChrome, PresetAndroidChrome,
	PresetMacOSSafariUA, PresetIPhoneSafariUA,
}

// ResolvePersona 返回已知 persona;未知值显式失败(fail-closed,不静默回退)。
func ResolvePersona(id string) (*Persona, error) {
	id = strings.TrimSpace(id)
	p, ok := Personas[id]
	if !ok {
		return nil, fmt.Errorf("unknown persona %q (valid: %s)", id, strings.Join(personaIDsSorted(), ", "))
	}
	return p, nil
}

func personaIDsSorted() []string {
	ids := make([]string, 0, len(Personas))
	for id := range Personas {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
