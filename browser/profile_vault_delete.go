// Package browser — profile 封存/解封涉及的全部删除，统一收口在这个文件。
//
// 硬约束（Human 指令）：不得直接删除范围不清的路径。因此：
//   - 全包没有任何递归删除；profile 根及其父目录永远不会成为删除对象。
//   - 每次删除都是"先生成精确清单 → 逐项校验 → 逐项删除"：
//     清单里的每一项都是 Lstat 过的具体路径（目录树被展开成逐个文件/目录/symlink）；
//   - 校验在任何删除之前对整份清单完成，任何一项不过就整体中止、一个都不删，返回 ErrVaultDeleteRefused；
//   - symlink 只删链接本身，绝不跟随；祖先目录若是 symlink（会让路径指向别处）一律拒绝；
//   - 删除用非递归的 os.Remove：文件/链接直接 unlink，目录必须已被清空（有计划外条目就拒绝而不是递归带走）。
//
// 本包其他文件里不允许出现 os.Remove / os.RemoveAll（TestDeletionsAreFunnelledThroughOneFile 守着）。
package browser

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// ErrVaultDeleteRefused 删除清单校验未通过：路径越界、祖先是 symlink、条目自规划后被改动、
// 出现计划外条目、缓存项不在允许列表，或与封存清单不一致。被拒时没有任何条目被删除（校验阶段）。
var ErrVaultDeleteRefused = errors.New("browser: profile vault deletion refused")

// VaultDeleteRefusedError 是 ErrVaultDeleteRefused 的带细节版本。
type VaultDeleteRefusedError struct {
	Path   string
	Reason string
}

func (e *VaultDeleteRefusedError) Error() string {
	return fmt.Sprintf("browser: profile vault deletion refused: %s (%s)", e.Path, e.Reason)
}

func (e *VaultDeleteRefusedError) Unwrap() error { return ErrVaultDeleteRefused }

func refuseDelete(p, reason string) error {
	return &VaultDeleteRefusedError{Path: p, Reason: reason}
}

type vaultItemKind int

const (
	vaultItemArchive  vaultItemKind = iota + 1 // 已封存进 vault 的明文：封存后删除
	vaultItemCache                             // 可再生缓存（必须命中允许列表）：不封存，直接删除
	vaultItemRuntime                           // 运行期产物 / 本模块自己的临时件：不封存，删除
	vaultItemSpecial                           // socket / fifo / 设备：没有数据，删除
	vaultItemLeftover                          // 解封时，已认证 vault 旁边的残留明文
	vaultItemTemp                              // 解封的临时目录树（本模块创建）
	vaultItemTree                              // 调用方（RemoveProfileTree）指定整体删除的那一个子目录树
)

// vaultItem 是删除清单里的一项：Lstat 过的具体路径。
type vaultItem struct {
	rel       string      // 相对 root 的正斜杠路径
	info      fs.FileInfo // 规划时的 Lstat 结果
	kind      vaultItemKind
	cacheRoot string // kind==vaultItemCache：命中允许列表的缓存目录（相对路径）
}

// vaultDeletePlan 是一次删除的完整清单。
type vaultDeletePlan struct {
	root       string          // 已解析 symlink 的 profile 根
	keep       map[string]bool // 根下永不删除的顶层名字（清单、封存文件、本次的临时件）
	items      []vaultItem
	coversRoot bool // 清单是否覆盖 root 下除 keep 外的全部顶层条目（是 → 校验"根下没有计划外条目"）
	archived   map[string]int64
	treeName   string // vaultItemTree：调用方指定的那个单段目录名；清单里每一项都必须在它之下
}

func newVaultKeep(names ...string) map[string]bool {
	keep := make(map[string]bool, len(names))
	for _, n := range names {
		keep[n] = true
	}
	return keep
}

