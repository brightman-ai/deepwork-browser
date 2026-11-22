// Package browser — profile 加密封存（profile.vault）。
//
// 防护目标：profile 目录在会话之外被复制、备份、同步或打包时不泄露。
// 不防护：会话运行期间同用户进程（profile 此时是可解明文，密钥也可经 /usr/bin/security 取到）。
//
// 封存格式 profile.vault：
//
//	"DWPV1\n" ‖ uint32BE(len(header)) ‖ header(JSON) ‖ chunk*
//	header = {v, alg, kdf, salt, nonce_prefix, profile_id, chunk_size, created_at}
//	chunk  = AES-256-GCM(明文 ≤ chunk_size)；末块之外都恰好 chunk_size 字节
//	nonce  = nonce_prefix(8) ‖ uint32BE(块序号)
//	AAD    = SHA256(magic ‖ len ‖ header) ‖ uint64BE(块序号) ‖ 末块标志(1 字节)
//
// 明文是 profile 目录内容的 tar 流。末块标志让"截断末块 / 在块边界截断 / 追加"都认证失败；
// 块序号同时进 nonce 和 AAD，让重排、重复都认证失败。
//
// 数据密钥 = HKDF-SHA256(机器主密钥, header.salt, "dw-browser/profile-vault/v1|"+profile_id)。
//
// 状态机（清单 vault 字段）：
//
//	封存：写 tmp → fsync → 自校验 → rename 成 profile.vault → 清单置 sealed（提交点）→ 删除明文
//	解封：解到 .dw-unseal-* → 校验末块 → 清掉残留 → 移入 → 清单置 open → 删除 profile.vault
//	清单 open + vault 并存（崩溃遗留）：以明文为准，vault 作废
//	清单 sealed + 残留明文（封存后删除一半崩溃）：以 vault 为准，解封时清掉残留
package browser

import (
	"archive/tar"
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	vaultMagic         = "DWPV1\n"
	vaultChunkSize     = 1 << 20
	vaultMaxChunkSize  = 16 << 20 // 读取侧上限：header 里的 chunk_size 在认证之前就要分配缓冲
	vaultMinChunkSize  = 1 << 10
	vaultMaxHeaderLen  = 64 << 10
	vaultAlgName       = "AES-256-GCM"
	vaultKDFName       = "HKDF-SHA256"
	vaultKDFInfoPrefix = "dw-browser/profile-vault/v1|"
	vaultSaltLen       = 32
	vaultNoncePrefix   = 8

	vaultUnsealTmpPrefix = ".dw-unseal-"
)

var (
	// ErrVaultAuthFailed 认证失败：错误密钥、篡改、截断、重排都落在这里（AEAD 无法区分它们）。
	ErrVaultAuthFailed = errors.New("browser: profile vault authentication failed")
	// ErrProfileInUse profile 仍在运行（SingletonLock 指向存活进程）或另一个封存操作持有锁。
	ErrProfileInUse = errors.New("browser: profile in use")
	// ErrVaultMalformed 封存文件结构不合法（魔数、头部、tar 结构、与清单不一致、自校验不符）。
	ErrVaultMalformed = errors.New("browser: profile vault malformed")
	// ErrVaultUnsafeEntry 封存里的条目会逃出目标目录或是不允许的类型（绝对路径、..、symlink 等）。
	ErrVaultUnsafeEntry = errors.New("browser: profile vault unsafe entry")
	// ErrVaultUnsupportedEntry 封存时遇到无法安全封存的条目（非运行期 symlink）。封存中止，明文不动。
	ErrVaultUnsupportedEntry = errors.New("browser: profile contains an entry that cannot be vaulted")
	// ErrVaultMissing 清单说 sealed 但 profile.vault 不在。
	ErrVaultMissing = errors.New("browser: profile vault file missing")
	// ErrVaultOrphaned 有 profile.vault 却没有清单。
	ErrVaultOrphaned = errors.New("browser: profile vault present without manifest")
)

// ProfileInUseError 是 ErrProfileInUse 的带细节版本。
type ProfileInUseError struct {
	Dir    string
	PID    int
	Reason string
}

func (e *ProfileInUseError) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf("browser: profile in use: %s (pid %d: %s)", e.Dir, e.PID, e.Reason)
	}
	return fmt.Sprintf("browser: profile in use: %s (%s)", e.Dir, e.Reason)
}

func (e *ProfileInUseError) Unwrap() error { return ErrProfileInUse }

// VaultReceipt 是封存/解封的回执，供 open / close / profile list 显示。
type VaultReceipt struct {
	Action       string `json:"action"`     // seal | unseal | noop
	Vault        string `json:"vault"`      // 操作后的清单 vault 状态
	OSCrypt      string `json:"os_crypt"`   // mock | system
	ProfileID    string `json:"profile_id"` //
	Protection   string `json:"protection"` // keychain | file
	Files        int    `json:"files,omitempty"`
	PlainBytes   int64  `json:"plain_bytes,omitempty"`
	VaultBytes   int64  `json:"vault_bytes,omitempty"`
	CachesPurged int    `json:"caches_purged,omitempty"`
	Verified     bool   `json:"verified,omitempty"` // 封存后已整体解密自校验
	DurationMs   int64  `json:"ms"`
	Note         string `json:"note,omitempty"`
}

// 封存/解封互斥锁最长等待。
var vaultLockWait = 30 * time.Second

// ErrVaultLockDirUnsafe vault 锁目录不是"当前用户私有的 0700 真目录"（symlink / 属主不对 / 权限过宽）。
var ErrVaultLockDirUnsafe = errors.New("browser: vault lock directory unsafe")

// vaultLockBaseDir 返回放 vault 锁目录的每用户基目录。变量只为让测试改位置。
var vaultLockBaseDir = os.UserCacheDir

// vaultSyncObserver / vaultBeforeVaultRemoveHook 是测试观测点：生产恒为 nil。
var (
	vaultSyncObserver          func(kind, path string)
	vaultBeforeVaultRemoveHook func(path string)
)

// vaultTotals 是写入头部的明文总量（头部受 AAD 认证）：解封时超出就中止。
type vaultTotals struct {
	Entries int
	Files   int
	Bytes   int64
}

// vaultSealHookAfterWrite 是测试注入点：在 tmp 封存写完、自校验开始之前调用，
// 让测试能构造"写出的封存与实际内容对不上"这类正常路径上不可能出现的故障。生产代码永远为 nil。
var vaultSealHookAfterWrite func(tmpPath string, master []byte, profileID string)

// ============================================================
// § 头部与密钥派生
// ============================================================

