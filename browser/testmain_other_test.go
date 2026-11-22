//go:build !darwin && !linux

package browser

import (
	"os"
	"testing"
)

// darwin 与 linux 各有自己的 TestMain（虚拟显示 / Chrome 泄漏闸）；其余平台只需要 hermetic profile 环境。
func TestMain(m *testing.M) {
	cleanup := useHermeticProfileEnv()
	code := m.Run()
	cleanup()
	os.Exit(code)
}