// absOf 静态校验 rel 并换算成 root 内的绝对路径：不空、不是 "."、是干净的相对路径、不含 ..、
// Rel(root, abs) 不以 .. 开头、顶层名不在 keep 里。
func (p *vaultDeletePlan) absOf(rel string) (string, error) {
	if rel == "" || rel == "." || path.Clean(rel) != rel || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", refuseDelete(rel, "outside profile root")
	}
	abs := filepath.Join(p.root, filepath.FromSlash(rel))
	back, err := filepath.Rel(p.root, abs)
	if err != nil || back == "." || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", refuseDelete(abs, "outside profile root")
	}
	top, _, _ := strings.Cut(rel, "/")
	if p.keep[top] {
		return "", refuseDelete(abs, "protected entry")
	}
	return abs, nil
}

// ancestorsClean：rel 的父目录路径上不能有 symlink（否则 Lstat/Remove 会穿过链接落到别处）。
// 父目录已不存在 → 条目也必然不在了，视为通过。
func (p *vaultDeletePlan) ancestorsClean(rel string, cache map[string]bool) error {
	parent := path.Dir(rel)
	if parent == "." {
		return nil
	}
	want := filepath.Join(p.root, filepath.FromSlash(parent))
	if ok, seen := cache[parent]; seen {
		if ok {
			return nil
		}
		return refuseDelete(want, "ancestor is or resolves through a symlink")
	}
	got, err := filepath.EvalSymlinks(want)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	ok := got == want
	cache[parent] = ok
	if !ok {
		return refuseDelete(want, "ancestor is or resolves through a symlink")
	}
	return nil
}

// unchangedSincePlanned：同一个文件系统对象、同一类型；常规文件还要大小和修改时间都没变
// （变了说明规划之后有人写过，删掉就会丢那部分新数据）。
func unchangedSincePlanned(it vaultItem, cur fs.FileInfo) error {
	if it.info.Mode().Type() != cur.Mode().Type() {
		return fmt.Errorf("type changed from %s to %s", it.info.Mode().Type(), cur.Mode().Type())
	}
	if !os.SameFile(it.info, cur) {
		return errors.New("replaced by a different filesystem object")
	}
	if cur.Mode().IsRegular() && (cur.Size() != it.info.Size() || !cur.ModTime().Equal(it.info.ModTime())) {
		return errors.New("modified after planning")
	}
	return nil
}

func (p *vaultDeletePlan) checkKind(it vaultItem, abs string, archived map[string]int64) error {
	top, _, _ := strings.Cut(it.rel, "/")
	switch it.kind {
	case vaultItemArchive:
		if archived == nil {
			return refuseDelete(abs, "no verified vault listing to check against")
		}
		size, ok := archived[it.rel]
		if !ok {
			return refuseDelete(abs, "not in the verified vault")
		}
		switch {
		case it.info.IsDir() && size != -1:
			return refuseDelete(abs, "vault records a different type")
		case !it.info.IsDir() && size != it.info.Size():
			return refuseDelete(abs, "size differs from what the vault holds")
		}
	case vaultItemCache:
		if it.cacheRoot == "" || !isRegenerableCachePath(it.cacheRoot) ||
			(it.rel != it.cacheRoot && !strings.HasPrefix(it.rel, it.cacheRoot+"/")) {
			return refuseDelete(abs, "cache entry not on the allow list")
		}
	case vaultItemRuntime:
		if !vaultTopLevelExcluded(top) {
			return refuseDelete(abs, "not a known runtime artifact")
		}
	case vaultItemSpecial:
		if it.info.Mode()&(fs.ModeSocket|fs.ModeNamedPipe|fs.ModeDevice|fs.ModeCharDevice|fs.ModeIrregular) == 0 {
			return refuseDelete(abs, "not a special file")
		}
	case vaultItemTemp:
		if !strings.HasPrefix(top, vaultUnsealTmpPrefix) {
			return refuseDelete(abs, "not an unseal temp entry")
		}
	case vaultItemTree:
		if p.treeName == "" || top != p.treeName {
			return refuseDelete(abs, "not under the designated directory")
		}
	case vaultItemLeftover:
		// 除了"在 root 内、不是受保护名字"之外没有别的类别限制：残留明文本来就没有名单。
	default:
		return refuseDelete(abs, "unknown entry kind")
	}
	return nil
}

