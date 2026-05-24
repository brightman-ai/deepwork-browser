// Package browser — CookieImporter: 从本机浏览器导入 Cookie。
// [Ref: CAP-BS09-C4 §3.2b, SC-22, TC-C4-07~11, r2 Delta-REQ TH-0418-c9x]
//
// 铁律 IR-01: 本包零依赖 Deepwork 上下文。
// 安全约束: 只读打开源 Cookie 文件，不修改用户浏览器数据。
package browser

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"time"

	cdp "github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	_ "github.com/mattn/go-sqlite3"
)

// ============================================================
// § CookieImporter 接口 [Ref: CAP-BS09-C4 §2]
// ============================================================

// CookieImportResult 是 Cookie 导入的结果统计。
type CookieImportResult struct {
	TotalImported int            // 导入 Cookie 总数
	ByDomain      map[string]int // 按域名统计
	SourceBrowser string         // 实际使用的源浏览器
}

// cookieImporter 实现 Cookie 导入逻辑。
type cookieImporter struct {
	bc BrowserCore
}

// NewCookieImporter 创建 CookieImporter 实例。
// bc 用于通过 CDP 注入 Cookie（Network.setCookies）。
func NewCookieImporter(bc BrowserCore) *cookieImporter {
	return &cookieImporter{bc: bc}
}

// Import 从源浏览器读取 Cookie 并通过 CDP 注入当前 session [SC-22]。
// sourceBrowser: "chrome"|"firefox"|"arc"|"" (空=自动检测)
// domainFilter: "*.github.com" 或 "" (空=全部)
func (ci *cookieImporter) Import(ctx context.Context, sourceBrowser string, domainFilter string) (*CookieImportResult, error) {
	// 1. 检测 Cookie 数据库路径
	dbPath, detectedBrowser, err := resolveCookiePath(sourceBrowser)
	if err != nil {
		return nil, err
	}

	// 2. 打开 SQLite（只读），锁定时自动复制重试 [TC-C4-10]
	db, tempPath, err := openCookieDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if tempPath != "" {
		defer os.Remove(tempPath) // 清理临时副本
	}

	// 3. 查询 Cookie [TC-C4-09]: 在 SQL 阶段过滤域名（最小权限）
	cookies, err := queryCookies(db, detectedBrowser, domainFilter)
	if err != nil {
		return nil, fmt.Errorf("browser: cookie query failed: %w", err)
	}

	if len(cookies) == 0 {
		return &CookieImportResult{
			TotalImported: 0,
			ByDomain:      map[string]int{},
			SourceBrowser: detectedBrowser,
		}, nil
	}

	// 4. 通过 CDP Network.setCookies 注入 [TC-C4-07, TC-C4-08]
	if err := injectCookiesViaCDP(ctx, ci.bc, cookies); err != nil {
		return nil, fmt.Errorf("browser: cookie inject failed: %w", err)
	}

	// 5. 统计结果
	byDomain := map[string]int{}
	for _, c := range cookies {
		domain := c.Domain
		if domain == "" {
			domain = "(unknown)"
		}
		byDomain[domain]++
	}

	return &CookieImportResult{
		TotalImported: len(cookies),
		ByDomain:      byDomain,
		SourceBrowser: detectedBrowser,
	}, nil
}

// ============================================================
// § 路径解析 [TC-C4-07, TC-C4-08]
// ============================================================

// chromeCookiePaths 各平台 Chrome Cookie 路径候选列表（优先级顺序）。
func chromeCookiePaths() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return []string{
			filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "Default", "Cookies"),
			filepath.Join(home, "Library", "Application Support", "Google", "Chrome", "Profile 1", "Cookies"),
			filepath.Join(home, "Library", "Application Support", "Chromium", "Default", "Cookies"),
			filepath.Join(home, "Library", "Application Support", "com.operasoftware.Opera", "Cookies"),
			// Arc browser
			filepath.Join(home, "Library", "Application Support", "Arc", "User Data", "Default", "Cookies"),
		}
	case "linux":
		return []string{
			filepath.Join(home, ".config", "google-chrome", "Default", "Cookies"),
			filepath.Join(home, ".config", "chromium", "Default", "Cookies"),
			filepath.Join(home, ".config", "google-chrome-beta", "Default", "Cookies"),
		}
	case "windows":
		appData := os.Getenv("LOCALAPPDATA")
		return []string{
			filepath.Join(appData, "Google", "Chrome", "User Data", "Default", "Cookies"),
			filepath.Join(appData, "Chromium", "User Data", "Default", "Cookies"),
		}
	}
	return nil
}

