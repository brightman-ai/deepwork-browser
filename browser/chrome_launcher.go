package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// cdpBasePort 起始端口 (大质数，远离常用端口)。
const cdpBasePort = 25137

// cdpPortOffsets 质数偏移序列，最多 10 个候选端口。
var cdpPortOffsets = []int{0, 1, 2, 3, 5, 7, 11, 13, 17, 19}

// findAvailableCDPPort 用确定性质数序列找一个可用端口。
// 启动和连接都用同一算法，消除端口发现问题。
func findAvailableCDPPort() (int, error) {
	for _, offset := range cdpPortOffsets {
		port := cdpBasePort + offset
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			ln.Close()
			return port, nil
		}
	}
	return 0, fmt.Errorf("no available CDP port in range %d-%d", cdpBasePort, cdpBasePort+cdpPortOffsets[len(cdpPortOffsets)-1])
}

// ============================================================
// § ChromeLauncher [Ref: CAP-BS09-C1, T5-B8]
// ============================================================

// chromeLauncherImpl implements ChromeLauncher.
type chromeLauncherImpl struct{}

// NewChromeLauncher 返回默认 ChromeLauncher 实现。
func NewChromeLauncher() *chromeLauncherImpl {
	return &chromeLauncherImpl{}
}

// linuxChromePaths Linux 平台检测顺序（优先级从高到低）[Ref: BP §A2, T5-B8.1]。
// [TH-0414-b3m] snap wrapper 优先 — snap 沙箱内 Chrome 的 TLS/DNS 行为与桌面一致。
// 直接二进制绕过沙箱会导致 Cloudflare 检测差异。
// 单实例冲突通过独立 --user-data-dir 解决（不同 profile = 不同实例）。
var linuxChromePaths = []string{
	"/usr/bin/google-chrome-stable",
	"/usr/bin/google-chrome",
	"/usr/bin/chromium-browser",
	"/snap/bin/chromium",
}

// macOSChromePaths macOS 平台检测顺序。
var macOSChromePaths = []string{
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	"/Applications/Chromium.app/Contents/MacOS/Chromium",
}

