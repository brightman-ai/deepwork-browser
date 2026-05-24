// identity_test.go — Phase_v2_1 单元测试 (TC-09-U-38/39/40/48)
//
// LAW-17: 本文件为 internal/browser 包内 L1 单元测试 (无 HTTP 边界).
// LAW-01: 测试失败 → 修代码, 禁止弱化断言.
// LAW-06: 副作用类断言含负向 + 幂等性探针.
package browser

import (
	"context"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// 公共 fixture
// ----------------------------------------------------------------------------

func presetA() Preset {
	return Preset{
		UserAgent:      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36",
		Viewport:       Viewport{Width: 1440, Height: 900, DPR: 2.0},
		Locale:         "en-US",
		Timezone:       "America/New_York",
		FingerprintTag: "macos-chrome-126",
	}
}

func presetB() Preset {
	p := presetA()
	p.Locale = "zh-CN" // 与 presetA 唯一差异
	return p
}

// fakePolicy — 测试用非 noop policy (验证 PolicyKey 参与 hash).
type fakePolicy struct{ key string }

func (f fakePolicy) PolicyKey() string { return f.key }
func (fakePolicy) Apply(_ context.Context, _ BrowserSessionHandle) error {
	return nil
}

// ============================================================
// TC-09-U-38: IdentityRegistry.Resolve 确定性 — 同三元组返回同 IdentityKey
// 来源: T6 §B1, CAP-BS09-C4 §2.bis 确定性约束
// 实施级聚焦: NewIdentityKey 是 Resolve 的核心算法, 此 TC 在算法层守护
// ============================================================
func Test_TC09_U_38_IdentityKey_Deterministic(t *testing.T) {
	profile := "default"
	preset := presetA()
	policy := NoopPolicy{}

	k1 := NewIdentityKey(profile, preset, policy)
	k2 := NewIdentityKey(profile, preset, policy)
	k3 := NewIdentityKey(profile, preset, policy)

	// 正向断言: 三次调用必须字节相等
	if k1 != k2 || k2 != k3 {
		t.Fatalf("IdentityKey not deterministic: k1=%q k2=%q k3=%q", k1, k2, k3)
	}
	if k1 == "" {
		t.Fatalf("IdentityKey empty for valid triple")
	}
	if got := len(string(k1)); got != 32 {
		t.Fatalf("IdentityKey hex length = %d, want 32 (16 bytes)", got)
	}
}

// ============================================================
// TC-09-U-39: IdentityKey hash collision — 任一维度变化 → key 必变 (3 维正交)
// 来源: T6 §B1, CAP-BS09-C4 §2.bis DDC-10
// ============================================================
func Test_TC09_U_39_IdentityKey_ThreeAxisOrthogonal(t *testing.T) {
	baseProfile := "default"
	basePreset := presetA()
	basePolicy := NoopPolicy{}
	baseKey := NewIdentityKey(baseProfile, basePreset, basePolicy)

	// 维度 1: profile 变化
	keyDiffProfile := NewIdentityKey("alt-profile", basePreset, basePolicy)
	if keyDiffProfile == baseKey {
		t.Errorf("profile change did not produce different key: %q == %q", keyDiffProfile, baseKey)
	}

	// 维度 2: preset 变化
	keyDiffPreset := NewIdentityKey(baseProfile, presetB(), basePolicy)
	if keyDiffPreset == baseKey {
		t.Errorf("preset change did not produce different key: %q == %q", keyDiffPreset, baseKey)
	}

	// 维度 3: policy 变化 (NoopPolicy → fakePolicy{key:"proxy-x"})
	keyDiffPolicy := NewIdentityKey(baseProfile, basePreset, fakePolicy{key: "proxy-x"})
	if keyDiffPolicy == baseKey {
		t.Errorf("policy change did not produce different key: %q == %q", keyDiffPolicy, baseKey)
	}

	// 负向断言: 三个变化彼此也不相同 (不只是与 base 不同)
	if keyDiffProfile == keyDiffPreset || keyDiffPreset == keyDiffPolicy || keyDiffProfile == keyDiffPolicy {
		t.Errorf("orthogonal changes collided: dp=%q dpre=%q dpol=%q",
			keyDiffProfile, keyDiffPreset, keyDiffPolicy)
	}
}

// ============================================================
// TC-09-U-40 (extended): IdentityKey 在不同 fakePolicy 配置下不冲突
// 验证 PolicyKey() 内容真实参与 hash (而非只看类型)
// ============================================================
func Test_TC09_U_40_IdentityKey_PolicyContentSensitive(t *testing.T) {
	profile := "default"
	preset := presetA()

	keyProxyA := NewIdentityKey(profile, preset, fakePolicy{key: "proxy-A"})
	keyProxyB := NewIdentityKey(profile, preset, fakePolicy{key: "proxy-B"})

	if keyProxyA == keyProxyB {
		t.Fatalf("policy content variation collapsed in IdentityKey: A=%q B=%q", keyProxyA, keyProxyB)
	}

	// 幂等性探针: 同 policy 第二次仍返回同 key
	keyProxyA2 := NewIdentityKey(profile, preset, fakePolicy{key: "proxy-A"})
	if keyProxyA != keyProxyA2 {
		t.Fatalf("idempotency violated: keyProxyA=%q keyProxyA2=%q", keyProxyA, keyProxyA2)
	}
}

// ============================================================
// TC-09-U-48: IsolationPolicy NoopPolicy stub
// 来源: T6 §B1, CAP-BS09-C4 §2.bis D9 SL-2
// 断言: NoopPolicy 实现 IsolationPolicy 接口, PolicyKey="noop", Apply 不报错
// ============================================================
func Test_TC09_U_48_NoopPolicy_Stub(t *testing.T) {
	var p IsolationPolicy = NoopPolicy{} // 编译期断言: NoopPolicy 实现接口

	// 正向断言: PolicyKey 必须是 "noop"
	if got := p.PolicyKey(); got != "noop" {
		t.Errorf("NoopPolicy.PolicyKey() = %q, want \"noop\"", got)
	}

	// 正向断言: Apply 返回 nil error, 不 panic
	if err := p.Apply(context.Background(), nil); err != nil {
		t.Errorf("NoopPolicy.Apply() returned err = %v, want nil", err)
	}

	// 幂等性探针: 多次调用 Apply 不应改变状态 (本身就是 no-op)
	for i := 0; i < 3; i++ {
		if err := p.Apply(context.Background(), nil); err != nil {
			t.Errorf("NoopPolicy.Apply() iter %d returned err = %v", i, err)
		}
	}
}

// ============================================================
// 边界: nil policy 容错 (NewIdentityKey 不应 panic)
// 来源: identity.go 设计自检
// ============================================================
func Test_NewIdentityKey_NilPolicy_TreatedAsNoop(t *testing.T) {
	keyNil := NewIdentityKey("default", presetA(), nil)
	keyNoop := NewIdentityKey("default", presetA(), NoopPolicy{})

	// 正向断言: nil policy 应等同 NoopPolicy (向后兼容)
	if keyNil != keyNoop {
		t.Fatalf("nil policy != NoopPolicy: nil=%q noop=%q", keyNil, keyNoop)
	}
}

// ============================================================
// Preset.canonical 稳定性: 同字段值 → 同字符串 (避免 map 顺序漂移)
// ============================================================
func Test_Preset_Canonical_Stable(t *testing.T) {
	p1 := presetA()
	p2 := presetA()
	c1 := p1.canonical()
	c2 := p2.canonical()
	if c1 != c2 {
		t.Errorf("canonical not stable: %q vs %q", c1, c2)
	}
	// 必须包含所有 5 个字段标记
	for _, marker := range []string{"ua=", "vp=", "loc=", "tz=", "fp="} {
		if !strings.Contains(c1, marker) {
			t.Errorf("canonical missing marker %q in %q", marker, c1)
		}
	}
}
