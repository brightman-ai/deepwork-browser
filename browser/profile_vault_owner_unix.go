//go:build !windows

package browser

import (
	"io/fs"
	"syscall"
)

// vaultFileOwner 返回文件属主 uid；平台不暴露时 ok=false。做成变量是为了让测试能伪造"属主不是我"。
var vaultFileOwner = func(fi fs.FileInfo) (uid int, ok bool) {
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, false
	}
	return int(st.Uid), true
}
