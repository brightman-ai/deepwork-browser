package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/brightman-ai/deepwork-browser/browser"
)

// TestMain 让本包的测试不碰真实的 ~/.deepwork/browser-cli，也不碰生产钥匙串：
// CLI profile 根与封存密钥都落在本次运行专用的临时目录里（见 browser.EnvBrowserCLIProfileRoot /
// browser.EnvProfileKeyFile）。验收标准：跑完 `go test ./...`，真实 profile 根的内容与跑之前完全一致。
//
// 清理只做"精确删除"：profile 目录走 browser.RemoveCLIProfileDir（清单 → 校验 → 逐项删除），
// 其余是对具体路径的非递归 os.Remove；删不掉就报告，不用递归删除兜底。
func TestMain(m *testing.M) {
	// 路径按本进程 PID 约定，在有人往里写之前不创建：本包有"重新执行测试二进制充当帮手进程"的测试，
	// 帮手进程用 os.Exit 退出时不会走下面的清理，急着 MkdirTemp 会让每个帮手漏一个目录。
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("dwb-cmd-test-profiles-%d", os.Getpid()))
	root := filepath.Join(dir, "browser-cli")
	keyFile := filepath.Join(dir, "profile-vault.key")
	os.Setenv(browser.EnvBrowserCLIProfileRoot, root)
	os.Setenv(browser.EnvProfileKeyFile, keyFile)

	code := m.Run()

	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			if err := browser.RemoveCLIProfileDir(filepath.Join(root, e.Name())); err != nil {
				fmt.Fprintf(os.Stderr, "hermetic profile env: leftover %s: %v\n", e.Name(), err)
			}
		}
	}
	for _, p := range []string{keyFile, root, dir} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) { // 具体路径、非递归
			fmt.Fprintf(os.Stderr, "hermetic profile env: could not remove %s: %v\n", p, err)
		}
	}
	os.Exit(code)
}
