//go:build !windows

package browser

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 真 Chrome 端到端（E4 + E5 + P2 合在一起）：
//
//	持久 profile 里设一个 max-age Cookie → 立刻 Close（不等 Chrome 自己的 30s 提交周期）
//	→ 回执 shutdown=graceful、profile 已封存、目录里只剩 2 个文件、磁盘上搜不到 Cookie 明文
//	→ 重新打开同一个 profile：先解封，Cookie 读得到。
//
// 之前的行为：Close 对进程组 SIGKILL，未提交的 Cookie 丢失（探针：Cookies 库 0 行）。

func serveCookieFixture(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>cookie</title><body>cookie fixture`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOwnedCorePersistentProfileKeepsCookieAcrossGracefulCloseAndSeal(t *testing.T) {
	requireChromeForPool(t)
	useTestKeyFile(t)
	srv := serveCookieFixture(t)
	profileID := fmt.Sprintf("persist-%d", time.Now().UnixNano())
	profileDir := filepath.Join(browserCLIDefaultBase(), profileID)
	t.Cleanup(func() { _ = RemoveCLIProfileDir(profileDir) })
	marker := fmt.Sprintf("PROBE-%d", time.Now().UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	core, err := NewBrowserCore(ctx, profileID, WithMode(ModeHeadless), WithPersistentProfile())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.Navigate(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	var set string
	if err := core.EvalJS(ctx, fmt.Sprintf(`document.cookie = "dwb_probe=%s; max-age=3600; path=/"; document.cookie`, marker), &set); err != nil || !strings.Contains(set, marker) {
		t.Fatalf("cookie not set: %q %v", set, err)
	}
	if err := core.Close(context.Background()); err != nil { // 紧接着关闭：不给 Chrome 自己的批量提交留时间
		t.Fatal(err)
	}
	rc := core.(*browserCoreImpl).LastCloseReceipt()
	if rc == nil || rc.Shutdown.Mode != ShutdownGraceful {
		t.Fatalf("close must exit normally for a persistent profile: %+v", rc)
	}
	if rc.Profile == nil || rc.Profile.Error != "" || rc.Profile.Vault != ProfileVaultSealed || rc.Profile.OSCrypt != ProfileOSCryptMock {
		t.Fatalf("profile receipt %+v", rc.Profile)
	}
	t.Logf("close receipt: shutdown=%s in %dms, seal %+v", rc.Shutdown.Mode, rc.Shutdown.Ms, rc.Profile.Seal)

	requireOnlyManifestAndVault(t, profileDir)
	vault, err := os.ReadFile(filepath.Join(profileDir, ProfileVaultFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(vault, []byte(marker)) || bytes.Contains(vault, []byte("dwb_probe")) {
		t.Fatal("the cookie is readable in the sealed file")
	}

	// 重新打开：解封 → 同一个加密身份（mock）→ Cookie 还在。
	core2, err := NewBrowserCore(ctx, profileID, WithMode(ModeHeadless), WithPersistentProfile())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := core2.Navigate(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := core2.EvalJS(ctx, `document.cookie`, &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "dwb_probe="+marker) {
		t.Fatalf("the cookie did not survive close → seal → unseal → reopen: %q", got)
	}
	if err := core2.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireOnlyManifestAndVault(t, profileDir) // 再次封存
}

func TestOwnedCoreTemporaryProfileCloseKillsAndDoesNotSeal(t *testing.T) {
	requireChromeForPool(t)
	useTestKeyFile(t)
	profileID := fmt.Sprintf("temp-%d", time.Now().UnixNano())
	profileDir := filepath.Join(browserCLIDefaultBase(), profileID)
	t.Cleanup(func() { _ = RemoveCLIProfileDir(profileDir) })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	core, err := NewBrowserCore(ctx, profileID, WithMode(ModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := core.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	rc := core.(*browserCoreImpl).LastCloseReceipt()
	if rc == nil || rc.Shutdown.Mode != ShutdownKilled || rc.Shutdown.Reason != "ephemeral-profile" || rc.Profile != nil {
		t.Fatalf("a temporary profile is killed at once and never sealed: %+v", rc)
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("closing a temporary profile must stay fast, took %s", d)
	}
	m, err := ReadProfileManifest(profileDir)
	if err != nil || m.OSCrypt != ProfileOSCryptMock || m.Vault != ProfileVaultNone {
		t.Fatalf("manifest %+v %v", m, err)
	}
	if contains(dirNames(t, profileDir), ProfileVaultFile) {
		t.Fatal("a temporary profile must not be vaulted")
	}
}