type vaultHeader struct {
	Version     int    `json:"v"`
	Alg         string `json:"alg"`
	KDF         string `json:"kdf"`
	Salt        []byte `json:"salt"`
	NoncePrefix []byte `json:"nonce_prefix"`
	ProfileID   string `json:"profile_id"`
	ChunkSize   int    `json:"chunk_size"`
	CreatedAt   string `json:"created_at"`
	// 明文总量：条目数（目录+文件）/ 常规文件数 / 总字节数。头部整体进 AAD，解封时超出就中止。
	Entries int   `json:"entries"`
	Files   int   `json:"files"`
	Bytes   int64 `json:"bytes"`
}

func (h *vaultHeader) validate() error {
	switch {
	case h.Version != 1:
		return fmt.Errorf("%w: unsupported version %d", ErrVaultMalformed, h.Version)
	case h.Alg != vaultAlgName || h.KDF != vaultKDFName:
		return fmt.Errorf("%w: unsupported algorithm", ErrVaultMalformed)
	case len(h.Salt) < 16 || len(h.Salt) > 64:
		return fmt.Errorf("%w: bad salt length", ErrVaultMalformed)
	case len(h.NoncePrefix) != vaultNoncePrefix:
		return fmt.Errorf("%w: bad nonce prefix length", ErrVaultMalformed)
	case h.ProfileID == "":
		return fmt.Errorf("%w: empty profile_id", ErrVaultMalformed)
	case h.ChunkSize < vaultMinChunkSize || h.ChunkSize > vaultMaxChunkSize:
		return fmt.Errorf("%w: bad chunk size %d", ErrVaultMalformed, h.ChunkSize)
	case h.Entries < 0 || h.Files < 0 || h.Bytes < 0 || h.Files > h.Entries:
		return fmt.Errorf("%w: bad content totals", ErrVaultMalformed)
	}
	return nil
}

// encodeVaultHeader 返回带成帧的头部字节（magic ‖ len ‖ json）及其 SHA256（AAD 的一部分）。
func encodeVaultHeader(h vaultHeader) ([]byte, [sha256.Size]byte, error) {
	body, err := json.Marshal(h)
	if err != nil {
		return nil, [sha256.Size]byte{}, err
	}
	framed := make([]byte, 0, len(vaultMagic)+4+len(body))
	framed = append(framed, vaultMagic...)
	framed = binary.BigEndian.AppendUint32(framed, uint32(len(body)))
	framed = append(framed, body...)
	return framed, sha256.Sum256(framed), nil
}

func readVaultHeader(r io.Reader) (vaultHeader, [sha256.Size]byte, error) {
	var h vaultHeader
	var zero [sha256.Size]byte
	magic := make([]byte, len(vaultMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return h, zero, fmt.Errorf("%w: truncated before magic", ErrVaultMalformed)
	}
	if string(magic) != vaultMagic {
		return h, zero, fmt.Errorf("%w: bad magic", ErrVaultMalformed)
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return h, zero, fmt.Errorf("%w: truncated header length", ErrVaultMalformed)
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n == 0 || n > vaultMaxHeaderLen {
		return h, zero, fmt.Errorf("%w: bad header length", ErrVaultMalformed)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return h, zero, fmt.Errorf("%w: truncated header", ErrVaultMalformed)
	}
	hash := sha256.New()
	hash.Write(magic)
	hash.Write(lenBuf[:])
	hash.Write(body)
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	if err := json.Unmarshal(body, &h); err != nil {
		return h, zero, fmt.Errorf("%w: header json", ErrVaultMalformed)
	}
	if err := h.validate(); err != nil {
		return h, zero, err
	}
	return h, digest, nil
}

func deriveVaultAEAD(master, salt []byte, profileID string) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, master, salt, vaultKDFInfoPrefix+profileID, 32)
	if err != nil {
		return nil, fmt.Errorf("browser: vault key derivation: %w", err)
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func vaultNonce(prefix []byte, idx uint64) []byte {
	nonce := make([]byte, 0, len(prefix)+4)
	nonce = append(nonce, prefix...)
	return binary.BigEndian.AppendUint32(nonce, uint32(idx))
}

func vaultAAD(dst []byte, digest [sha256.Size]byte, idx uint64, final bool) []byte {
	dst = append(dst[:0], digest[:]...)
	dst = binary.BigEndian.AppendUint64(dst, idx)
	if final {
		return append(dst, 1)
	}
	return append(dst, 0)
}

// ============================================================
// § 分块 AEAD 流
// ============================================================

// vaultWriter 把明文切成 chunkSize 一块加密写出。满块先扣着，直到确认后面还有数据才作为"非末块"
// 写出；Close 时把最后一块（可能为空、可能满块）作为末块写出。
type vaultWriter struct {
	w      io.Writer
	aead   cipher.AEAD
	prefix []byte
	digest [sha256.Size]byte
	buf    []byte
	n      int
	idx    uint64
	out    []byte
	aad    []byte
	closed bool
	err    error
}

func newVaultWriter(w io.Writer, aead cipher.AEAD, prefix []byte, digest [sha256.Size]byte, chunkSize int) *vaultWriter {
	return &vaultWriter{
		w: w, aead: aead, prefix: prefix, digest: digest,
		buf: make([]byte, chunkSize),
		out: make([]byte, 0, chunkSize+aead.Overhead()),
	}
}

func (v *vaultWriter) Write(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}
	if v.closed {
		return 0, errors.New("browser: vault writer closed")
	}
	total := len(p)
	for len(p) > 0 {
		if v.n == len(v.buf) {
			if err := v.flush(false); err != nil {
				v.err = err
				return total - len(p), err
			}
		}
		c := copy(v.buf[v.n:], p)
		v.n += c
		p = p[c:]
	}
	return total, nil
}

// Close 写出末块。不关闭底层 writer。
func (v *vaultWriter) Close() error {
	if v.closed {
		return v.err
	}
	v.closed = true
	if v.err != nil {
		return v.err
	}
	if err := v.flush(true); err != nil {
		v.err = err
	}
	return v.err
}

func (v *vaultWriter) flush(final bool) error {
	if v.idx > math.MaxUint32 {
		return errors.New("browser: vault too large")
	}
	v.aad = vaultAAD(v.aad, v.digest, v.idx, final)
	v.out = v.aead.Seal(v.out[:0], vaultNonce(v.prefix, v.idx), v.buf[:v.n], v.aad)
	if _, err := v.w.Write(v.out); err != nil {
		return err
	}
	v.idx++
	v.n = 0
	return nil
}

// vaultReader 是 vaultWriter 的逆。每块在放出明文之前先认证；末块认证通过才返回 io.EOF，
// 所以"读到 EOF"本身就是"整条流完整"的证明。
type vaultReader struct {
	r      *bufio.Reader
	aead   cipher.AEAD
	prefix []byte
	digest [sha256.Size]byte
	ct     []byte
	pbuf   []byte
	plain  []byte
	aad    []byte
	idx    uint64
	done   bool
	err    error
}

func newVaultReader(r *bufio.Reader, aead cipher.AEAD, prefix []byte, digest [sha256.Size]byte, chunkSize int) *vaultReader {
	return &vaultReader{
		r: r, aead: aead, prefix: prefix, digest: digest,
		ct:   make([]byte, chunkSize+aead.Overhead()),
		pbuf: make([]byte, 0, chunkSize),
	}
}

func (v *vaultReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(v.plain) == 0 {
		if v.err != nil {
			return 0, v.err
		}
		if v.done {
			return 0, io.EOF
		}
		if err := v.next(); err != nil {
			v.err = err
			return 0, err
		}
	}
	n := copy(p, v.plain)
	v.plain = v.plain[n:]
	return n, nil
}

func (v *vaultReader) next() error {
	n, err := io.ReadFull(v.r, v.ct)
	final := false
	switch {
	case err == nil:
		// 读满一整块：后面还有数据 → 非末块；恰好到头 → 它就是末块。
		if _, perr := v.r.Peek(1); perr != nil {
			if !errors.Is(perr, io.EOF) {
				return perr
			}
			final = true
		}
	case errors.Is(err, io.ErrUnexpectedEOF):
		final = true
	case errors.Is(err, io.EOF):
		// 写入侧至少会写一块（哪怕明文为空），一块都没有只能是被截断。
		return fmt.Errorf("%w: stream truncated", ErrVaultAuthFailed)
	default:
		return err
	}
	if n < v.aead.Overhead() {
		return fmt.Errorf("%w: stream truncated", ErrVaultAuthFailed)
	}
	if v.idx > math.MaxUint32 {
		return fmt.Errorf("%w: too many chunks", ErrVaultMalformed)
	}
	v.aad = vaultAAD(v.aad, v.digest, v.idx, final)
	plain, err := v.aead.Open(v.pbuf[:0], vaultNonce(v.prefix, v.idx), v.ct[:n], v.aad)
	if err != nil {
		return fmt.Errorf("%w: chunk %d", ErrVaultAuthFailed, v.idx)
	}
	v.plain = plain
	v.idx++
	v.done = final
	return nil
}

// ============================================================
// § 封存流的写与读（tar 之下）
// ============================================================

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("browser: entropy: %w", err)
	}
	return b, nil
}

