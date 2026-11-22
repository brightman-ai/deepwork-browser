// Package browser — 封存条目的"接受规则"，封存、自校验 dry-run、解封三处共用同一份实现。
//
// 不变量：封存接受的条目集合 ⊆ 解封接受的条目集合。否则封存成功、明文已删，却永远解不开。
// 做法不是"各写一套再祈祷一致"，而是规则只有一份：
//   - SealProfile 规划阶段对每个归档项调用 vaultArchivableName（同一套名字规则）；
//   - 封存后的自校验对整份 vault 做 dry-run（同一个 vaultEntryChecker，等价于解封但不落盘）；
//   - UnsealProfile 提取时用的也是这个 vaultEntryChecker。
package browser

import (
	"archive/tar"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	vaultMaxNameLen = 255 // NAME_MAX：单个路径元素的字节数上限

	// 解封先解到 <dir>/.dw-unseal-XXXXXXXXXX/ 下面再移入；os.MkdirTemp 的随机后缀是 uint32 的十进制，最多 10 位。
	vaultUnsealTmpNameLen = len(vaultUnsealTmpPrefix) + 10
)

// 解封接受的 PAX 记录白名单：只有我们自己的写入端会产生的几种。其余一律拒绝，
// 尤其是 GNU.sparse.*（声明巨大的稀疏文件会把磁盘写满）和 SCHILY.xattr.*（扩展属性）。
var vaultPAXAllowed = map[string]bool{
	"path": true, "mtime": true, "atime": true, "ctime": true,
	"uid": true, "gid": true, "uname": true, "gname": true, "size": true,
}

// vaultNameRules 判断解封是否会接受条目名 name，返回去掉尾斜杠后的正斜杠路径。
// fits 非 nil 时再检查"放进解封临时目录后路径是否超过 PATH_MAX"。
func vaultNameRules(name string, fits func(rel string) bool) (string, error) {
	rel, err := safeVaultRel(name)
	if err != nil {
		return "", err
	}
	bad := func(why string) (string, error) {
		return "", fmt.Errorf("%w: %s (%q)", ErrVaultUnsafeEntry, why, name)
	}
	if !utf8.ValidString(rel) {
		return bad("name is not valid UTF-8")
	}
	for _, el := range strings.Split(rel, "/") {
		if len(el) > vaultMaxNameLen {
			return bad("path element longer than 255 bytes")
		}
	}
	if fits != nil && !fits(rel) {
		return bad("path too long to unseal")
	}
	return rel, nil
}

// vaultFitsUnderProfile 返回判断函数：<dir>/<解封临时目录最长名>/<rel> 是否小于平台路径上限。
func vaultFitsUnderProfile(dir string) func(rel string) bool {
	return func(rel string) bool {
		return len(dir)+1+vaultUnsealTmpNameLen+1+len(rel) < vaultPathLimit()
	}
}

// vaultArchivableName 是封存侧的入口：解封会拒绝的名字，封存时就以 ErrVaultUnsupportedEntry 中止。
func vaultArchivableName(root, rel string) error {
	if _, err := vaultNameRules(rel, vaultFitsUnderProfile(root)); err != nil {
		return fmt.Errorf("%w: %s (%s)", ErrVaultUnsupportedEntry, rel, err.Error())
	}
	return nil
}

// vaultEntryChecker 对解封要接受的每个 tar 条目应用同一套规则，并按头部声明的总量计数。
// 提取（落盘）和封存自校验（dry-run）都用它。
type vaultEntryChecker struct {
	limits vaultTotals // 头部声明（受 AAD 认证）
	got    vaultTotals
	fits   func(rel string) bool
	seen   map[string]bool // 条目名（含隐含的父目录）→ 是否目录
}

func newVaultEntryChecker(limits vaultTotals, fits func(rel string) bool) *vaultEntryChecker {
	return &vaultEntryChecker{limits: limits, fits: fits, seen: map[string]bool{}}
}