// validate 对整份清单做全部校验，不删除任何东西。archived 是已验证封存里的条目名 → 大小（目录为 -1），
// 非 nil 时：封存类条目必须与它逐项一致，且两边数量相等（清单 == 封存内容）。
func (p *vaultDeletePlan) validate(archived map[string]int64) error {
	p.archived = archived
	planned := make(map[string]struct{}, len(p.items))
	archiveCount := 0
	for _, it := range p.items {
		if _, dup := planned[it.rel]; dup {
			return refuseDelete(it.rel, "duplicate entry in deletion list")
		}
		planned[it.rel] = struct{}{}
		if it.kind == vaultItemArchive {
			archiveCount++
		}
	}
	if archived != nil && archiveCount != len(archived) {
		return refuseDelete(p.root, fmt.Sprintf("deletion list has %d archived entries but the vault holds %d", archiveCount, len(archived)))
	}

	clean := map[string]bool{}
	for _, it := range p.items {
		abs, err := p.absOf(it.rel)
		if err != nil {
			return err
		}
		if err := p.checkKind(it, abs, archived); err != nil {
			return err
		}
		if err := p.ancestorsClean(it.rel, clean); err != nil {
			return err
		}
		cur, err := os.Lstat(abs)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // 规划后自己消失了：没什么可删，不构成风险
			}
			return err
		}
		if err := unchangedSincePlanned(it, cur); err != nil {
			return refuseDelete(abs, "changed since planned: "+err.Error())
		}
	}
	return p.checkNoUnplanned(planned)
}

// checkNoUnplanned：清单里每个目录的当前内容都必须全部在清单里（根目录同理，keep 除外）。
// 防的是规划之后冒出来的新文件（比如 Chrome 又启动了）被我们当成"自己的东西"一起带走。
func (p *vaultDeletePlan) checkNoUnplanned(planned map[string]struct{}) error {
	if p.coversRoot {
		entries, err := os.ReadDir(p.root)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if p.keep[e.Name()] {
				continue
			}
			if _, ok := planned[e.Name()]; !ok {
				return refuseDelete(filepath.Join(p.root, e.Name()), "unplanned entry appeared")
			}
		}
	}
	for _, it := range p.items {
		if !it.info.IsDir() {
			continue
		}
		abs := filepath.Join(p.root, filepath.FromSlash(it.rel))
		cur, err := os.Lstat(abs)
		if err != nil || !cur.IsDir() {
			continue // 消失或已不是目录：前面的逐项校验会处理类型变化
		}
		children, err := os.ReadDir(abs)
		if err != nil {
			return err
		}
		for _, c := range children {
			if _, ok := planned[it.rel+"/"+c.Name()]; !ok {
				return refuseDelete(filepath.Join(abs, c.Name()), "unplanned entry appeared")
			}
		}
	}
	return nil
}

// execute 先把整份清单重新校验一遍（关掉"校验 → 删除"之间的空窗），通过后逐项删除：
// 非目录条目在前，目录由深到浅在后；每项删除前再 Lstat 对一次。
func (p *vaultDeletePlan) execute() error {
	if err := p.validate(p.archived); err != nil {
		return err
	}
	order := append([]vaultItem(nil), p.items...)
	sort.SliceStable(order, func(i, j int) bool {
		di, dj := order[i].info.IsDir(), order[j].info.IsDir()
		if di != dj {
			return !di
		}
		if di {
			return strings.Count(order[i].rel, "/") > strings.Count(order[j].rel, "/")
		}
		return false
	})
	clean := map[string]bool{}
	for _, it := range order {
		abs, err := p.absOf(it.rel)
		if err != nil {
			return err
		}
		if err := p.ancestorsClean(it.rel, clean); err != nil {
			return err
		}
		cur, err := os.Lstat(abs)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if err := unchangedSincePlanned(it, cur); err != nil {
			return refuseDelete(abs, "changed since planned: "+err.Error())
		}
		// os.Remove 对 symlink 是 unlink 链接本身；对目录是 rmdir（非空会失败）。绝不递归、绝不跟随。
		if err := os.Remove(abs); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if cur.IsDir() && (errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)) {
				return refuseDelete(abs, "directory not empty: unplanned entries appeared")
			}
			return fmt.Errorf("browser: remove %s: %w", it.rel, err)
		}
	}
	return nil
}

