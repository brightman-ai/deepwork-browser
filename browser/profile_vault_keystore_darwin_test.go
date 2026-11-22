//go:build darwin

package browser

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"testing"
)

// 名字校验是纯逻辑：服务名/账户名会被拼进 `security -i` 的 stdin 命令，必须拒绝注入。
func TestKeychainKeyStoreRejectsUnsafeNames(t *testing.T) {
	for _, bad := range []struct{ service, account string }{
		{"has space", "ok"},
		{"ok", "line\nbreak"},
		{"ok", `quo"te`},
		{"semi;colon", "ok"},
		{"", "ok"},
		{"ok", ""},
	} {
		ks := newKeychainKeyStore(bad.service, bad.account)
		if _, err := ks.Load(); err == nil {
			t.Fatalf("Load accepted %+v", bad)
		}
		if err := ks.Create(bytes.Repeat([]byte{1}, 32)); err == nil {
			t.Fatalf("Create accepted %+v", bad)
		}
	}
}

// 真实登录钥匙串集成测试：只在 DW_BROWSER_KEYCHAIN_IT=1 时运行，使用独立 service 名，结束时删除条目。
func TestKeychainKeyStoreIntegration(t *testing.T) {
	if os.Getenv("DW_BROWSER_KEYCHAIN_IT") != "1" {
		t.Skip("set DW_BROWSER_KEYCHAIN_IT=1 to run against the real login keychain")
	}
	service := "dw-browser.profile-vault.it-" + hex.EncodeToString(randBytes(t, 6))
	ks := newKeychainKeyStore(service, "master-key-it")
	t.Cleanup(func() {
		err := ks.delete()
		if err != nil && !errors.Is(err, ErrVaultKeyNotFound) {
			t.Errorf("cleanup: could not delete keychain entry %s: %v", service, err)
		}
		if _, err := ks.Load(); !errors.Is(err, ErrVaultKeyNotFound) {
			t.Errorf("cleanup: entry still present after delete: %v", err)
		}
	})

	if ks.Protection() != KeyProtectionKeychain {
		t.Fatalf("protection %q", ks.Protection())
	}
	if _, err := ks.Load(); !errors.Is(err, ErrVaultKeyNotFound) {
		t.Fatalf("fresh service must be absent (exit 44): %v", err)
	}

	key, err := LoadOrCreateMasterKey(ks)
	if err != nil {
		t.Fatalf("LoadOrCreateMasterKey: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("key length %d", len(key))
	}
	again, err := ks.Load()
	if err != nil || !bytes.Equal(again, key) {
		t.Fatalf("read-back mismatch: %v", err)
	}
	// 不覆盖：重复创建报 ErrVaultKeyExists，原值不变。
	if err := ks.Create(bytes.Repeat([]byte{0xAB}, 32)); !errors.Is(err, ErrVaultKeyExists) {
		t.Fatalf("duplicate create: want ErrVaultKeyExists, got %v", err)
	}
	if again, _ := ks.Load(); !bytes.Equal(again, key) {
		t.Fatal("duplicate create overwrote the entry")
	}
	// 另一个实例（模拟另一个进程）读到同一把。
	if other, err := newKeychainKeyStore(service, "master-key-it").Load(); err != nil || !bytes.Equal(other, key) {
		t.Fatalf("second instance: %v", err)
	}

	// 端到端：用钥匙串密钥封存/解封一个真实 profile。
	dir, _ := newTestProfile(t)
	want := snapshotTree(t, dir)
	rc := mustSeal(t, dir, ks)
	if rc.Protection != KeyProtectionKeychain || !rc.Verified {
		t.Fatalf("seal receipt: %+v", rc)
	}
	requireOnlyManifestAndVault(t, dir)
	mustUnseal(t, dir, ks)
	diffTrees(t, want, snapshotTree(t, dir))
}