// writeVaultStream 写出完整封存流：头部 + 分块密文。fill 往 tar.Writer 里写内容。
func writeVaultStream(w io.Writer, master []byte, profileID string, chunkSize int, totals vaultTotals, fill func(*tar.Writer) error) error {
	salt, err := randomBytes(vaultSaltLen)
	if err != nil {
		return err
	}
	prefix, err := randomBytes(vaultNoncePrefix)
	if err != nil {
		return err
	}
	hdr := vaultHeader{
		Version: 1, Alg: vaultAlgName, KDF: vaultKDFName,
		Salt: salt, NoncePrefix: prefix, ProfileID: profileID,
		ChunkSize: chunkSize, CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Entries: totals.Entries, Files: totals.Files, Bytes: totals.Bytes,
	}
	framed, digest, err := encodeVaultHeader(hdr)
	if err != nil {
		return err
	}
	aead, err := deriveVaultAEAD(master, salt, profileID)
	if err != nil {
		return err
	}
	if _, err := w.Write(framed); err != nil {
		return err
	}
	vw := newVaultWriter(w, aead, prefix, digest, chunkSize)
	tw := tar.NewWriter(vw)
	if err := fill(tw); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return vw.Close()
}

// openVaultStream 解析头部、派生密钥，返回明文 reader。expectProfileID 非空时必须与头部一致。
func openVaultStream(r io.Reader, master []byte, expectProfileID string) (*vaultReader, vaultHeader, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	hdr, digest, err := readVaultHeader(br)
	if err != nil {
		return nil, hdr, err
	}
	if expectProfileID != "" && hdr.ProfileID != expectProfileID {
		return nil, hdr, fmt.Errorf("%w: profile id does not match manifest", ErrVaultMalformed)
	}
	aead, err := deriveVaultAEAD(master, hdr.Salt, hdr.ProfileID)
	if err != nil {
		return nil, hdr, err
	}
	return newVaultReader(br, aead, hdr.NoncePrefix, digest, hdr.ChunkSize), hdr, nil
}

// vaultSummary 是对一份封存整体解密后得到的内容摘要。
type vaultSummary struct {
	files   int
	bytes   int64
	entries map[string]int64 // 条目名（无尾斜杠）→ 常规文件大小；目录为 -1
}

// verifyVaultFile 整体解密并走一遍 tar，返回内容摘要。它同时是"解封 dry-run"：每个条目都过一遍解封的接受规则
// （名字、类型、PAX、路径长度、冲突、头部总量），不落盘。dryRunDir 非空时，路径长度按
// "<dryRunDir>/.dw-unseal-XXXXXXXXXX/<条目>" 计算——也就是真正解封时会遇到的最坏情况。
func verifyVaultFile(vaultPath string, master []byte, profileID, dryRunDir string) (*vaultSummary, error) {
	f, err := os.Open(vaultPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	vr, hdr, err := openVaultStream(f, master, profileID)
	if err != nil {
		return nil, err
	}
	var fits func(string) bool
	if dryRunDir != "" {
		fits = vaultFitsUnderProfile(dryRunDir)
	}
	chk := newVaultEntryChecker(vaultTotals{Entries: hdr.Entries, Files: hdr.Files, Bytes: hdr.Bytes}, fits)
	sum := &vaultSummary{entries: map[string]int64{}}
	tr := tar.NewReader(vr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, mapTarError(err)
		}
		rel, err := chk.check(h)
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeDir {
			sum.entries[rel] = -1
			continue
		}
		n, err := io.Copy(io.Discard, tr)
		if err != nil {
			return nil, mapTarError(err)
		}
		sum.entries[rel] = n
		sum.files++
		sum.bytes += n
	}
	if _, err := io.Copy(io.Discard, vr); err != nil {
		return nil, err
	}
	if !vr.done {
		return nil, fmt.Errorf("%w: stream truncated", ErrVaultAuthFailed)
	}
	if err := chk.finish(); err != nil {
		return nil, err
	}
	return sum, nil
}

// mapTarError 保留认证失败的类型，其余 tar 解析错误归为结构不合法。
func mapTarError(err error) error {
	if errors.Is(err, ErrVaultAuthFailed) || errors.Is(err, ErrVaultMalformed) || errors.Is(err, ErrVaultUnsafeEntry) {
		return err
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: tar stream truncated", ErrVaultAuthFailed)
	}
	return fmt.Errorf("%w: %v", ErrVaultMalformed, err)
}

// ============================================================
// § 封存范围
// ============================================================