// planSubtrees 把 root 下满足 include(顶层名) 的每个顶层条目展开成逐项清单。
// WalkDir 不跟随 symlink：指向目录的 symlink 只作为一个条目（链接本身）出现。
func planSubtrees(root string, include func(top string) bool, kind vaultItemKind) ([]vaultItem, error) {
	var items []vaultItem
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		top, _, _ := strings.Cut(relSlash, "/")
		if !include(top) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		items = append(items, vaultItem{rel: relSlash, info: info, kind: kind})
		return nil
	})
	return items, err
}

// removeVaultTree：展开 → 校验 → 逐项删除 root 下满足 include 的顶层条目。
// coversRoot 为 true 表示清单打算清掉 root 下除 keep 之外的一切（此时根下出现计划外条目也要拒绝）。
func removeVaultTree(root string, keep map[string]bool, include func(top string) bool, kind vaultItemKind, coversRoot bool) error {
	items, err := planSubtrees(root, include, kind)
	if err != nil {
		return err
	}
	plan := &vaultDeletePlan{root: root, keep: keep, items: items, coversRoot: coversRoot}
	return plan.execute()
}

// removeFileInRoot 删除恰好一个非目录条目（常规文件，或 symlink 本身），要求它位于 root 内。
// 用于本模块自己创建/自己命名的单个文件（vault、临时件）。目录一律拒绝；不存在视为成功。
func removeFileInRoot(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return refuseDelete(target, "outside profile root")
	}
	plan := &vaultDeletePlan{root: root, keep: nil}
	if err := plan.ancestorsClean(filepath.ToSlash(rel), map[string]bool{}); err != nil {
		return err
	}
	fi, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if fi.IsDir() {
		return refuseDelete(target, "is a directory")
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// validProfileTreeName：name 必须是单段、合法的目录名——不空、不是 "." / ".."、不含路径分隔符（正反斜杠）、
// 不含 NUL、不超过 NAME_MAX。
func validProfileTreeName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > vaultMaxNameLen {
		return false
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return false
	}
	return filepath.Base(name) == name && filepath.IsLocal(name)
}

// RemoveProfileTree 整体删除 root 下名为 name 的那一个子目录（例如 CLI close 删除临时 profile）。
// 走与封存/解封相同的删除机制：先把整棵树展开成逐项清单，对整份清单校验通过之后才逐项删除。
//
//   - name 必须是单段合法名（见 validProfileTreeName），否则 ErrVaultDeleteRefused。
//   - root 必须是已存在的目录，不能是文件系统根；root 自身若经过 symlink，按解析后的真实路径作边界。
//     root 不存在 → 返回 os.ErrNotExist（调用方的错误，不当成成功）。
//   - root/name 不存在 → nil（幂等）；是 symlink 或不是目录 → ErrVaultDeleteRefused（不跟随、也不替调用方
//     猜"删链接是不是他们想要的"）。
//   - 树内的 symlink 只删链接本身，绝不跟随；任何一项校验不过（越界、祖先是 symlink、规划后被改动、
//     出现计划外条目）就整体中止、一个都不删，返回 ErrVaultDeleteRefused。
//
// 调用方必须保证 Chrome 已退出：树里还有进程在写，就会因"计划外条目"被拒绝。
func RemoveProfileTree(root, name string) error {
	if !validProfileTreeName(name) {
		return refuseDelete(name, "not a single, valid path segment")
	}
	if strings.TrimSpace(root) == "" {
		return refuseDelete(root, "empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	realRoot, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return err
	}
	rootInfo, err := os.Stat(realRoot)
	if err != nil {
		return err
	}
	if !rootInfo.IsDir() {
		return refuseDelete(realRoot, "root is not a directory")
	}
	if filepath.Dir(realRoot) == realRoot {
		return refuseDelete(realRoot, "root is the filesystem root")
	}

	target := filepath.Join(realRoot, name)
	fi, err := os.Lstat(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return refuseDelete(target, "not a real directory")
	}
	items, err := planSubtrees(realRoot, func(top string) bool { return top == name }, vaultItemTree)
	if err != nil {
		return err
	}
	plan := &vaultDeletePlan{root: realRoot, keep: newVaultKeep(), items: items, treeName: name}
	return plan.execute()
}