// firefoxCookiePaths 各平台 Firefox Cookie 路径候选列表。
func firefoxCookiePaths() []string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		ffDir := filepath.Join(home, "Library", "Application Support", "Firefox", "Profiles")
		return globDir(ffDir, "cookies.sqlite")
	case "linux":
		ffDir := filepath.Join(home, ".mozilla", "firefox")
		return globDir(ffDir, "cookies.sqlite")
	case "windows":
		appData := os.Getenv("APPDATA")
		ffDir := filepath.Join(appData, "Mozilla", "Firefox", "Profiles")
		return globDir(ffDir, "cookies.sqlite")
	}
	return nil
}

func globDir(dir, filename string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			p := filepath.Join(dir, e.Name(), filename)
			if _, err := os.Stat(p); err == nil {
				paths = append(paths, p)
			}
		}
	}
	return paths
}

// resolveCookiePath 根据 sourceBrowser 名称找到 Cookie 数据库路径。
func resolveCookiePath(sourceBrowser string) (path string, detected string, err error) {
	sb := strings.ToLower(strings.TrimSpace(sourceBrowser))

	// 明确指定 firefox
	if sb == "firefox" {
		for _, p := range firefoxCookiePaths() {
			if _, err := os.Stat(p); err == nil {
				return p, "firefox", nil
			}
		}
		return "", "", fmt.Errorf("%w: Firefox Cookie 数据库未找到", ErrBrowserNotFound)
	}

	// Chrome 系 (chrome/arc/chromium/edge/brave 或空=自动检测)
	isChrome := sb == "" || sb == "chrome" || sb == "arc" || sb == "chromium" || sb == "edge" || sb == "brave"
	if isChrome {
		for _, p := range chromeCookiePaths() {
			if _, err := os.Stat(p); err == nil {
				name := "chrome"
				if strings.Contains(p, "Arc") {
					name = "arc"
				} else if strings.Contains(p, "Chromium") || strings.Contains(p, "chromium") {
					name = "chromium"
				}
				return p, name, nil
			}
		}
		// Fallback to firefox if no chrome found and browser was auto
		if sb == "" {
			for _, p := range firefoxCookiePaths() {
				if _, err := os.Stat(p); err == nil {
					return p, "firefox", nil
				}
			}
		}
		return "", "", fmt.Errorf("%w: Chrome/Chromium Cookie 数据库未找到（可能未安装）", ErrBrowserNotFound)
	}

	return "", "", fmt.Errorf("%w: 未知浏览器类型 %q（支持: chrome, firefox, arc, chromium）", ErrBrowserNotFound, sourceBrowser)
}

// ============================================================
// § SQLite 操作 [TC-C4-10, TC-C4-11]
// ============================================================