// windowsChromePaths Windows 平台检测顺序。
var windowsChromePaths = []string{
	`C:\Program Files\Google\Chrome\Application\chrome.exe`,
	`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
}

// FindChrome 返回本地 Chrome/Edge/Chromium 可执行文件路径。
// 未找到返回 ErrBrowserNotFound。无浏览器不下载 Chromium [IR-02, BP §A2]。
func (l *chromeLauncherImpl) FindChrome() (string, error) {
	var candidates []string
	switch runtime.GOOS {
	case "linux":
		candidates = linuxChromePaths
	case "darwin":
		candidates = macOSChromePaths
	case "windows":
		candidates = windowsChromePaths
	default:
		candidates = linuxChromePaths
	}

	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	// 也尝试 PATH 中的 chromium/chrome
	for _, name := range []string{"google-chrome-stable", "google-chrome", "chromium-browser", "chromium", "chrome"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}

	return "", ErrBrowserNotFound
}

// DetachedChromeLaunchOptions 描述 detached Chrome 进程的启动参数。
// 用于需要独立 Chrome 生命周期的场景（如 dw-browser open/session）。
type DetachedChromeLaunchOptions struct {
	DebugPort  int
	ProfileDir string
	Width      int
	Height     int
	PresetID   string
	UserAgent  string
	Touch      bool
	Mode       BrowserMode
}

// BuildDetachedChromeArgs 生成 detached Chrome 的启动参数。
// 设计目标:
//   - headless/headed/visible 三模式共用同一组生命周期必需参数
//   - visible 保留本机 Chrome 的真实指纹面，不叠加自动化/CI flags
//   - headed 使用真实 Chrome + 虚拟显示，并补最小反后台节流参数保证 LiveView 切 tab 稳定
//   - headless 只在必要处补偿 UA/webdriver，不关闭 GPU/WebGL
func BuildDetachedChromeArgs(opts DetachedChromeLaunchOptions) []string {
	width, height := opts.Width, opts.Height
	if width <= 0 {
		width = DefaultViewportWidth
	}
	if height <= 0 {
		height = DefaultViewportHeight
	}
	mode := NormalizeBrowserMode(opts.Mode, ModeHeaded)

	effectiveUA := opts.UserAgent
	if effectiveUA == "" && mode == ModeHeadless {
		preset := ResolveRuntimeFingerprintPreset(opts.PresetID, "")
		if preset == nil {
			preset = BuiltinPresets[NormalizePresetID(opts.PresetID)]
		}
		if preset != nil {
			// headless 的 /json/version 与网络层 UA 会暴露 HeadlessChrome。
			// 只在 headless 显式覆写；headed/visible 走本机 Chrome 原生 UA。
			effectiveUA = preset.UserAgent
		}
	}

	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", opts.DebugPort),
		"--remote-debugging-address=127.0.0.1",
		"--user-data-dir=" + opts.ProfileDir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-session-crashed-bubble",
		"--hide-crash-restore-bubble",
		"--force-color-profile=srgb",
		fmt.Sprintf("--window-size=%d,%d", width, height),
	}

	if mode == ModeHeadless {
		args = append(args,
			"--headless=new",
			"--disable-blink-features=AutomationControlled",
			"--disable-background-networking",
			"--disable-background-timer-throttling",
			"--disable-backgrounding-occluded-windows",
			"--disable-breakpad",
			"--disable-client-side-phishing-detection",
			"--disable-default-apps",
			"--disable-dev-shm-usage",
			"--disable-hang-monitor",
			"--disable-ipc-flooding-protection",
			"--disable-popup-blocking",
			"--disable-prompt-on-repost",
			"--disable-renderer-backgrounding",
			"--disable-sync",
			"--metrics-recording-only",
			"--safebrowsing-disable-auto-update",
			"--password-store=basic",
			"--use-mock-keychain",
		)
	} else {
		if mode == ModeHeaded {
			args = append(args,
				"--disable-background-timer-throttling",
				"--disable-backgrounding-occluded-windows",
				"--disable-renderer-backgrounding",
			)
		}
		switch runtime.GOOS {
		case "linux":
			args = append(args, "--use-gl=egl")
		case "darwin":
			args = append(args, "--use-angle=metal")
			if mode == ModeVisible {
				// 仅 visible 模式由 Workspace 绑定独立 Space。headed 模式使用
				// CGVirtualDisplay，不能 fullscreen，否则窗口可能脱离虚拟屏。
				args = append(args, "--start-fullscreen")
			}
		case "windows":
			args = append(args, "--use-angle=d3d11")
		}
	}

	if effectiveUA != "" {
		args = append(args, "--user-agent="+effectiveUA)
	}
	if opts.Touch {
		args = append(args, "--touch-events=enabled")
	}

	return append(args, ChromeInitialPageURL)
}

func appendChromeArgBeforeURL(args []string, arg string) []string {
	if len(args) == 0 {
		return []string{arg}
	}
	last := args[len(args)-1]
	if strings.HasPrefix(last, "--") {
		return append(args, arg)
	}
	out := make([]string, 0, len(args)+1)
	out = append(out, args[:len(args)-1]...)
	out = append(out, arg, last)
	return out
}

// ExecAllocatorOptionsFromArgs 将 detached Chrome 参数转换为 chromedp ExecAllocatorOption。
// 设计目标:
//   - BrowserPool 与 detached dw-browser 复用同一套跨平台 launch args
//   - 避免两条路径各维护一份 flags，导致 Cloudflare / Turnstile 行为分叉
func ExecAllocatorOptionsFromArgs(chromePath string, args []string) []chromedp.ExecAllocatorOption {
	opts := []chromedp.ExecAllocatorOption{
		chromedp.ExecPath(chromePath),
	}

	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			continue
		}

		switch arg {
		case "--no-first-run":
			opts = append(opts, chromedp.NoFirstRun)
			continue
		case "--no-default-browser-check":
			opts = append(opts, chromedp.NoDefaultBrowserCheck)
			continue
		}

		if value, ok := strings.CutPrefix(arg, "--user-agent="); ok {
			opts = append(opts, chromedp.UserAgent(value))
			continue
		}
		if value, ok := strings.CutPrefix(arg, "--window-size="); ok {
			parts := strings.SplitN(value, ",", 2)
			if len(parts) == 2 {
				if width, errW := strconv.Atoi(parts[0]); errW == nil {
					if height, errH := strconv.Atoi(parts[1]); errH == nil {
						opts = append(opts, chromedp.WindowSize(width, height))
						continue
					}
				}
			}
		}

		nameValue := strings.TrimPrefix(arg, "--")
		if name, value, ok := strings.Cut(nameValue, "="); ok {
			opts = append(opts, chromedp.Flag(name, value))
			continue
		}
		opts = append(opts, chromedp.Flag(nameValue, true))
	}

	return opts
}

// Launch 检测并启动 Chrome，返回 CDP WebSocket URL 和进程 PID。
// profileID 对应 ~/.deepwork/browser-data/{profileID}/ 目录。
func (l *chromeLauncherImpl) Launch(ctx context.Context, profileID string, headless ...bool) (cdpURL string, pid int, err error) {
	chromePath, err := l.FindChrome()
	if err != nil {
		return "", 0, err
	}

	// 构建 Profile 目录
	// Snap chromium 对 ~/. 路径有权限限制，使用 XDG 缓存或 /tmp
	cacheDir := os.Getenv("XDG_CACHE_HOME")
	if cacheDir == "" {
		cacheDir = "/tmp"
	}
	profilePath := fmt.Sprintf("%s/dw-browser-data/%s", cacheDir, profileID)
	if err := os.MkdirAll(profilePath, 0755); err != nil {
		return "", 0, fmt.Errorf("browser: create profile dir: %w", err)
	}

	// 确定性端口分配: 从 25137 起，质数偏移，最多 10 个候选
	cdpPort, err := findAvailableCDPPort()
	if err != nil {
		return "", 0, fmt.Errorf("%w: %v", ErrBrowserNotFound, err)
	}

	// Chrome 启动参数 [Ref: BP §A2, T5-B8.2]
	useHeadless := true
	if len(headless) > 0 {
		useHeadless = headless[0]
	}
	mode := ModeVisible
	if useHeadless {
		mode = ModeHeadless
	}
	args := BuildDetachedChromeArgs(DetachedChromeLaunchOptions{
		DebugPort:  cdpPort,
		ProfileDir: profilePath,
		Width:      DefaultViewportWidth,
		Height:     DefaultViewportHeight,
		Mode:       mode,
	})

	cmd := exec.CommandContext(ctx, chromePath, args...)
	// 捕获 stderr 用于调试 Chrome 启动失败
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		return "", 0, fmt.Errorf("%w: %v", ErrBrowserNotFound, err)
	}

	procPID := cmd.Process.Pid

	// 等待 Chrome CDP 就绪 (端口已知，只需轮询连通性)
	// snap chromium 启动较慢 (~3-5s)，给足等待时间
	cdpURL, err = waitForCDP(ctx, cdpPort, ChromeCDPStartupAttempts, ChromeCDPStartupPollInterval)
	if err != nil {
		// 超时则 Kill 残留进程
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		stderr := stderrBuf.String()
		if len(stderr) > 200 {
			stderr = stderr[:200]
		}
		return "", 0, fmt.Errorf("%w: CDP polling failed: %v (chrome stderr: %s)", ErrBrowserNotFound, err, stderr)
	}

	return cdpURL, procPID, nil
}

// pollCDPVersion 轮询 /json/version 获取 CDP DevTools URL。
// waitForCDP 在已知端口上轮询 /json/version 等待 Chrome CDP 就绪。
func waitForCDP(ctx context.Context, port int, maxAttempts int, interval time.Duration) (string, error) {
	startedAt := time.Now()
	versionURL := fmt.Sprintf("http://127.0.0.1:%d/json/version", port)

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			log.Printf("[CHROME-LAUNCH] legacy_cdp_cancelled port=%d attempts=%d elapsed_ms=%d err=%v",
				port, attempt, time.Since(startedAt).Milliseconds(), ctx.Err())
			return "", ctx.Err()
		case <-time.After(interval):
		}

		resp, err := http.Get(versionURL)
		if err != nil {
			lastErr = err
			continue
		}
		var vInfo struct {
			WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		}
		decErr := json.NewDecoder(resp.Body).Decode(&vInfo)
		resp.Body.Close()
		if decErr == nil && vInfo.WebSocketDebuggerURL != "" {
			log.Printf("[CHROME-LAUNCH] legacy_cdp_ready port=%d attempts=%d elapsed_ms=%d",
				port, attempt+1, time.Since(startedAt).Milliseconds())
			return vInfo.WebSocketDebuggerURL, nil
		}
		if decErr != nil {
			lastErr = decErr
		}
	}
	log.Printf("[CHROME-LAUNCH] legacy_cdp_timeout port=%d attempts=%d elapsed_ms=%d last_err=%v",
		port, maxAttempts, time.Since(startedAt).Milliseconds(), lastErr)
	return "", fmt.Errorf("CDP not ready on port %d after %d attempts", port, maxAttempts)
}

// findCDPPortByPID 通过读取 /proc/{pid}/net/tcp6 找到 Chrome 监听的 CDP 端口。
func findCDPPortByPID(pid int) (int, error) {
	// Linux: 读取 /proc/{pid}/net/tcp 文件找 LISTEN 状态的本地端口
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/tcp6", pid))
	if err != nil {
		data, err = os.ReadFile(fmt.Sprintf("/proc/%d/net/tcp", pid))
		if err != nil {
			return 0, err
		}
	}

	// 解析 tcp/tcp6 文件: 每行格式 "  sl  local_address:port rem_addr:port st ..."
	// 找 state=0A (LISTEN) 的本地端口
	lines := splitLines(string(data))
	for _, line := range lines[1:] { // 跳过 header
		fields := splitFields(line)
		if len(fields) < 4 {
			continue
		}
		if fields[3] == "0A" { // LISTEN
			// local_address 格式: "00000000:1A2B"（IPv4）或 "00000000000000000000000000000000:1A2B"（IPv6）
			localAddr := fields[1]
			colonIdx := -1
			for i := len(localAddr) - 1; i >= 0; i-- {
				if localAddr[i] == ':' {
					colonIdx = i
					break
				}
			}
			if colonIdx < 0 {
				continue
			}
			portHex := localAddr[colonIdx+1:]
			var port int
			if n, err := fmt.Sscanf(portHex, "%X", &port); n == 1 && err == nil {
				// CDP 调试端口通常在 1024-65535 范围内，且不是常见端口
				if port > 1024 && port < 65535 {
					return port, nil
				}
			}
		}
	}
	return 0, fmt.Errorf("no LISTEN port found for pid %d", pid)
}

// splitLines 分割字符串为行。
func splitLines(s string) []string {
	var lines []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// splitFields 按空白分割字段。
func splitFields(s string) []string {
	var fields []string
	inField := false
	start := 0
	for i, c := range s {
		if c == ' ' || c == '\t' {
			if inField {
				fields = append(fields, s[start:i])
				inField = false
			}
		} else {
			if !inField {
				start = i
				inField = true
			}
		}
	}
	if inField {
		fields = append(fields, s[start:])
	}
	return fields
}

// Kill 杀死指定 PID 的 Chrome 进程。
func (l *chromeLauncherImpl) Kill(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}

// IsAlive 检查 Chrome 进程是否存活。
func (l *chromeLauncherImpl) IsAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// 发送信号 0 检查进程是否存在（不影响进程）
	err = proc.Signal(os.Interrupt)
	// 在 Linux 上，如果进程不存在会返回 "os: process already finished" 或 ESRCH
	if err != nil {
		return false
	}
	return true
}

// cdpVersionResponse is the JSON response from /json/version.
type cdpVersionResponse struct {
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	Browser              string `json:"Browser"`
}

// GetCDPVersion 查询 CDP /json/version 返回版本信息。
func GetCDPVersion(port int) (*cdpVersionResponse, error) {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var v cdpVersionResponse
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, err
	}
	return &v, nil
}
