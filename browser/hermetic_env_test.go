package browser

import (
	"fmt"
	"os"
	"path/filepath"
)

// useHermeticProfileEnv 让整个 browser 包的测试不碰真实的 ~/.deepwork/browser-cli，也不碰生产钥匙串：
//
//   - EnvBrowserCLIProfileRoot → 本进程专用的临时目录下的 browser-cli。NewBrowserCore("<name>-<ns>") 一类夹具、
//     document 运行时、RemoveCLIProfileDir 的边界都跟着它走；
//   - EnvProfileKeyFile → 同一个临时目录里的密钥文件。封存/解封在测试里永远用文件密钥库，
//     不会在登录钥匙串里创建 dw-browser.profile-vault 条目。
//
// 目录只是"约定的路径"，在真正有人往里写之前并不创建（NewBrowserCore、密钥库都会自己 MkdirAll）：
// 这个包有很多"重新执行测试二进制充当帮手进程"的测试（假 Chrome、锁持有者……），帮手进程会经过同一个 TestMain，
// 它们被信号杀掉或 os.Exit 时不会走清理——如果在这里急着 MkdirTemp，每个帮手都会漏一个目录。
// 目录名带本进程 PID，清理函数（只在主测试进程里跑到）把整个目录先展开成逐项清单、校验、再逐项删除
// （RemoveProfileTree 的 funnel），不对目录根做递归删除。
func useHermeticProfileEnv() (cleanup func()) {
	name := fmt.Sprintf("dwb-test-profiles-%d", os.Getpid())
	dir := filepath.Join(os.TempDir(), name)
	os.Setenv(EnvBrowserCLIProfileRoot, filepath.Join(dir, "browser-cli"))
	os.Setenv(EnvProfileKeyFile, filepath.Join(dir, "profile-vault.key"))
	return func() {
		os.Unsetenv(EnvBrowserCLIProfileRoot)
		os.Unsetenv(EnvProfileKeyFile)
		if err := RemoveProfileTree(os.TempDir(), name); err != nil {
			fmt.Fprintf(os.Stderr, "hermetic profile env: could not remove %s: %v\n", dir, err)
		}
	}
}