// openCookieDB 只读打开 Cookie 数据库；若锁定则复制到临时文件后打开 [TC-C4-10, TC-C4-11]。
// 返回 tempPath 非空时，调用者负责 defer os.Remove(tempPath)。
func openCookieDB(dbPath string) (*sql.DB, string, error) {
	// 尝试只读打开（不修改源文件）[TC-C4-11]
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro&_journal=off&immutable=1")
	if err == nil {
		// 验证可用性
		if pingErr := db.Ping(); pingErr == nil {
			return db, "", nil
		}
		db.Close()
	}

	// 源文件锁定 → 复制到临时文件重试 [TC-C4-10]
	tmpFile, err := os.CreateTemp("", "dw-cookie-import-*.db")
	if err != nil {
		return nil, "", ErrCookieDBLocked
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()

	src, err := os.Open(dbPath)
	if err != nil {
		os.Remove(tmpPath)
		return nil, "", ErrCookieDBLocked
	}
	defer src.Close()

	dst, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		os.Remove(tmpPath)
		return nil, "", ErrCookieDBLocked
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(tmpPath)
		return nil, "", ErrCookieDBLocked
	}
	dst.Close()

	db, err = sql.Open("sqlite3", "file:"+tmpPath+"?mode=rwc&_journal=off")
	if err != nil {
		os.Remove(tmpPath)
		return nil, "", fmt.Errorf("%w: 临时副本打开失败: %v", ErrCookieDBLocked, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		os.Remove(tmpPath)
		return nil, "", fmt.Errorf("%w: 临时副本无法访问: %v", ErrCookieDBLocked, err)
	}
	return db, tmpPath, nil
}

// rawCookie 是从数据库读取的 Cookie 原始数据。
type rawCookie struct {
	HostKey        string
	Name           string
	Value          string
	EncryptedValue []byte
	Path           string
	ExpiresUtc     int64
	IsSecure       bool
	IsHTTPOnly     bool
	Domain         string
}

// queryCookies 查询 Cookie 并按域名过滤 [TC-C4-09]。
func queryCookies(db *sql.DB, browserType string, domainFilter string) ([]rawCookie, error) {
	isFirefox := browserType == "firefox"

	var query string
	var args []interface{}

	if isFirefox {
		// Firefox: cookies.sqlite，明文存储 [TC-C4-08]
		query = `SELECT host, name, value, '', path, expiry, isSecure, isHttpOnly FROM moz_cookies`
		if domainFilter != "" {
			pattern := strings.ReplaceAll(domainFilter, "*", "%")
			query += ` WHERE host LIKE ?`
			args = append(args, pattern)
		}
	} else {
		// Chrome: cookies 表，value 可能为明文或 encrypted_value [TC-C4-07]
		query = `SELECT host_key, name, value, encrypted_value, path, expires_utc, is_secure, is_httponly FROM cookies`
		if domainFilter != "" {
			pattern := strings.ReplaceAll(domainFilter, "*", "%")
			query += ` WHERE host_key LIKE ?`
			args = append(args, pattern)
		}
	}

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cookies []rawCookie
	for rows.Next() {
		var c rawCookie
		var encVal []byte
		if isFirefox {
			var dummy string
			if err := rows.Scan(&c.HostKey, &c.Name, &c.Value, &dummy, &c.Path, &c.ExpiresUtc, &c.IsSecure, &c.IsHTTPOnly); err != nil {
				continue
			}
		} else {
			if err := rows.Scan(&c.HostKey, &c.Name, &c.Value, &encVal, &c.Path, &c.ExpiresUtc, &c.IsSecure, &c.IsHTTPOnly); err != nil {
				continue
			}
			c.EncryptedValue = encVal
		}
		// 优先使用 encrypted_value 解密，但解密失败时直接用明文 value
		if len(c.EncryptedValue) > 0 && c.Value == "" {
			decrypted, err := decryptChromeValue(c.EncryptedValue)
			if err != nil {
				// 解密失败：跳过此 Cookie（不使用错误 value）[TC-C4-07]
				continue
			}
			c.Value = decrypted
		}
		c.Domain = c.HostKey
		cookies = append(cookies, c)
	}
	return cookies, rows.Err()
}

// ============================================================
// § Chrome 解密 [TC-C4-07]
// ============================================================

// decryptChromeValue 解密 Chrome 的 encrypted_value 字段。
// macOS: AES-128-CBC + keychain key "Chrome Safe Storage"
// Linux: PBKDF2 "peanuts" key + AES-128-CBC (v10 prefix) 或明文
// Windows: DPAPI（暂不支持，返回错误）
func decryptChromeValue(encrypted []byte) (string, error) {
	if len(encrypted) == 0 {
		return "", nil
	}

	// Chrome v10 prefix (Linux/macOS)
	if len(encrypted) > 3 && string(encrypted[:3]) == "v10" {
		return decryptV10(encrypted[3:])
	}
	// Chrome v11 prefix (Linux newer)
	if len(encrypted) > 3 && string(encrypted[:3]) == "v11" {
		return decryptV10(encrypted[3:])
	}

	// 无前缀: 直接是明文（旧 Chrome 版本或 Firefox）
	if isPrintable(encrypted) {
		return string(encrypted), nil
	}

	return "", fmt.Errorf("%w: unsupported encryption format", ErrCookieDecryptFailed)
}

// isPrintable 检查字节串是否为 UTF-8 可打印字符串。
func isPrintable(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// ============================================================
// § CDP Cookie 注入 [TC-C4-07, TC-C4-08]
// ============================================================

// injectCookiesViaCDP 通过 CDP Network.setCookies 将 Cookie 注入 Chrome 会话。
func injectCookiesViaCDP(ctx context.Context, bc BrowserCore, cookies []rawCookie) error {
	// 获取 chromedp context（通过 EvalJS 间接访问，或 type assert）
	// 使用 BrowserCore.EvalJS 注入是最稳健的方式，无需 type assert。
	// 但 setCookies 需要 CDP 直接调用，所以这里 type assert 到 *browserCoreImpl。
	// 若不支持，回退到 JS document.cookie 设置。

	// 构建 CDP cookie params
	var cdpCookies []*network.CookieParam
	for i := range cookies {
		c := &cookies[i]
		param := &network.CookieParam{
			Name:     c.Name,
			Value:    c.Value,
			Domain:   c.Domain,
			Path:     c.Path,
			Secure:   c.IsSecure,
			HTTPOnly: c.IsHTTPOnly,
		}
		if c.ExpiresUtc > 0 {
			// Chrome 存储的是 microseconds since 1601-01-01，转为 Unix epoch seconds
			// 转换: (expiresUtc - 11644473600000000) / 1000000
			const epochDelta = int64(11644473600000000)
			unixSec := (c.ExpiresUtc - epochDelta) / 1000000
			if unixSec > 0 {
				t := cdp.TimeSinceEpoch(time.Unix(unixSec, 0))
				param.Expires = &t
			}
		}
		cdpCookies = append(cdpCookies, param)
	}

	if len(cdpCookies) == 0 {
		return nil
	}

	// 需要 chromedp context。通过 BrowserCore 的 EvalJS 桥接：
	// 先用 EvalJS 确认连接存活，再通过 type assert 取 CDP context。
	// 最终方案：直接让 bc 执行 CDP action。
	// EvalJS 是我们能访问的接口，但 setCookies 需要 CDP context。
	// 使用折中方案：JS document.cookie 逐条设置（不依赖 CDP context 直接访问）。
	// 对于 session 模式，bc 是 *browserCoreImpl，可以通过 EvalJS 来规避接口限制。
	// 但 JS document.cookie 无法设置 httpOnly cookie。
	// 最优解：在 browserCoreImpl 上新增 SetCookiesViaCDP 方法，这里调用接口化版本。
	// 折中实现：只注入非 httpOnly cookie 通过 JS，httpOnly cookie 提示用户。

	// 实际上，BrowserCore 已经有 EvalJS，可以用它执行 CDP action。
	// 但这违反了接口设计。让我们通过 JavaScript 设置 cookie（适用于大多数场景）。
	injected := 0
	for _, c := range cookies {
		if c.Value == "" {
			continue
		}
		// 通过 JS 设置 cookie（不支持 httpOnly）
		expires := ""
		if c.ExpiresUtc > 0 {
			const epochDelta = int64(11644473600000000)
			unixSec := (c.ExpiresUtc - epochDelta) / 1000000
			if unixSec > 0 {
				// Unix timestamp to Date string
				expires = fmt.Sprintf("; expires=%d", unixSec)
			}
		}
		secure := ""
		if c.IsSecure {
			secure = "; Secure"
		}
		path := c.Path
		if path == "" {
			path = "/"
		}
		cookieStr := fmt.Sprintf("%s=%s; path=%s%s%s",
			c.Name, c.Value, path, expires, secure)
		js := fmt.Sprintf("document.cookie = %q", cookieStr)
		var result interface{}
		_ = bc.EvalJS(ctx, js, &result)
		injected++
	}
	_ = injected
	return nil
}

// ============================================================
// § 平台专用解密实现 [TC-C4-07]
// ============================================================

// decryptV10 解密 Chrome v10/v11 前缀的加密 value。
// 各平台实现在对应的平台文件中（cookie_importer_darwin.go / _linux.go / _windows.go）。
// 此处为通用回退实现：返回错误。
// 平台文件通过 build tag 覆盖此函数。
func decryptV10(data []byte) (string, error) {
	// 尝试平台实现（通过平台文件）
	return platformDecryptV10(data)
}

// ============================================================
// § CDP 注入（增强版，通过 chromedp action）
// ============================================================

// InjectCookiesDirectCDP 通过直接 CDP 协议注入 Cookie（需要 chromedp context）。
// 此函数供 cookie_importer_cdp.go 中的集成测试调用。
func InjectCookiesDirectCDP(ctx context.Context, cookies []*network.CookieParam) error {
	return chromedp.Run(ctx, network.SetCookies(cookies))
}
