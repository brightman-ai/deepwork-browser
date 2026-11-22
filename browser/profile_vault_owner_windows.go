//go:build windows

package browser

import "io/fs"

// vaultFileOwner Windows 没有 uid 概念：不校验属主。
var vaultFileOwner = func(fi fs.FileInfo) (uid int, ok bool) { return 0, false }
