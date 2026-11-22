// Package browser — 整个 CLI profile 目录的删除（D8）。
//
// 临时 profile 随会话删除、失败的 open 清理现场、profile prune，过去都是对一个由调用方给出的路径
// os.RemoveAll。现在统一走 P2 模块导出的 RemoveProfileTree（清单 → 校验 → 逐项删除）：
// 以 CLI profile 根为边界，路径都在根内、祖先不是 symlink、条目自规划后没被改动、没有计划外条目，
// 任一项不过就整体中止、一个都不删，返回 ErrVaultDeleteRefused。
//
// 本文件只做两件 P2 模块不管的事：哪些目录算"CLI profile 根"（BrowserCLIProfileRoots），以及这一层的名字约定
// （点开头的名字不是 profile 目录）。它不含任何直接删除调用，也不依赖 P2 模块的内部符号。
package browser

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func profileNameRefusal(profileDir, name string) error {
	return &VaultDeleteRefusedError{Path: profileDir, Reason: "not a plain profile directory name under a CLI profile root: " + name}
}

// validProfileDirName：空、点开头（含 . 和 ..）、含路径分隔符或 NUL 的名字一律不是 profile 目录名。
func validProfileDirName(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") {
		return false
	}
	return !strings.ContainsAny(name, `/\`) && !strings.ContainsRune(name, 0)
}

// removeProfileDirUnderRoot 删除 root 下名为 name 的整棵目录。root 之外的任何东西都不会被碰；
// 目录不存在（或 root 本身不存在）视为成功。
func removeProfileDirUnderRoot(root, name string) error {
	if !validProfileDirName(name) {
		return profileNameRefusal(filepath.Join(root, name), name)
	}
	if err := RemoveProfileTree(root, name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// RemoveCLIProfileDir 删除一个 CLI profile 目录。profileDir 必须恰好是某个 CLI profile 根的直接子级
// （见 BrowserCLIProfileRoots），否则拒绝并返回 ErrVaultDeleteRefused，什么都不删。
func RemoveCLIProfileDir(profileDir string) error {
	abs, err := filepath.Abs(profileDir)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	name := filepath.Base(abs)
	parent := filepath.Dir(abs)
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, root := range BrowserCLIProfileRoots("") {
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		if realRoot == realParent {
			return removeProfileDirUnderRoot(root, name)
		}
	}
	return &VaultDeleteRefusedError{Path: abs, Reason: "not a direct child of a CLI profile root"}
}