// vaultTopLevelExcluded：profile 根目录下不进封存的名字——清单、封存文件及其临时件、解封临时目录，
// 以及运行期产物（Singleton*、RunningChromeVersion、dw-browser 自己的 owner 标记，内容是 pid）。
// 解封时同一组名字出现在封存里一律拒绝，避免覆盖清单/vault。
func vaultTopLevelExcluded(name string) bool {
	switch {
	case name == ProfileManifestFile, name == ProfileVaultFile:
		return true
	case strings.HasPrefix(name, ProfileVaultFile+".tmp-"),
		strings.HasPrefix(name, ProfileManifestFile+".tmp-"),
		strings.HasPrefix(name, vaultUnsealTmpPrefix):
		return true
	case strings.HasPrefix(name, "Singleton"), name == "RunningChromeVersion", name == deepworkProfileOwnerFile:
		return true
	}
	return false
}

type vaultPlan struct {
	items        []vaultItem // 全部条目：封存的、只删不封的缓存/运行期产物/特殊文件；即本次的精确删除清单
	cachesPurged int
}

func (p *vaultPlan) totals() vaultTotals {
	var t vaultTotals
	for _, it := range p.archiveItems() {
		t.Entries++
		if it.info.Mode().IsRegular() {
			t.Files++
			t.Bytes += it.info.Size()
		}
	}
	return t
}

func (p *vaultPlan) archiveItems() []vaultItem {
	var out []vaultItem
	for _, it := range p.items {
		if it.kind == vaultItemArchive {
			out = append(out, it)
		}
	}
	return out
}

// planVaultEntries 先完整走一遍目录（不跟随 symlink），把每个条目归入：封存 / 缓存 / 运行期产物 / 特殊文件。
// 只删不封的子树也被展开成逐项清单——这份清单就是封存之后唯一允许删除的东西。
// 遇到无法安全封存的条目就在写任何字节之前中止。
func planVaultEntries(root string) (*vaultPlan, error) {
	plan := &vaultPlan{}
	var skipRel, cacheRoot string
	var skipKind vaultItemKind
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
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if skipRel != "" {
			if strings.HasPrefix(relSlash, skipRel+"/") {
				plan.items = append(plan.items, vaultItem{rel: relSlash, info: info, kind: skipKind, cacheRoot: cacheRoot})
				return nil
			}
			skipRel, cacheRoot = "", ""
		}
		name := d.Name()
		top := !strings.Contains(relSlash, "/")
		if top && (name == ProfileManifestFile || name == ProfileVaultFile) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if top && vaultTopLevelExcluded(name) {
			plan.items = append(plan.items, vaultItem{rel: relSlash, info: info, kind: vaultItemRuntime})
			if info.IsDir() {
				skipRel, skipKind = relSlash, vaultItemRuntime
			}
			return nil
		}
		if info.IsDir() && isRegenerableCachePath(relSlash) {
			plan.cachesPurged++
			plan.items = append(plan.items, vaultItem{rel: relSlash, info: info, kind: vaultItemCache, cacheRoot: relSlash})
			skipRel, skipKind, cacheRoot = relSlash, vaultItemCache, relSlash
			return nil
		}
		mode := info.Mode()
		switch {
		case mode.IsDir(), mode.IsRegular():
			// 封存接受的集合必须是解封接受集合的子集：解封会拒绝的名字，这里就中止，明文保留。
			if err := vaultArchivableName(root, relSlash); err != nil {
				return err
			}
			plan.items = append(plan.items, vaultItem{rel: relSlash, info: info, kind: vaultItemArchive})
		case mode&fs.ModeSymlink != 0:
			// 解封一律拒绝 symlink，所以这里也不能封：中止并保留明文，比悄悄丢文件安全。
			return fmt.Errorf("%w: symlink %s", ErrVaultUnsupportedEntry, relSlash)
		default:
			// socket / fifo / 设备：运行期产物，没有数据，不封存；列入删除清单。
			plan.items = append(plan.items, vaultItem{rel: relSlash, info: info, kind: vaultItemSpecial})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func writeVaultTar(tw *tar.Writer, root string, plan *vaultPlan) (files int, nbytes int64, err error) {
	for _, e := range plan.archiveItems() {
		hdr, err := tar.FileInfoHeader(e.info, "")
		if err != nil {
			return 0, 0, err
		}
		hdr.Name = e.rel
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		hdr.AccessTime, hdr.ChangeTime = time.Time{}, time.Time{}
		hdr.Mode = int64(e.info.Mode().Perm())
		if e.info.IsDir() {
			hdr.Name += "/"
			if err := tw.WriteHeader(hdr); err != nil {
				return 0, 0, err
			}
			continue
		}
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(e.rel)))
		if err != nil {
			return 0, 0, err
		}
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			_ = f.Close()
			return 0, 0, fmt.Errorf("browser: %s changed while sealing", e.rel)
		}
		if fi.Size() != e.info.Size() {
			_ = f.Close()
			return 0, 0, fmt.Errorf("browser: %s changed while sealing", e.rel)
		}
		hdr.Size = fi.Size()
		if err := tw.WriteHeader(hdr); err != nil {
			_ = f.Close()
			return 0, 0, err
		}
		n, err := io.Copy(tw, f)
		_ = f.Close()
		if err != nil {
			return 0, 0, err
		}
		if n != hdr.Size {
			return 0, 0, fmt.Errorf("browser: %s changed while sealing", e.rel)
		}
		files++
		nbytes += n
	}
	return files, nbytes, nil
}

// ============================================================
// § 解封：安全提取
// ============================================================

// safeVaultRel 校验 tar 条目名：相对、不含 ..、不含反斜杠/NUL、不碰保留名。返回去掉尾斜杠的正斜杠路径。
func safeVaultRel(name string) (string, error) {
	bad := func(why string) (string, error) {
		return "", fmt.Errorf("%w: %s (%q)", ErrVaultUnsafeEntry, why, name)
	}
	if name == "" {
		return bad("empty name")
	}
	if strings.ContainsRune(name, 0) || strings.Contains(name, "\\") {
		return bad("illegal character")
	}
	trimmed := strings.TrimSuffix(name, "/")
	if strings.HasPrefix(trimmed, "/") || filepath.IsAbs(trimmed) || filepath.VolumeName(trimmed) != "" {
		return bad("absolute path")
	}
	for _, el := range strings.Split(trimmed, "/") {
		switch el {
		case "":
			return bad("empty path element")
		case ".":
			return bad("dot path element")
		case "..":
			return bad("parent path element")
		}
	}
	if !filepath.IsLocal(filepath.FromSlash(trimmed)) {
		return bad("non-local path")
	}
	top, _, _ := strings.Cut(trimmed, "/")
	if vaultTopLevelExcluded(top) {
		return bad("reserved top-level name")
	}
	return trimmed, nil
}

type extractResult struct {
	files    int
	nbytes   int64
	dirModes map[string]fs.FileMode // rel → 目录权限，解封完成后才应用
}

// syncVaultFile / syncVaultDir 把提取出来的数据真正落盘；observer 仅供测试核对"落盘发生在清单置 open 之前"。
func syncVaultFile(f *os.File) error {
	if vaultSyncObserver != nil {
		vaultSyncObserver("file", f.Name())
	}
	return f.Sync()
}

