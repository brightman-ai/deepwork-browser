// identity_registry_test.go — Phase_v2_1 单元测试 (TC-09-U-41 等)
package browser

import (
	"errors"
	"sync"
	"testing"
)

// ============================================================
// TC-09-U-41: IdentityRegistry.Resolve 幂等性
// 来源: T6 §B1 (CAP-BS09-C4 §2.bis 确定性 + 不重复注册)
// 断言: 同 id 二次调用返回同 key + store 大小不变
// ============================================================
func Test_TC09_U_41_Registry_ResolveIdempotent(t *testing.T) {
	reg := NewIdentityRegistry()
	profile := "default"
	preset := presetA()
	policy := NoopPolicy{}

	k1, err := reg.Resolve(profile, preset, policy)
	if err != nil {
		t.Fatalf("first Resolve err = %v", err)
	}
	sizeAfter1 := len(reg.List())
	if sizeAfter1 != 1 {
		t.Fatalf("after 1st Resolve, List size = %d, want 1", sizeAfter1)
	}

	// 二次 Resolve 同三元组
	k2, err := reg.Resolve(profile, preset, policy)
	if err != nil {
		t.Fatalf("second Resolve err = %v", err)
	}
	if k1 != k2 {
		t.Errorf("Resolve not deterministic: k1=%q k2=%q", k1, k2)
	}

	sizeAfter2 := len(reg.List())
	if sizeAfter2 != 1 {
		t.Errorf("after 2nd Resolve same triple, List size = %d, want 1 (no duplicate registration)", sizeAfter2)
	}

	// 第三次 — 幂等性探针
	k3, _ := reg.Resolve(profile, preset, policy)
	if k3 != k1 {
		t.Errorf("3rd Resolve key drift: k3=%q k1=%q", k3, k1)
	}
	if got := len(reg.List()); got != 1 {
		t.Errorf("after 3rd Resolve, List size = %d, want 1", got)
	}
}

// ============================================================
// TC-09-U-41 (extended): 不同三元组 → 不同 key + store 增长
// ============================================================
func Test_Registry_ResolveDifferentTriples_Grows(t *testing.T) {
	reg := NewIdentityRegistry()

	k1, _ := reg.Resolve("prof-a", presetA(), NoopPolicy{})
	k2, _ := reg.Resolve("prof-b", presetA(), NoopPolicy{})
	k3, _ := reg.Resolve("prof-a", presetB(), NoopPolicy{})

	// 正向: 三个不同三元组 → 三个不同 key
	if k1 == k2 || k2 == k3 || k1 == k3 {
		t.Fatalf("different triples produced colliding keys: k1=%q k2=%q k3=%q", k1, k2, k3)
	}

	// 正向: List 大小 = 3
	if got := len(reg.List()); got != 3 {
		t.Errorf("List size = %d, want 3", got)
	}
}

// ============================================================
// Inspect: 命中 + 未命中
// ============================================================
func Test_Registry_Inspect_HitMiss(t *testing.T) {
	reg := NewIdentityRegistry()
	k, _ := reg.Resolve("default", presetA(), NoopPolicy{})

	desc, err := reg.Inspect(k)
	if err != nil {
		t.Fatalf("Inspect existing key err = %v", err)
	}
	if desc == nil {
		t.Fatal("Inspect returned nil descriptor for existing key")
	}
	// 正向: 反查内容与 Resolve 输入一致
	if desc.Key != k {
		t.Errorf("desc.Key = %q, want %q", desc.Key, k)
	}
	if desc.Profile != "default" {
		t.Errorf("desc.Profile = %q, want \"default\"", desc.Profile)
	}
	if desc.Preset.Locale != "en-US" {
		t.Errorf("desc.Preset.Locale = %q, want \"en-US\"", desc.Preset.Locale)
	}
	if desc.Policy == nil || desc.Policy.PolicyKey() != "noop" {
		t.Errorf("desc.Policy invalid: %+v", desc.Policy)
	}

	// 负向断言: 未注册 key → ErrIdentityNotFound
	_, err = reg.Inspect(IdentityKey("nonexistent-key-xxxx"))
	if !errors.Is(err, ErrIdentityNotFound) {
		t.Errorf("Inspect missing key err = %v, want ErrIdentityNotFound", err)
	}
}

// ============================================================
// List: 排序稳定性 (字典序)
// ============================================================
func Test_Registry_List_SortedDeterministic(t *testing.T) {
	reg := NewIdentityRegistry()

	// 注册顺序: c, a, b (打乱)
	reg.Resolve("p-c", presetA(), NoopPolicy{})
	reg.Resolve("p-a", presetA(), NoopPolicy{})
	reg.Resolve("p-b", presetA(), NoopPolicy{})

	got1 := reg.List()
	got2 := reg.List()

	if len(got1) != 3 {
		t.Fatalf("List size = %d, want 3", len(got1))
	}
	// 两次 List 必须返回同样顺序
	for i := range got1 {
		if got1[i].Key != got2[i].Key {
			t.Errorf("List order unstable at %d: %q vs %q", i, got1[i].Key, got2[i].Key)
		}
	}
	// 字典序校验
	for i := 1; i < len(got1); i++ {
		if got1[i-1].Key > got1[i].Key {
			t.Errorf("List not sorted: [%d]=%q > [%d]=%q", i-1, got1[i-1].Key, i, got1[i].Key)
		}
	}
}

// ============================================================
// 并发安全: 50 goroutine 并发 Resolve 同三元组 → store 大小仍为 1
// LAW-06: 副作用幂等性 (并发版)
// ============================================================
func Test_Registry_ConcurrentResolve_NoDuplicates(t *testing.T) {
	reg := NewIdentityRegistry()
	var wg sync.WaitGroup
	const N = 50

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = reg.Resolve("default", presetA(), NoopPolicy{})
		}()
	}
	wg.Wait()

	if got := len(reg.List()); got != 1 {
		t.Errorf("after %d concurrent Resolve, List size = %d, want 1", N, got)
	}
}

// ============================================================
// nil policy 容错: Resolve(profile, preset, nil) 等同 NoopPolicy
// ============================================================
func Test_Registry_Resolve_NilPolicyDefaultsToNoop(t *testing.T) {
	reg := NewIdentityRegistry()
	k1, err := reg.Resolve("default", presetA(), nil)
	if err != nil {
		t.Fatalf("Resolve with nil policy err = %v", err)
	}
	k2, _ := reg.Resolve("default", presetA(), NoopPolicy{})
	if k1 != k2 {
		t.Errorf("nil policy key %q != NoopPolicy key %q", k1, k2)
	}
	// 不应注册两个 entry
	if got := len(reg.List()); got != 1 {
		t.Errorf("nil + NoopPolicy registered as 2 entries (got %d)", got)
	}
}
