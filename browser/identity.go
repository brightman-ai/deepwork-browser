// Package browser — identity 三元组类型定义 [v2 , MERGED]
//
// 来源:
// - (IdentityRegistry / IsolationPolicy interface 契约)
// - (identity 三元组 (Profile, Preset, IsolationPolicy) → IdentityKey)
// - ( 必读数据契约)
//
// 范围 :
// - 类型定义: IdentityKey / Preset / Viewport / IdentityDescriptor / IsolationPolicy / NoopPolicy / Identity / BrowserSessionHandle 占位
// - IdentityKey 工厂函数: 三元组 → 确定性 hash
//
// 不在范围 (deferred to /v2_3/v2_4):
// - Pool 集成 : identity-keyed Chrome 实例池
// - 6 入口迁移 : webui/tool/dw-browser/council/live_sync/desktop 走 Pool API
// - ProxyPolicy 首例 ( P1): 当前只有 NoopPolicy stub (D9 SL-2)
//
// 推迟标记 (T5 §0 D9 SL-2):
// - 推迟项: ProxyPolicy 实装
// - 理由分类: 认知未闭合 (J3 默认值待 Round 7+ 冻结)
// - 触发条件: 启动 + J3 默认值确认
// - 验证标准: 之外新增 ProxyPolicy.Apply 走 CDP Network.setProxy
package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// ============================================================
// § Identity 三元组类型
// ============================================================

// IdentityKey — identity 三元组的确定性 hash, 稳定可比较 + 跨进程一致.
// 实现: SHA-256(profile|preset.canonical|policy.PolicyKey) 截断 32 字节 hex.
//
// 不变式:
// - 同三元组多次调用 NewIdentityKey 必须返回同一 IdentityKey (字节相等).
// - 三个维度任一变化 → IdentityKey 必变 (D1 + 3 维正交).
type IdentityKey string

// Preset — 浏览器表观属性集 (UA / viewport / locale / timezone / 指纹 runtime tag).
//
// 注意: 字段顺序参与 canonical 序列化, 不可随意调整 (会破坏 IdentityKey 稳定性).
type Preset struct {
	UserAgent string // navigator.userAgent
	Viewport Viewport // 视口尺寸 + DPR
	Locale string // navigator.language (e.g. "en-US")
	Timezone string // IANA tz name (e.g. "America/New_York")
	FingerprintTag string // 指纹 runtime tag (e.g. "macos-chrome-126")
}

// Viewport — 视口尺寸 + 设备像素比.
type Viewport struct {
	Width int
	Height int
	DPR float64
}

// canonical 返回 Preset 的稳定字符串表示, 用于参与 IdentityKey hash.
//
// 格式: "ua=...|vp=WxH@DPR|loc=...|tz=...|fp=..."
// 不依赖 JSON / encoding/gob 实现细节 (避免 runtime 行为漂移).
func (p Preset) canonical string {
	return fmt.Sprintf("ua=%s|vp=%dx%d@%.2f|loc=%s|tz=%s|fp=%s"
		p.UserAgent
		p.Viewport.Width, p.Viewport.Height, p.Viewport.DPR
		p.Locale
		p.Timezone
		p.FingerprintTag
	)
}

// IdentityDescriptor — IdentityKey 反查结果 (Inspect / List 用).
type IdentityDescriptor struct {
	Key IdentityKey
	Profile string // 物理身份 (profileID, 与 user-data-dir 一一对应)
	Preset Preset // UA / viewport / locale / timezone / 指纹
	Policy IsolationPolicy // 隔离策略 (: 仅 NoopPolicy)
}

// Identity — 三元组逻辑聚合 (便于上层调用方按值传递).
//
// 注意: Identity 本身不参与 hash, IdentityKey 由 NewIdentityKey(profile, preset, policy)
// 三参数计算; 此结构体只是便利封装.
type Identity struct {
	Profile string
	Preset Preset
	Policy IsolationPolicy
}

// ============================================================
// § IsolationPolicy 接口
// ============================================================

// BrowserSessionHandle — IsolationPolicy.Apply 的 session 句柄占位.
//
// : 接口型空 marker, 由 BrowserSession 实现 (e.g. 暴露 CDP target / 配置写入接口).
// 这里仅声明类型, 让 IsolationPolicy 接口可编译, 不约束方法集.
type BrowserSessionHandle interface{}

// IsolationPolicy — identity 第 3 正交维度 (, D9 SL-2).
//
// : 仅 NoopPolicy stub (P0 skeleton).
// : ProxyPolicy 首例 (J3 默认值, P1 deferred).
type IsolationPolicy interface {
	// PolicyKey 返回稳定 key 参与 IdentityKey hash.
	// 同一类型 policy 同一配置 → 必返回同一 key (确定性).
	PolicyKey string

	// Apply 在 Chrome 启动后通过 CDP 把 policy 落地 (proxy / permission / download / extension / geo / compliance).
	// : NoopPolicy.Apply 直接返回 nil.
	Apply(ctx context.Context, session BrowserSessionHandle) error
}

// NoopPolicy — 默认隔离策略, 不施加任何额外约束.
//
// 不变式: 所有 NoopPolicy 实例 PolicyKey 相同 (= "noop"), 因此 (profile, preset, NoopPolicy{})
// 三元组的 IdentityKey 也相同, 实现"默认 identity 即 (profile, preset)"语义.
type NoopPolicy struct{}

// PolicyKey 返回稳定常量 "noop".
func (NoopPolicy) PolicyKey string { return "noop" }

// Apply 是 no-op, 直接返回 nil (不修改 session).
func (NoopPolicy) Apply(_ context.Context, _ BrowserSessionHandle) error { return nil }

// ============================================================
// § IdentityKey 工厂
// ============================================================

// NewIdentityKey 把三元组归一化为 IdentityKey (确定性 SHA-256 hash 截断).
//
// 算法:
//
//	raw = "profile=" + profile + "||preset=" + preset.canonical + "||policy=" + policy.PolicyKey
//	hash = sha256(raw)
//	key = hex(hash[:16]) // 32 字符, 128 bit, 抗碰撞充足
//
// 约束 ( 守护):
// - 同三元组多次调用必须返回字节相等的 IdentityKey.
// - policy=nil 时按 NoopPolicy 处理 (不会 panic), 但调用方应显式传 NoopPolicy{}.
func NewIdentityKey(profile string, preset Preset, policy IsolationPolicy) IdentityKey {
	policyKey := "noop"
	if policy != nil {
		policyKey = policy.PolicyKey
	}
	raw := "profile=" + profile +
		"||preset=" + preset.canonical +
		"||policy=" + policyKey
	sum := sha256.Sum256(byte(raw))
	return IdentityKey(hex.EncodeToString(sum[:16]))
}