// check 校验一个条目并计数；必须在读取条目内容之前调用，超出头部声明时才能在写盘之前中止。
func (c *vaultEntryChecker) check(h *tar.Header) (string, error) {
	unsafe := func(format string, a ...any) (string, error) {
		return "", fmt.Errorf("%w: %s (%q)", ErrVaultUnsafeEntry, fmt.Sprintf(format, a...), h.Name)
	}
	if h.Format != tar.FormatUSTAR && h.Format != tar.FormatPAX {
		return unsafe("non-standard tar format %v", h.Format)
	}
	for k := range h.PAXRecords {
		if !vaultPAXAllowed[k] {
			return unsafe("unsupported PAX record %q", k)
		}
	}
	isDir := h.Typeflag == tar.TypeDir
	if !isDir && h.Typeflag != tar.TypeReg {
		// symlink、hardlink、设备、fifo、GNU 稀疏文件……一律拒绝。
		return unsafe("entry type %q", string(h.Typeflag))
	}
	rel, err := vaultNameRules(h.Name, c.fits)
	if err != nil {
		return "", err
	}
	if h.Size < 0 {
		return "", fmt.Errorf("%w: negative size (%q)", ErrVaultMalformed, h.Name)
	}

	// 同名 / 文件与目录冲突：落盘会失败，dry-run 里也要同样失败。
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		anc := strings.Join(parts[:i], "/")
		if isDirAnc, ok := c.seen[anc]; ok && !isDirAnc {
			return "", fmt.Errorf("%w: %q lies under the file %q", ErrVaultMalformed, rel, anc)
		} else if !ok {
			c.seen[anc] = true
		}
	}
	if prevIsDir, ok := c.seen[rel]; ok && !(isDir && prevIsDir) {
		return "", fmt.Errorf("%w: duplicate or conflicting entry %q", ErrVaultMalformed, rel)
	}
	c.seen[rel] = isDir

	// 头部声明的总量：超出就中止。
	c.got.Entries++
	if c.got.Entries > c.limits.Entries {
		return "", fmt.Errorf("%w: more entries than the header declares (%d)", ErrVaultMalformed, c.limits.Entries)
	}
	if !isDir {
		c.got.Files++
		if c.got.Files > c.limits.Files {
			return "", fmt.Errorf("%w: more files than the header declares (%d)", ErrVaultMalformed, c.limits.Files)
		}
		if h.Size > c.limits.Bytes-c.got.Bytes {
			return "", fmt.Errorf("%w: more bytes than the header declares (%d)", ErrVaultMalformed, c.limits.Bytes)
		}
		c.got.Bytes += h.Size
	}
	return rel, nil
}

// finish：流读完之后，实际内容必须恰好等于头部声明。
func (c *vaultEntryChecker) finish() error {
	if c.got != c.limits {
		return fmt.Errorf("%w: content (%d entries, %d files, %d bytes) does not match the header (%d, %d, %d)",
			ErrVaultMalformed, c.got.Entries, c.got.Files, c.got.Bytes, c.limits.Entries, c.limits.Files, c.limits.Bytes)
	}
	return nil
}

// ============================================================
// § 可再生缓存的允许列表：只在已知位置匹配
// ============================================================

// 可再生缓存目录名：封存时删除而不是封存——HTTP 缓存里可能有敏感响应。
var vaultCacheDirPatterns = []string{"Cache", "Code Cache", "GPUCache", "*ShaderCache", "Dawn*Cache", "*_crx_cache"}

var vaultProfileDirRE = regexp.MustCompile(`^Profile [0-9]+$`)

func matchesCacheName(name string) bool {
	for _, pat := range vaultCacheDirPatterns {
		if ok, _ := path.Match(pat, name); ok {
			return true
		}
	}
	return false
}

func isKnownProfileDir(name string) bool {
	return name == "Default" || name == "Guest Profile" || name == "System Profile" || vaultProfileDirRE.MatchString(name)
}

// isRegenerableCachePath 判断相对路径 rel（正斜杠）是否是"已知位置上的可再生缓存目录"：
// profile 根目录第一层，或 Default/、Profile N/、Guest Profile/、System Profile/ 下的第一层。
// 不在任意深度按名字匹配：扩展、Service Worker 自己的数据目录恰好叫 Cache 时，那是用户数据。
func isRegenerableCachePath(rel string) bool {
	parts := strings.Split(rel, "/")
	switch len(parts) {
	case 1:
		return matchesCacheName(parts[0])
	case 2:
		return isKnownProfileDir(parts[0]) && matchesCacheName(parts[1])
	}
	return false
}