func syncVaultDir(dir string) error {
	if vaultSyncObserver != nil {
		vaultSyncObserver("dir", dir)
	}
	if runtime.GOOS == "windows" {
		return nil // Windows 不支持对目录 fsync
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	serr := d.Sync()
	if cerr := d.Close(); serr == nil {
		serr = cerr
	}
	return serr
}

// extractVaultTar 把明文 tar 流解到 dest（一个全新的空目录）。每个条目先过 vaultEntryChecker（和封存自校验
// dry-run 同一套规则，含头部声明的总量），只接受常规文件和目录。
// 结束时必须把流读到头，让末块被认证——调用方据此才能信任提取结果；
// 并把每个文件、每个目录 fsync 之后才返回，这样调用方把清单置 open 时数据已经落盘。
func extractVaultTar(vr *vaultReader, dest string, limits vaultTotals) (*extractResult, error) {
	res := &extractResult{dirModes: map[string]fs.FileMode{}}
	chk := newVaultEntryChecker(limits, func(rel string) bool {
		return len(filepath.Join(dest, filepath.FromSlash(rel))) < vaultPathLimit()
	})
	dirs := map[string]struct{}{"": {}} // 要 fsync 的目录（相对 dest，"" 是 dest 自己）
	noteParents := func(rel string) {
		for p := path.Dir(rel); p != "." && p != "/"; p = path.Dir(p) {
			dirs[p] = struct{}{}
		}
	}
	tr := tar.NewReader(vr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, mapTarError(err)
		}
		rel, err := chk.check(h)
		if err != nil {
			return nil, err
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrVaultMalformed, err)
			}
			res.dirModes[rel] = fs.FileMode(h.Mode).Perm()
			dirs[rel] = struct{}{}
			noteParents(rel)
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrVaultMalformed, err)
			}
			noteParents(rel)
			// O_EXCL：重名（含"文件与目录同名"）一律拒绝，也不会写穿任何已有的东西。
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrVaultMalformed, err)
			}
			n, cerr := io.Copy(f, tr)
			if cerr == nil && n != h.Size {
				cerr = fmt.Errorf("%w: short file %s", ErrVaultMalformed, rel)
			}
			if cerr == nil {
				cerr = f.Chmod(fs.FileMode(h.Mode).Perm())
			}
			if cerr == nil && !h.ModTime.IsZero() {
				_ = os.Chtimes(target, h.ModTime, h.ModTime)
			}
			if cerr == nil {
				cerr = syncVaultFile(f)
			}
			if clErr := f.Close(); cerr == nil {
				cerr = clErr
			}
			if cerr != nil {
				return nil, mapTarError(cerr)
			}
			res.files++
			res.nbytes += n
		}
	}
	if _, err := io.Copy(io.Discard, vr); err != nil {
		return nil, mapTarError(err)
	}
	if !vr.done {
		return nil, fmt.Errorf("%w: stream truncated", ErrVaultAuthFailed)
	}
	if err := chk.finish(); err != nil {
		return nil, err
	}
	for rel := range dirs {
		if err := syncVaultDir(filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
			return nil, fmt.Errorf("browser: fsync extracted directory %q: %w", rel, err)
		}
	}
	return res, nil
}

func applyDirModes(root string, modes map[string]fs.FileMode) error {
	rels := make([]string, 0, len(modes))
	for rel := range modes {
		rels = append(rels, rel)
	}
	// 深的先改：父目录收紧权限之前，子目录还能访问。
	sort.Slice(rels, func(i, j int) bool {
		return strings.Count(rels[i], "/") > strings.Count(rels[j], "/")
	})
	for _, rel := range rels {
		if err := os.Chmod(filepath.Join(root, filepath.FromSlash(rel)), modes[rel]); err != nil {
			return err
		}
	}
	return nil
}

// ============================================================
// § 通用辅助
// ============================================================

func resolveProfileDir(profileDir string) (string, error) {
	abs, err := filepath.Abs(profileDir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("browser: profile path is not a directory: %s", profileDir)
	}
	return real, nil
}

// vaultLockDir 返回每用户私有的锁目录（os.UserCacheDir()/dw-browser/vault-locks），并校验：
// 是真目录（不是 symlink）、属主是当前用户、权限是 0700。不往全局可写的 /tmp 放锁——
// 否则别的本地用户可以预先占位 / 换成 symlink，让封存永远拿不到锁或把锁文件建到别处。
func vaultLockDir() (string, error) {
	base, err := vaultLockBaseDir()
	if err != nil || base == "" {
		return "", fmt.Errorf("%w: cannot determine the per-user cache directory", ErrVaultLockDirUnsafe)
	}
	dir := filepath.Join(base, "dw-browser", "vault-locks")
	if err := mkdirAllPrivate(dir); err != nil {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return "", fmt.Errorf("%w: %s is not a real directory", ErrVaultLockDirUnsafe, dir)
	}
	if runtime.GOOS != "windows" {
		if uid, ok := vaultFileOwner(fi); ok && uid != os.Getuid() {
			return "", fmt.Errorf("%w: %s is owned by uid %d, not %d", ErrVaultLockDirUnsafe, dir, uid, os.Getuid())
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 || perm&0o700 != 0o700 {
			return "", fmt.Errorf("%w: %s has mode %04o, want 0700", ErrVaultLockDirUnsafe, dir, perm)
		}
	}
	return dir, nil
}

// acquireProfileVaultLock 取"这个 profile 目录的封存/解封"排他锁。锁文件放在每用户私有目录里按路径哈希命名，
// 不往 profile 目录里加文件（封存后目录只剩 2 个文件）。进程死亡内核自动放锁。
func acquireProfileVaultLock(dir string) (exclusiveFileLock, error) {
	lockDir, err := vaultLockDir()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(dir))
	lock, ok, err := acquireExclusiveFileLock(filepath.Join(lockDir, hex.EncodeToString(sum[:8])+".lock"), vaultLockWait)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, &ProfileInUseError{Dir: dir, Reason: "another seal/unseal operation holds the profile lock"}
	}
	return lock, nil
}

