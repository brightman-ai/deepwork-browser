// Package browser — v2 BrowserPool Phase_v2_2 测试矩阵.
//
// 绑定 TC (T6 v2):
//   - TC-09-U-40: BrowserPool keying — 同 identity 多 acquire 复用同 entry (单元: 校验错误路径)
//   - TC-09-U-41: BrowserPool Role 校验 — Role 不在 4 枚举 → ErrInvalidRole (无 Chrome 启动)
//   - TC-09-U-42: council Role 必须配合 council-* profile (P1)
//   - TC-09-U-43: Tab 四元身份唯一性 (covered in tab_index_test.go)
//   - TC-09-I-47/48: Pool e2e (需 Chrome — Environment Gate skip 兜底)
//   - 辅助: LegacyAcquireTab 行为对齐 r1/r2 (TC-09-I-36 shim 验证)
//   - LAW-04 N±1: AcquireTab → IdentityRegistry → TabIndex 全链
package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
)

// ============================================================
// § L1 单元: 入参校验 (无 Chrome 启动)
// ============================================================

// TestPool_TC09U41_RoleValidation — Role 不在 4 枚举矩阵 → ErrInvalidRole + 不启动 Chrome.
//
// 校验顺序: AcquireTab 必须先 IsValidRole 校验 (无副作用), 再 lazy launch.
// LAW-06 负向断言: 无效 Role 调用后 Pool 内部状态完全不变 (entries 仍空, tabIndex.Len()=0).
func TestPool_TC09U41_RoleValidation_NoSideEffect(t *testing.T) {
	pool := NewBrowserPool(PoolConfig{DataDir: t.TempDir(), Mode: BrowserModeHeadless})
	defer pool.Shutdown(context.Background())

	// 预先 Resolve 一个 valid identity (校验顺序: Role 校验在 IdentityKey 校验之前)
	key, err := pool.Registry().Resolve("default", Preset{}, NoopPolicy{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	cases := []TabRole{TabRole(""), TabRole("robot"), TabRole("HUMAN"), TabRole("admin")}
	for _, role := range cases {
		_, err := pool.AcquireTab(context.Background(), AcquireTabRequest{
			IdentityKey: key,
			WorkspaceID: "ws-0",
			Role:        role,
		})
		if err != ErrInvalidRole {
			t.Fatalf("Role=%q: expected ErrInvalidRole, got %v", role, err)
		}
	}

	// 正向断言: Pool 内部状态零副作用 (无 entry / 无 tab)
	snap := pool.Inspect()
	if snap.TotalTabs != 0 {
		t.Fatalf("Inspect after invalid roles: expected TotalTabs=0, got %d", snap.TotalTabs)
	}
	if len(snap.Identities) != 0 {
		t.Fatalf("Inspect after invalid roles: expected 0 identities, got %d", len(snap.Identities))
	}
}

// TestPool_TC09U41_IdentityKeyEmpty — 空 IdentityKey → ErrIdentityUnresolved.
func TestPool_TC09U41_EmptyIdentityKey(t *testing.T) {
	pool := NewBrowserPool(PoolConfig{DataDir: t.TempDir(), Mode: BrowserModeHeadless})
	defer pool.Shutdown(context.Background())

	_, err := pool.AcquireTab(context.Background(), AcquireTabRequest{
		IdentityKey: "",
		WorkspaceID: "ws-0",
		Role:        RoleAgent,
	})
	if err != ErrIdentityUnresolved {
		t.Fatalf("empty key: expected ErrIdentityUnresolved, got %v", err)
	}
}

// TestPool_TC09U41_UnregisteredIdentityKey — 未通过 Registry.Resolve 的 IdentityKey → ErrIdentityUnresolved.
//
// 字符串拼装 IdentityKey 是 CAP §2.bis 明确禁止的 anti-pattern, 此 TC 守护.
func TestPool_TC09U41_UnregisteredIdentityKey(t *testing.T) {
	pool := NewBrowserPool(PoolConfig{DataDir: t.TempDir(), Mode: BrowserModeHeadless})
	defer pool.Shutdown(context.Background())

	bogus := IdentityKey("hand-crafted-not-resolved")
	_, err := pool.AcquireTab(context.Background(), AcquireTabRequest{
		IdentityKey: bogus,
		WorkspaceID: "ws-0",
		Role:        RoleAgent,
	})
	if err == nil || !strings.Contains(err.Error(), "identity not resolved") {
		t.Fatalf("bogus key: expected unresolved error, got %v", err)
	}
}

// TestPool_TC09U42_CouncilRoleRequiresCouncilPrefix (P1) — council role 必须配合 council-* profile.
func TestPool_TC09U42_CouncilRoleRequiresCouncilProfile(t *testing.T) {
	pool := NewBrowserPool(PoolConfig{DataDir: t.TempDir(), Mode: BrowserModeHeadless})
	defer pool.Shutdown(context.Background())

	// Resolve 一个 NON-council profile
	key, err := pool.Registry().Resolve("agent-1", Preset{}, NoopPolicy{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	_, err = pool.AcquireTab(context.Background(), AcquireTabRequest{
		IdentityKey: key,
		WorkspaceID: "ws-0",
		Role:        RoleCouncil,
	})
	if err == nil {
		t.Fatalf("council role + non-council profile: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "council") {
		t.Fatalf("expected ErrInvalidRoleIdentity wrap, got %v", err)
	}

	// Inspect: zero side-effect
	if snap := pool.Inspect(); snap.TotalTabs != 0 {
		t.Fatalf("invalid council acquire should leave TotalTabs=0, got %d", snap.TotalTabs)
	}
}

// TestPool_PoolClosed_AcquireTabFails — Shutdown 后 AcquireTab → ErrPoolClosed.
func TestPool_PoolClosed_AcquireTabFails(t *testing.T) {
	pool := NewBrowserPool(PoolConfig{DataDir: t.TempDir(), Mode: BrowserModeHeadless})
	pool.Shutdown(context.Background())

	key, _ := pool.Registry().Resolve("default", Preset{}, NoopPolicy{})
	_, err := pool.AcquireTab(context.Background(), AcquireTabRequest{
		IdentityKey: key,
		Role:        RoleAgent,
	})
	if err != ErrPoolClosed {
		t.Fatalf("after shutdown: expected ErrPoolClosed, got %v", err)
	}
}

// TestPool_GracefulShutdown_Idempotent — 多次 Shutdown 安全.
func TestPool_GracefulShutdown_Idempotent(t *testing.T) {
	pool := NewBrowserPool(PoolConfig{DataDir: t.TempDir(), Mode: BrowserModeHeadless})
	if err := pool.GracefulShutdown(context.Background()); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	if err := pool.GracefulShutdown(context.Background()); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
	if err := pool.GracefulShutdown(context.Background()); err != nil {
		t.Fatalf("third shutdown: %v", err)
	}
}

// TestPool_Registry_Returned — Pool.Registry() 返回内部 registry, 与 Resolve 后 Inspect 一致.
//
// LAW-04: AcquireTab 必经 Registry.Inspect 反查; 此测试验证两者共享同一 store.
func TestPool_Registry_Returned(t *testing.T) {
	pool := NewBrowserPool(PoolConfig{DataDir: t.TempDir(), Mode: BrowserModeHeadless})
	defer pool.Shutdown(context.Background())

	reg := pool.Registry()
	key, err := reg.Resolve("p1", Preset{UserAgent: "UA"}, NoopPolicy{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	desc, err := reg.Inspect(key)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if desc.Profile != "p1" || desc.Preset.UserAgent != "UA" {
		t.Fatalf("Inspect mismatch: %+v", desc)
	}
}

// TestPool_DefaultIdentityKey_StableAcrossCalls — 默认 identity key 跨 Switch 调用前后稳定.
//
// 不变式: NewBrowserPool 内部 Resolve 出来的 default key 必须等于
// 同 (config.ProfileID, defaultPreset, NoopPolicy{}) 三元组的 NewIdentityKey 结果.
func TestPool_DefaultIdentityKey_DeterministicWithExternalCalc(t *testing.T) {
	cfg := PoolConfig{DataDir: t.TempDir(), ProfileID: "default", PresetID: DefaultPresetID()}
	pool := NewBrowserPool(cfg)
	defer pool.Shutdown(context.Background())

	expected := NewIdentityKey(NormalizeProfileID(cfg.ProfileID), defaultPresetForConfig(cfg), NoopPolicy{})
	if pool.defaultIdentityKey != expected {
		t.Fatalf("default key drift: pool=%s expected=%s", pool.defaultIdentityKey, expected)
	}

	// Inspect via registry
	desc, err := pool.Registry().Inspect(pool.defaultIdentityKey)
	if err != nil {
		t.Fatalf("Inspect default identity: %v", err)
	}
	if desc.Profile != NormalizeProfileID(cfg.ProfileID) {
		t.Fatalf("default identity profile mismatch: got %s", desc.Profile)
	}
}

func TestPool_ResolveProfileIdentity_UsesCanonicalDefaultPreset(t *testing.T) {
	cfg := PoolConfig{DataDir: t.TempDir(), ProfileID: "default", PresetID: DefaultPresetID(), Mode: BrowserModeHeadless}
	pool := NewBrowserPool(cfg)
	defer pool.Shutdown(context.Background())

	key, err := pool.ResolveProfileIdentity("default")
	if err != nil {
		t.Fatalf("ResolveProfileIdentity: %v", err)
	}
	if key != pool.DefaultIdentity() {
		t.Fatalf("ResolveProfileIdentity(default) = %s, want default identity %s", key, pool.DefaultIdentity())
	}

	legacyKey, err := pool.Registry().Resolve("default", Preset{FingerprintTag: DefaultPresetID()}, NoopPolicy{})
	if err != nil {
		t.Fatalf("legacy resolve: %v", err)
	}
	if legacyKey == key {
		t.Fatalf("legacy minimal preset unexpectedly matched canonical key; regression test no longer protects profile collision")
	}
}

// ============================================================
// § L2 集成: 需 Chrome (Environment Gate)
// ============================================================

// TestPool_TC09I47_TC09U40_SameIdentityReusesChrome — 同 identity 二次 Acquire 复用同一 Chrome (entry).
//
// TC-09-I-47 + TC-09-U-40: Pool keying 不变式.
// TC-09-I-48: lazy launch — 第 1 次 Acquire 启动 Chrome, 第 2 次同 key 复用.
//
// 正向断言:
//   - 两次 Acquire 后 Inspect TotalTabs == 2.
//   - len(Identities) == 1 (同 identity 共享一个 entry).
//   - Identities[0].Tabs[0].IdentityKey == Identities[0].Tabs[1].IdentityKey == key1.
func TestPool_TC09I47_SameIdentityReusesChromeEntry(t *testing.T) {
	requireChromeForPool(t)

	pool := NewBrowserPool(PoolConfig{DataDir: t.TempDir(), MaxTabs: 5, Mode: BrowserModeHeadless})
	defer pool.Shutdown(context.Background())

	key1, err := pool.Registry().Resolve("identity-A", Preset{FingerprintTag: DefaultPresetID()}, NoopPolicy{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	h1, err := pool.AcquireTab(context.Background(), AcquireTabRequest{IdentityKey: key1, WorkspaceID: "ws-A", Role: RoleAgent})
	if err != nil {
		t.Fatalf("AcquireTab #1: %v", err)
	}
	h2, err := pool.AcquireTab(context.Background(), AcquireTabRequest{IdentityKey: key1, WorkspaceID: "ws-B", Role: RoleAgent})
	if err != nil {
		t.Fatalf("AcquireTab #2: %v", err)
	}
	if h1.TargetID == h2.TargetID {
		t.Fatalf("TargetIDs should be unique per Tab; got both %s", h1.TargetID)
	}
	if h1.IdentityKey != h2.IdentityKey {
		t.Fatalf("Both tabs should share IdentityKey; got %s / %s", h1.IdentityKey, h2.IdentityKey)
	}

	snap := pool.Inspect()
	if snap.TotalTabs != 2 {
		t.Fatalf("Inspect TotalTabs: expected 2, got %d", snap.TotalTabs)
	}
	if len(snap.Identities) != 1 {
		t.Fatalf("Inspect Identities: expected 1 (shared chrome), got %d", len(snap.Identities))
	}
	if snap.Identities[0].Key != key1 {
		t.Fatalf("Inspect identity key mismatch: got %s, want %s", snap.Identities[0].Key, key1)
	}
	if len(snap.Identities[0].Tabs) != 2 {
		t.Fatalf("Inspect Tabs[0]: expected 2 tabs, got %d", len(snap.Identities[0].Tabs))
	}
}

// TestPool_TC09U40_DifferentIdentityCreatesIndependentChrome — 不同 identity 启动独立 Chrome (entry).
//
// LAW-06 负向断言对照: 同 identity 复用 vs 不同 identity 隔离.
func TestPool_TC09U40_DifferentIdentityCreatesIndependentEntry(t *testing.T) {
	requireChromeForPool(t)

	pool := NewBrowserPool(PoolConfig{DataDir: t.TempDir(), MaxTabs: 5, Mode: BrowserModeHeadless})
	defer pool.Shutdown(context.Background())

	keyA, _ := pool.Registry().Resolve("identity-A", Preset{FingerprintTag: DefaultPresetID()}, NoopPolicy{})
	keyB, _ := pool.Registry().Resolve("identity-B", Preset{FingerprintTag: DefaultPresetID()}, NoopPolicy{})
	if keyA == keyB {
		t.Fatalf("different profiles should yield different keys; got both %s", keyA)
	}

	if _, err := pool.AcquireTab(context.Background(), AcquireTabRequest{IdentityKey: keyA, WorkspaceID: "ws", Role: RoleAgent}); err != nil {
		t.Fatalf("Acquire A: %v", err)
	}
	if _, err := pool.AcquireTab(context.Background(), AcquireTabRequest{IdentityKey: keyB, WorkspaceID: "ws", Role: RoleAgent}); err != nil {
		t.Fatalf("Acquire B: %v", err)
	}

	snap := pool.Inspect()
	if len(snap.Identities) != 2 {
		t.Fatalf("Inspect Identities: expected 2 separate entries, got %d", len(snap.Identities))
	}
	if snap.TotalTabs != 2 {
		t.Fatalf("Inspect TotalTabs: expected 2, got %d", snap.TotalTabs)
	}
}

// TestPool_ReleaseTab_RemovesFromIndex — ReleaseTab 后 Inspect 不再包含该 tab.
func TestPool_ReleaseTab_RemovesFromIndex(t *testing.T) {
	requireChromeForPool(t)

	pool := NewBrowserPool(PoolConfig{DataDir: t.TempDir(), MaxTabs: 5, Mode: BrowserModeHeadless})
	defer pool.Shutdown(context.Background())

	key, _ := pool.Registry().Resolve("identity-X", Preset{FingerprintTag: DefaultPresetID()}, NoopPolicy{})
	h, err := pool.AcquireTab(context.Background(), AcquireTabRequest{IdentityKey: key, WorkspaceID: "ws", Role: RoleAgent})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if pool.Inspect().TotalTabs != 1 {
		t.Fatalf("expected TotalTabs=1 after acquire")
	}

	if err := pool.ReleaseTab(context.Background(), h.TargetID); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if pool.Inspect().TotalTabs != 0 {
		t.Fatalf("expected TotalTabs=0 after release, got %d", pool.Inspect().TotalTabs)
	}
	if err := pool.ReleaseTab(context.Background(), h.TargetID); err != ErrTabNotFound {
		t.Fatalf("double release: expected ErrTabNotFound, got %v", err)
	}
}

// TestPool_WebUIPanel_ReusesRootTarget — webui-panel 首次 acquire 应复用 entry 的 root target，
// 避免 human-mode service browser 平白多出一个空白 tab。
func TestPool_WebUIPanel_ReusesRootTarget(t *testing.T) {
	requireChromeForPool(t)

	pool := NewBrowserPool(PoolConfig{DataDir: t.TempDir(), MaxTabs: 5, Mode: BrowserModeHeadless})
	defer pool.Shutdown(context.Background())

	key, _ := pool.Registry().Resolve("identity-panel", Preset{FingerprintTag: DefaultPresetID()}, NoopPolicy{})
	h, err := pool.AcquireTab(context.Background(), AcquireTabRequest{
		IdentityKey: key,
		WorkspaceID: "webui-panel",
		Role:        RoleAgent,
	})
	if err != nil {
		t.Fatalf("AcquireTab: %v", err)
	}

	entry := pool.entries[key]
	if entry == nil {
		t.Fatalf("expected entry for key=%s", key)
	}
	if entry.rootTargetID == "" {
		t.Fatalf("expected root target id to be captured")
	}
	if h.TargetID != entry.rootTargetID {
		t.Fatalf("webui-panel should reuse root target: got=%s want=%s", h.TargetID, entry.rootTargetID)
	}
}

// TestPool_ReapIdleTabs_ClosesNonPanelTabs — 非 panel tab 到达 IdleTimeout 后应被回收，
// 避免 tool/council 类 caller 在 pool 中长期残留空白 target。
func TestPool_ReapIdleTabs_ClosesNonPanelTabs(t *testing.T) {
	requireChromeForPool(t)

	pool := NewBrowserPool(PoolConfig{DataDir: t.TempDir(), MaxTabs: 5, IdleTimeout: time.Second, Mode: BrowserModeHeadless})
	defer pool.Shutdown(context.Background())

	key, _ := pool.Registry().Resolve("identity-idle", Preset{FingerprintTag: DefaultPresetID()}, NoopPolicy{})
	h, err := pool.AcquireTab(context.Background(), AcquireTabRequest{
		IdentityKey: key,
		WorkspaceID: "tool-default",
		Role:        RoleAgent,
	})
	if err != nil {
		t.Fatalf("AcquireTab: %v", err)
	}

	entry := pool.entries[key]
	if entry == nil {
		t.Fatalf("expected entry for key=%s", key)
	}
	te := entry.tabs[h.TargetID]
	if te == nil {
		t.Fatalf("expected tab entry for target=%s", h.TargetID)
	}
	te.lastActive = time.Now().Add(-2 * time.Second)

	pool.reapIdleTabs()

	if _, ok := pool.GetCore(h.TargetID); ok {
		t.Fatalf("idle tab should be evicted from pool")
	}
	if pool.Inspect().TotalTabs != 0 {
		t.Fatalf("expected TotalTabs=0 after idle reap, got %d", pool.Inspect().TotalTabs)
	}
}

// ============================================================
// § Phase_v2_4: 双阶段退出 bounded wait + audit (CAP §3.5)
// ============================================================

// TestPool_envShutdownGraceSec — 验证 BS09_SHUTDOWN_GRACE_SEC 解析:
//   - 未设 → 默认 5s
//   - 合法值 → 该值
//   - 超过 30s → 截断 30s
//   - 非法 / ≤0 → 默认 5s
func TestPool_envShutdownGraceSec(t *testing.T) {
	cases := []struct {
		raw    string
		expect time.Duration
	}{
		{"", 5 * time.Second},
		{"1", 1 * time.Second},
		{"7", 7 * time.Second},
		{"30", 30 * time.Second},
		{"31", 30 * time.Second}, // 截断到上限
		{"100", 30 * time.Second},
		{"0", 5 * time.Second},   // ≤0 → 默认
		{"-1", 5 * time.Second},  // 负数 → 默认
		{"abc", 5 * time.Second}, // 非数 → 默认
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			t.Setenv(shutdownGraceEnv, c.raw)
			got := envShutdownGraceSec()
			if got != c.expect {
				t.Fatalf("env=%q: expected %v, got %v", c.raw, c.expect, got)
			}
		})
	}
}

// TestPool_GracefulShutdown_BoundedWaitGraceful — 持有 Tab 后 GracefulShutdown 走 graceful 路径,
// 在 grace 内退出 (无 audit event). 验证 happy path 不会被错误标记为 kill.
//
// 此 TC 守护 CAP §3.5 持久化 invariant: 正常退出走 chromedp.Cancel → Browser.close → flush.
func TestPool_GracefulShutdown_BoundedWaitGraceful(t *testing.T) {
	requireChromeForPool(t)

	pool := NewBrowserPool(PoolConfig{DataDir: t.TempDir(), Mode: BrowserModeHeadless})

	key, _ := pool.Registry().Resolve("default", Preset{}, NoopPolicy{})
	if _, err := pool.AcquireTab(context.Background(), AcquireTabRequest{
		IdentityKey: key, WorkspaceID: "ws", Role: RoleAgent,
	}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	t.Setenv(shutdownGraceEnv, "10") // 10s 充裕, headless chrome 应在 < 1s 退出
	start := time.Now()
	if err := pool.GracefulShutdown(context.Background()); err != nil {
		t.Fatalf("GracefulShutdown: %v", err)
	}
	elapsed := time.Since(start)

	// graceful 路径: < grace; 真实 headless 通常 < 2s
	if elapsed >= 10*time.Second {
		t.Fatalf("GracefulShutdown elapsed=%v exceeded grace (expected graceful exit)", elapsed)
	}
	t.Logf("GracefulShutdown graceful path: elapsed=%v (grace=10s)", elapsed)

	// 二次断言: shutdown 后 Inspect 为空
	snap := pool.Inspect()
	if snap.TotalTabs != 0 || len(snap.Identities) != 0 {
		t.Fatalf("post-shutdown Inspect not clean: %+v", snap)
	}
}

// TestPool_TargetTrackerCreateTab_RegistersSynchronously verifies /tabs/new's core invariant:
// Target.createTarget returning a target ID is sufficient to switch immediately, even if the
// asynchronous TargetCreated event is delayed or absent.
func TestPool_TargetTrackerCreateTab_RegistersSynchronously(t *testing.T) {
	requireChromeForPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := NewBrowserPool(PoolConfig{DataDir: t.TempDir(), MaxTabs: 5, Mode: BrowserModeHeadless})
	defer pool.Shutdown(context.Background())

	handle, err := pool.AcquireTab(ctx, AcquireTabRequest{
		IdentityKey: pool.DefaultIdentity(),
		WorkspaceID: "target-tracker-create-tab",
		Role:        RoleAgent,
	})
	if err != nil {
		t.Fatalf("AcquireTab: %v", err)
	}
	core, ok := pool.GetCore(handle.TargetID)
	if !ok {
		t.Fatalf("GetCore(%s) miss", handle.TargetID)
	}
	tracker := core.GetTargetTracker()
	if tracker == nil {
		t.Fatal("GetTargetTracker() nil")
	}

	newID, err := tracker.CreateTab("about:blank")
	if err != nil {
		t.Fatalf("CreateTab() error = %v", err)
	}
	if tracker.ActiveTargetID() != target.ID(newID) {
		t.Fatalf("ActiveTargetID() = %s, want %s", tracker.ActiveTargetID(), newID)
	}

	tabs := tracker.ListTargets()
	foundActive := false
	for _, tab := range tabs {
		if tab.ID == newID && tab.Active {
			foundActive = true
			break
		}
	}
	if !foundActive {
		t.Fatalf("ListTargets() does not contain active created tab %s: %+v", newID, tabs)
	}
}

func TestPool_TargetTrackerFollowsPageOpenedTarget(t *testing.T) {
	requireChromeForPool(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/child" {
			_, _ = w.Write([]byte("<!doctype html><title>child</title><main>child</main>"))
			return
		}
		_, _ = w.Write([]byte("<!doctype html><title>main</title><main>main</main>"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool := NewBrowserPool(PoolConfig{DataDir: t.TempDir(), MaxTabs: 5, Mode: BrowserModeHeadless})
	defer pool.Shutdown(context.Background())

	handle, err := pool.AcquireTab(ctx, AcquireTabRequest{
		IdentityKey: pool.DefaultIdentity(),
		WorkspaceID: "target-tracker-window-open",
		Role:        RoleAgent,
	})
	if err != nil {
		t.Fatalf("AcquireTab: %v", err)
	}
	core, ok := pool.GetCore(handle.TargetID)
	if !ok {
		t.Fatalf("GetCore(%s) miss", handle.TargetID)
	}
	if _, err := core.StartLiveView(ctx); err != nil {
		t.Fatalf("StartLiveView: %v", err)
	}
	defer core.StopLiveView(context.Background())

	if _, err := core.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	var opened bool
	childURL := srv.URL + "/child"
	if err := core.EvalJS(ctx, `(() => { setTimeout(() => window.open('`+childURL+`', '_blank'), 0); return true; })()`, &opened); err != nil {
		t.Fatalf("EvalJS window.open: %v", err)
	}
	if !opened {
		t.Fatal("window.open trigger returned false")
	}

	tracker := core.GetTargetTracker()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tracker.ActiveTargetID() != "" && tracker.TargetCount() > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("TargetTracker did not follow page-opened target; active=%s targets=%+v",
		tracker.ActiveTargetID(), tracker.ListTargets())
}

// requireChromeForPool — Environment Gate: 跑 Chrome 集成测试需本机有 Chrome.
func requireChromeForPool(t *testing.T) {
	t.Helper()
	if _, err := NewChromeLauncher().FindChrome(); err != nil {
		t.Skipf("Environment Gate: Chrome unavailable (%v) — Pool L2 test skipped", err)
	}
}