// ensureProfileNotRunning 判断 profile 是否还有人在用，任何一条证据成立就拒绝：
//  1. SingletonLock 指向存活进程（解析不了的锁按陈旧处理，与 startup_recovery 的 cleanSingletonLocks 一致）；
//  2. owner marker（.deepwork-browser-owner.json）里的 owner / chrome / mux host pid 有存活的（不含本进程：
//     mux host 在自己进程里封存时，marker 里写的就是它自己）；
//  3. 进程列表里有命令行带 --user-data-dir=<dir> 的进程（不含本进程）。
//
// 2、3 补的是"Chrome 已经起来、但 SingletonLock 还没建好"的窗口。
func ensureProfileNotRunning(dir string) error {
	self := os.Getpid()
	lock := filepath.Join(dir, "SingletonLock")
	if _, err := os.Lstat(lock); err == nil {
		if pid, perr := readSingletonLockPID(lock); perr == nil && isPIDAlive(pid) {
			return &ProfileInUseError{Dir: dir, PID: pid, Reason: "SingletonLock points at a live process"}
		}
	}
	if body, err := os.ReadFile(filepath.Join(dir, deepworkProfileOwnerFile)); err == nil {
		var marker deepworkProfileOwner
		if json.Unmarshal(body, &marker) == nil {
			for _, c := range []struct {
				field string
				pid   int
			}{{"owner_pid", marker.OwnerPID}, {"chrome_pid", marker.ChromePID}, {"browser_mux_host_pid", marker.BrowserMuxHostPID}} {
				if c.pid > 0 && c.pid != self && isPIDAlive(c.pid) {
					return &ProfileInUseError{Dir: dir, PID: c.pid, Reason: "owner marker " + c.field + " is a live process"}
				}
			}
		}
	}
	for _, pid := range platformFindChromePIDsByProfileDir(dir) {
		if pid > 0 && pid != self {
			return &ProfileInUseError{Dir: dir, PID: pid, Reason: "a process was started with --user-data-dir for this profile"}
		}
	}
	return nil
}

// hasArchivablePlaintext：目录里是否有会被封存的明文（即规划结果里有"封存类"条目）。
// 遇到不能封存的条目（symlink、名字不合规）说明那里确实有用户数据，同样算有。
func hasArchivablePlaintext(dir string) (bool, error) {
	plan, err := planVaultEntries(dir)
	if err != nil {
		if errors.Is(err, ErrVaultUnsupportedEntry) {
			return true, nil
		}
		return false, err
	}
	return len(plan.archiveItems()) > 0, nil
}

// removeVaultFile 删除 profile.vault。调用方必须先关闭自己持有的句柄（Windows 不让删除打开着的文件）；
// 测试钩子在这里核对"确实已经关闭"。
func removeVaultFile(dir, vaultPath string) error {
	if vaultBeforeVaultRemoveHook != nil {
		vaultBeforeVaultRemoveHook(vaultPath)
	}
	return removeFileInRoot(dir, vaultPath)
}

func hasEntriesBeyond(dir string, keep ...string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	keepSet := make(map[string]bool, len(keep))
	for _, k := range keep {
		keepSet[k] = true
	}
	for _, e := range entries {
		if !keepSet[e.Name()] {
			return true
		}
	}
	return false
}

// ============================================================
// § SealProfile
// ============================================================

// SealProfile 把 profileDir 封存成 profile.vault 并删除全部明文，最后目录里只剩清单和封存文件。
//
//   - ks 为 nil 时用 DefaultProfileKeyStore()；主密钥不存在会自动创建（仅"真封存"这条路径）。
//   - 清单 vault=none → noop。
//   - 清单 sealed：目录里没有别的东西 → noop；有残留明文 → 加载主密钥并整体验证 vault，通过后经清单化删除
//     清掉残留（vault 本身一个字节不动）；任何一步不过，残留原样保留。
//   - 清单 open 且已有 vault 但目录里没有明文：按 sealed 处理（修正清单），绝不用空封存覆盖 vault。
//   - 在运行（SingletonLock / owner marker / --user-data-dir 进程）→ ErrProfileInUse。
//   - 遇到解封会拒绝或无法安全封存的条目 → ErrVaultUnsupportedEntry，明文原样保留。
//   - 任何失败发生在"清单置 sealed"之前，都保证明文完好、清单仍为 open、没有临时文件遗留。
func SealProfile(profileDir string, ks KeyStore) (rc VaultReceipt, err error) {
	start := time.Now()
	if ks == nil {
		ks = DefaultProfileKeyStore()
	}
	rc = VaultReceipt{Action: "seal", Protection: ks.Protection()}
	defer func() { rc.DurationMs = time.Since(start).Milliseconds() }()

	dir, err := resolveProfileDir(profileDir)
	if err != nil {
		return rc, err
	}
	lock, err := acquireProfileVaultLock(dir)
	if err != nil {
		return rc, err
	}
	defer lock.Close()

	m, err := ReadProfileManifest(dir)
	if err != nil {
		return rc, err
	}
	rc.ProfileID, rc.OSCrypt, rc.Vault = m.ProfileID, m.OSCrypt, m.Vault

	vaultPath := filepath.Join(dir, ProfileVaultFile)
	_, statErr := os.Lstat(vaultPath)
	staleVault := statErr == nil

	switch m.Vault {
	case ProfileVaultNone:
		rc.Action, rc.Note = "noop", "vault disabled for this profile"
		return rc, nil
	case ProfileVaultSealed:
		return sealDiscardLeftovers(dir, m, ks, rc)
	case ProfileVaultOpen:
		if staleVault {
			plain, perr := hasArchivablePlaintext(dir)
			if perr != nil {
				return rc, perr
			}
			if !plain {
				// 清单被回滚 / 写坏成 open，但目录里只有 vault：它才是数据的唯一副本。
				// 封存一个空目录会把它覆盖掉，所以改为修正清单。
				sealed := *m
				sealed.Vault = ProfileVaultSealed
				if err := WriteProfileManifest(dir, &sealed); err != nil {
					return rc, fmt.Errorf("browser: repair manifest: %w", err)
				}
				rc.Action, rc.Vault = "noop", ProfileVaultSealed
				rc.Note = "manifest said open but only a vault is present; treated as sealed and left untouched"
				return rc, nil
			}
		}
	}

	if err := ensureProfileNotRunning(dir); err != nil {
		return rc, err
	}
	master, err := LoadOrCreateMasterKey(ks)
	if err != nil {
		return rc, err
	}
	defer clear(master)

	plan, err := planVaultEntries(dir)
	if err != nil {
		return rc, err
	}

	tmp, err := os.CreateTemp(dir, ProfileVaultFile+".tmp-*")
	if err != nil {
		return rc, err
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = tmp.Close()
			_ = removeFileInRoot(dir, tmpPath)
		}
	}()

	var files int
	var nbytes int64
	if err := writeVaultStream(tmp, master, m.ProfileID, vaultChunkSize, plan.totals(), func(tw *tar.Writer) error {
		var ferr error
		files, nbytes, ferr = writeVaultTar(tw, dir, plan)
		return ferr
	}); err != nil {
		return rc, err
	}
	if err := tmp.Sync(); err != nil {
		return rc, err
	}
	if err := tmp.Close(); err != nil {
		return rc, err
	}
	fi, err := os.Stat(tmpPath)
	if err != nil {
		return rc, err
	}
	if vaultSealHookAfterWrite != nil {
		vaultSealHookAfterWrite(tmpPath, master, m.ProfileID)
	}

	// 删除明文不可逆，所以先把刚写好的封存整体解密一遍，同时按解封的接受规则做 dry-run：
	// 每个块都认证、每个条目都是解封会接受的、文件数与字节数对得上头部声明。
	sum, err := verifyVaultFile(tmpPath, master, m.ProfileID, dir)
	if err != nil {
		return rc, fmt.Errorf("browser: vault self-verification failed: %w", err)
	}
	if sum.files != files || sum.bytes != nbytes {
		return rc, fmt.Errorf("%w: self-verification mismatch (files %d/%d, bytes %d/%d)", ErrVaultMalformed, sum.files, files, sum.bytes, nbytes)
	}

	// 删除清单 = 本次规划出的精确条目列表。在提交（清单置 sealed）之前先整体校验：路径都在 profile 根内、
	// 祖先不是 symlink、条目自规划后没被改动、没有计划外条目、缓存项都命中允许列表，
	// 并且封存类条目与刚验证过的 vault 内容逐项一致。任何一项不过 → 中止，什么都不删，清单仍为 open。
	deletion := &vaultDeletePlan{
		root:       dir,
		keep:       newVaultKeep(ProfileManifestFile, ProfileVaultFile, filepath.Base(tmpPath)),
		items:      plan.items,
		coversRoot: true,
	}
	if err := deletion.validate(sum.entries); err != nil {
		return rc, fmt.Errorf("browser: seal aborted before commit, nothing deleted: %w", err)
	}

	if err := os.Rename(tmpPath, vaultPath); err != nil {
		return rc, err
	}
	renamed = true
	syncDir(dir)

	// 提交点：清单置 sealed。之后即使在删除明文途中崩溃，也以 vault 为准。
	sealed := *m
	sealed.Vault = ProfileVaultSealed
	if err := WriteProfileManifest(dir, &sealed); err != nil {
		// vault 已就位但清单仍是 open：状态自洽，明文为准，vault 作废。
		return rc, fmt.Errorf("browser: commit sealed manifest: %w", err)
	}
	rc.Vault = ProfileVaultSealed
	rc.Files, rc.PlainBytes, rc.VaultBytes = files, nbytes, fi.Size()
	rc.CachesPurged, rc.Verified = plan.cachesPurged, true
	if staleVault {
		rc.Note = "replaced stale vault from an interrupted run"
	}

	// 逐项删除（execute 会在删除前再整体校验一遍）。清单已是 sealed：中途失败也以 vault 为准，
	// 残留明文由下一次 SealProfile / UnsealProfile 清掉。
	if err := deletion.execute(); err != nil {
		return rc, fmt.Errorf("browser: sealed, but plaintext cleanup incomplete: %w", err)
	}
	syncDir(dir)
	return rc, nil
}

// sealDiscardLeftovers 处理"清单 sealed、目录里却还有别的东西"：崩溃在"提交之后、删除明文途中"留下的残留。
// 只有在 vault 确实存在、主密钥可用、整个 vault 通过认证与 dry-run、且 profile 没在运行时，才清理残留；
// 任何一步不过，残留原样保留，由调用方决定。vault 本身一个字节都不动。
func sealDiscardLeftovers(dir string, m *ProfileManifest, ks KeyStore, rc VaultReceipt) (VaultReceipt, error) {
	if !hasEntriesBeyond(dir, ProfileManifestFile, ProfileVaultFile) {
		rc.Action, rc.Note = "noop", "already sealed"
		return rc, nil
	}
	vaultPath := filepath.Join(dir, ProfileVaultFile)
	fi, err := os.Lstat(vaultPath)
	if err != nil {
		return rc, fmt.Errorf("%w: %s", ErrVaultMissing, vaultPath)
	}
	if err := ensureProfileNotRunning(dir); err != nil {
		return rc, err
	}
	master, err := LoadMasterKey(ks)
	if err != nil {
		return rc, err
	}
	defer clear(master)
	sum, err := verifyVaultFile(vaultPath, master, m.ProfileID, dir)
	if err != nil {
		return rc, fmt.Errorf("browser: vault failed verification, leftovers kept: %w", err)
	}
	keep := newVaultKeep(ProfileManifestFile, ProfileVaultFile)
	if err := removeVaultTree(dir, keep, func(top string) bool { return !keep[top] }, vaultItemLeftover, true); err != nil {
		return rc, fmt.Errorf("browser: discard leftovers: %w", err)
	}
	syncDir(dir)
	rc.Action, rc.Verified = "seal", true
	rc.Files, rc.PlainBytes, rc.VaultBytes = sum.files, sum.bytes, fi.Size()
	rc.Note = "discarded plaintext leftovers from an interrupted seal; vault verified and left untouched"
	return rc, nil
}

// ============================================================
// § UnsealProfile
// ============================================================

// UnsealProfile 把 profile.vault 还原成明文目录，清单置 open，删除封存文件。
//
//   - 清单 vault=none → noop；open 且目录里有明文 → noop，并丢弃崩溃遗留的旧 profile.vault（以明文为准）。
//   - 清单 open 且有 vault、但目录里没有明文：按 sealed 处理去解封，绝不先删 vault。
//   - 清单缺失但有 profile.vault → 用头部里的 profile_id 恢复一份清单（mock / open）。
//   - 主密钥只读不建：找不到 → ErrVaultKeyUnavailable。
//   - 错误密钥 / 篡改 / 截断 / 重排 → ErrVaultAuthFailed；恶意条目 → ErrVaultUnsafeEntry；
//     超出头部声明的总量 → ErrVaultMalformed。失败时目录原样保留（清单 sealed + profile.vault），
//     不留临时目录，也不动残留。
func UnsealProfile(profileDir string, ks KeyStore) (VaultReceipt, error) {
	rc, release, err := UnsealProfileForLaunch(profileDir, ks)
	release()
	return rc, err
}

// UnsealProfileForLaunch 解封并"持有启动租约"：返回的 release 被调用之前，vault 锁一直被持有，
// 其他进程的 SealProfile / UnsealProfile 都会等待（超时返回 ErrProfileInUse）。
//
// 为什么需要：解封完成到 Chrome 建好 SingletonLock 之间有一段窗口，此时另一个进程调 SealProfile，
// 看不到任何"在运行"的证据，会把刚解封的 profile 又封回去。集成方应在 Chrome 建好 SingletonLock
// 之后才 release，这样 Seal 一侧的 ensureProfileNotRunning 才看得见它。
//
// 无论成功与否，返回的 release 都非 nil，可无条件调用，可重复调用；出错时锁已经释放。
func UnsealProfileForLaunch(profileDir string, ks KeyStore) (rc VaultReceipt, release func(), err error) {
	start := time.Now()
	if ks == nil {
		ks = DefaultProfileKeyStore()
	}
	rc = VaultReceipt{Action: "unseal", Protection: ks.Protection()}
	release = func() {}
	defer func() { rc.DurationMs = time.Since(start).Milliseconds() }()

	dir, err := resolveProfileDir(profileDir)
	if err != nil {
		return rc, release, err
	}
	lock, err := acquireProfileVaultLock(dir)
	if err != nil {
		return rc, release, err
	}
	if err := unsealLocked(dir, ks, &rc); err != nil {
		_ = lock.Close()
		return rc, release, err
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = lock.Close() }) }
	return rc, release, nil
}

// unsealLocked 是解封本体，调用方必须已持有 vault 锁。
func unsealLocked(dir string, ks KeyStore, rc *VaultReceipt) (err error) {
	vaultPath := filepath.Join(dir, ProfileVaultFile)
	_, vaultStatErr := os.Lstat(vaultPath)
	vaultExists := vaultStatErr == nil

	m, merr := ReadProfileManifest(dir)
	recovered := false
	var notes []string
	if merr != nil {
		if !errors.Is(merr, ErrProfileManifestNotFound) || !vaultExists {
			return merr
		}
		recovered = true
	} else {
		rc.ProfileID, rc.OSCrypt, rc.Vault = m.ProfileID, m.OSCrypt, m.Vault
		switch m.Vault {
		case ProfileVaultNone:
			rc.Action, rc.Note = "noop", "vault disabled for this profile"
			return nil
		case ProfileVaultOpen:
			if !vaultExists {
				rc.Action, rc.Note = "noop", "already open"
				return nil
			}
			plain, perr := hasArchivablePlaintext(dir)
			if perr != nil {
				return perr
			}
			if plain {
				// 崩溃遗留（封存到一半 / 解封后没来得及删 vault）：明文是完整且最新的，旧 vault 作废。
				if rmErr := removeVaultFile(dir, vaultPath); rmErr != nil {
					return fmt.Errorf("browser: discard stale vault: %w", rmErr)
				}
				syncDir(dir)
				rc.Action, rc.Note = "noop", "already open; discarded stale vault from an interrupted run"
				return nil
			}
			// 清单说 open、目录里却只有 vault：vault 是数据的唯一副本。按 sealed 处理，成功之前绝不动它。
			notes = append(notes, "manifest said open but only a vault is present; treated as sealed")
		}
		if !vaultExists {
			return fmt.Errorf("%w: %s", ErrVaultMissing, vaultPath)
		}
	}

	if err := ensureProfileNotRunning(dir); err != nil {
		return err
	}
	master, err := LoadMasterKey(ks)
	if err != nil {
		return err
	}
	defer clear(master)

	vf, err := os.Open(vaultPath)
	if err != nil {
		return err
	}
	vaultClosed := false
	closeVault := func() {
		if !vaultClosed {
			vaultClosed = true
			_ = vf.Close()
		}
	}
	defer closeVault()

	expectID := ""
	if m != nil {
		expectID = m.ProfileID
	}
	vr, hdr, err := openVaultStream(vf, master, expectID)
	if err != nil {
		return err
	}
	if recovered {
		rc.ProfileID, rc.OSCrypt = hdr.ProfileID, ProfileOSCryptMock
	}

	tmpDir, err := os.MkdirTemp(dir, vaultUnsealTmpPrefix+"*")
	if err != nil {
		return err
	}
	tmpName := filepath.Base(tmpDir)
	// protectedTop：任何删除都不得触碰的顶层名；keep 额外包含本次的临时目录（清残留时它既不删也不算"计划外"）。
	protectedTop := newVaultKeep(ProfileManifestFile, ProfileVaultFile)
	keep := newVaultKeep(ProfileManifestFile, ProfileVaultFile, tmpName)
	moved := false
	defer func() {
		if moved {
			return
		}
		// 失败路径：只按"展开后的精确清单"清理自己创建的临时目录树，不碰目录里的任何其他东西。
		cerr := removeVaultTree(dir, protectedTop, func(top string) bool { return top == tmpName }, vaultItemTemp, false)
		if cerr != nil && err != nil {
			err = fmt.Errorf("%w (additionally, could not clean temp dir %s: %v)", err, tmpName, cerr)
		}
	}()

	res, err := extractVaultTar(vr, tmpDir, vaultTotals{Entries: hdr.Entries, Files: hdr.Files, Bytes: hdr.Bytes})
	if err != nil {
		return err
	}
	var vaultSize int64
	if fi, serr := vf.Stat(); serr == nil {
		vaultSize = fi.Size()
	}
	// 整条流已读完并认证：先关掉句柄（Windows 不允许删除打开着的文件），后面要删 vault。
	closeVault()

	// 整条流（含末块）已认证。现在才清掉封存后残留的明文并移入——此前任何失败都不碰目录原状。
	if err := removeVaultTree(dir, keep, func(top string) bool { return !keep[top] }, vaultItemLeftover, true); err != nil {
		return fmt.Errorf("browser: clear leftovers before unseal: %w", err)
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Rename(filepath.Join(tmpDir, e.Name()), filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("browser: move unsealed entry %s: %w", e.Name(), err)
		}
	}
	moved = true
	if err := removeVaultTree(dir, protectedTop, func(top string) bool { return top == tmpName }, vaultItemTemp, false); err != nil {
		return fmt.Errorf("browser: unsealed, but could not remove temp dir: %w", err)
	}
	if err := applyDirModes(dir, res.dirModes); err != nil {
		return err
	}
	// 文件和各级目录在 extractVaultTar 里已经 fsync；移入之后再 fsync 根目录，让"目录项已经就位"落盘。
	// 全部落盘之后才允许把清单置 open——否则断电后清单说 open、数据却没写下去。
	if err := syncVaultDir(dir); err != nil {
		return fmt.Errorf("browser: fsync profile directory: %w", err)
	}

	var open ProfileManifest
	if recovered {
		open = ProfileManifest{
			Schema: profileManifestSchema, ProfileID: hdr.ProfileID,
			OSCrypt: ProfileOSCryptMock, Vault: ProfileVaultOpen,
			CreatedMode: "recovered", CreatedAt: hdr.CreatedAt,
		}
		notes = append(notes, "manifest was missing; recovered from vault header")
	} else {
		open = *m
		open.Vault = ProfileVaultOpen
	}
	if err := WriteProfileManifest(dir, &open); err != nil {
		return err
	}
	rc.Vault = ProfileVaultOpen
	rc.Files, rc.PlainBytes, rc.VaultBytes = res.files, res.nbytes, vaultSize
	rc.Note = strings.Join(notes, "; ")
	if err := removeVaultFile(dir, vaultPath); err != nil {
		// 清单已 open + vault 残留：以明文为准，下一次封存会覆盖它。
		return fmt.Errorf("browser: unsealed, but could not remove vault file: %w", err)
	}
	syncDir(dir)
	return nil
}

// vaultPathLimit 是当前平台单个路径的长度上限（PATH_MAX），解封时用它判断"加上临时目录前缀后还放不放得下"。
func vaultPathLimit() int {
	switch runtime.GOOS {
	case "darwin":
		return 1024
	case "windows":
		return 260
	default:
		return 4096
	}
}
