// Package browser — DisplayManager: 虚拟 display 生命周期管理 (跨平台)。
// 从 BrowserPool 提取，供 Pool 和 dw-browser CLI (NewBrowserCore) 共享。
package browser

import (
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// DisplayManager 管理独立虚拟 display 的生命周期 (跨平台)。
// Pool 和 NewBrowserCore 共享此组件。goroutine-safe。
type DisplayManager struct {
	mu sync.Mutex
	xvfbCmd *exec.Cmd
	display string // e.g. ":99"
	ready bool
}

// EnsureDisplay 确保虚拟 display 就绪 (幂等,可多次调用)。
// 仅在 human 模式下有意义；headless 模式调用为 no-op。
// 返回 true 表示 display 已就绪，false 表示应降级为 headless。
func (dm *DisplayManager) EnsureDisplay bool {
	dm.mu.Lock
	defer dm.mu.Unlock

	if dm.ready {
		return true
	}

	switch runtime.GOOS {
	case "linux":
		return dm.ensureDisplayLinux
	case "darwin":
		// macOS: Space 隔离 + Metal GPU .
		// 窗口放入独立 Space，Human 不被打扰，Chrome 内 visibilityState=visible.
		// Workspace 生命周期由调用方 (BrowserPool/NewBrowserCore) 管理.
		log.Printf("[DISPLAY-MGR] macOS: Space isolation + Metal GPU (Phase 0)")
		dm.ready = true
		return true
	case "windows":
		// Windows: DWM + ANGLE D3D11 真实 GPU (Space 隔离 stub, Phase 0 TODO).
		log.Printf("[DISPLAY-MGR] Windows: ANGLE D3D11 GPU (VDesktop isolation TODO)")
		dm.ready = true
		return true
	default:
		log.Printf("[DISPLAY-MGR] WARNING: unsupported OS %s — should fall back to headless", runtime.GOOS)
		return false
	}
}

// Close 清理 Xvfb 进程。实现 io.Closer 语义。
func (dm *DisplayManager) Close error {
	dm.mu.Lock
	defer dm.mu.Unlock

	if dm.xvfbCmd != nil && dm.xvfbCmd.Process != nil {
		_ = dm.xvfbCmd.Process.Kill
		_ = dm.xvfbCmd.Wait
		log.Printf("[DISPLAY-MGR] Xvfb process killed")
		dm.xvfbCmd = nil
	}
	dm.ready = false
	return nil
}

// Display 返回当前 display 字符串 (e.g. ":99")。空字符串表示未启动。
func (dm *DisplayManager) Display string {
	dm.mu.Lock
	defer dm.mu.Unlock
	return dm.display
}

// XvfbPID 返回 Xvfb 进程 PID。0 表示未启动或非 Linux。
func (dm *DisplayManager) XvfbPID int {
	dm.mu.Lock
	defer dm.mu.Unlock
	if dm.xvfbCmd != nil && dm.xvfbCmd.Process != nil {
		return dm.xvfbCmd.Process.Pid
	}
	return 0
}

// UsesHeadlessHumanMode 返回当前平台的 human 模式是否底层使用 headless=new。
// 当前终局策略:
// - Linux: Xvfb + EGL (headed)
// - macOS: offscreen window + Metal (headed)
// - Windows: offscreen window + D3D11 (headed)
//
// 调用者在此返回 true 时应:
// 1. 显式覆写 UA（移除 HeadlessChrome 签名）
// 2. 注入完整 stealth 脚本（与 headless 模式一致）
func (dm *DisplayManager) UsesHeadlessHumanMode bool {
	return false
}

// ChromeGLOpts 返回平台专用 GPU/display 的 chromedp ExecAllocatorOption。
// human 模式下，各平台需要不同的 GL 渲染参数。
func (dm *DisplayManager) ChromeGLOpts chromedp.ExecAllocatorOption {
	switch runtime.GOOS {
	case "linux":
		// Linux Xvfb+EGL: EGL 绕过 GLX，直连 /dev/dri/renderD128
		return chromedp.ExecAllocatorOption{
			chromedp.Flag("use-gl", "egl")
		}
	case "darwin":
		// macOS: Metal GPU only — window-position removed (Phase 0).
		// 隔离由 Workspace (SkyLight Space) 实现，不再通过 off-screen 坐标.
		// [: --window-position=-32000 是双向失败; ]
		return chromedp.ExecAllocatorOption{
			chromedp.Flag("use-angle", "metal")
		}
	case "windows":
		// Windows: D3D11 GPU only — window-position removed (Phase 0).
		// TODO: VDesktop isolation (stub, window appears on current desktop).
		return chromedp.ExecAllocatorOption{
			chromedp.Flag("use-angle", "d3d11")
		}
	default:
		return nil
	}
}

// ensureDisplayLinux 启动独立 Xvfb 虚拟 display (Linux 专用)。
// 必须在 mu.Lock 内调用。
func (dm *DisplayManager) ensureDisplayLinux bool {
	// snap GPU 环境注入 — EGL 需要 LIBGL_DRIVERS_PATH 等
	injectSnapEnvIfNeeded

	// 清除 Wayland 环境 — 强制 Chrome 使用 X11 (在 Xvfb 上)
	os.Unsetenv("WAYLAND_DISPLAY")

	display := ":99"
	xvfbPath, err := exec.LookPath("Xvfb")
	if err != nil {
		log.Printf("[DISPLAY-MGR] WARNING: Xvfb not found — should fall back to headless")
		return false
	}

	// 检查 :99 是否已被占用
	socketPath := "/tmp/.X11-unix/X99"
	if _, err := os.Stat(socketPath); err == nil {
		// socket 文件存在 — 验证 X server 是否真的可连接 (防孤儿 socket)
		if dm.xvfbCmd != nil && dm.xvfbCmd.Process != nil {
			// 我们自己启动的 Xvfb
			os.Setenv("DISPLAY", display)
			dm.display = display
			dm.ready = true
			log.Printf("[DISPLAY-MGR] Xvfb+EGL: reusing own Xvfb on %s", display)
			return true
		}
		// 外部 Xvfb: 用 xdpyinfo 验证是否可连接
		checkCmd := exec.Command("xdpyinfo", "-display", display)
		checkCmd.Env = append(os.Environ, "DISPLAY="+display)
		if err := checkCmd.Run; err == nil {
			os.Setenv("DISPLAY", display)
			dm.display = display
			dm.ready = true
			log.Printf("[DISPLAY-MGR] Xvfb+EGL: reusing existing Xvfb on %s (verified)", display)
			return true
		}
		// X server 不可连接 → 孤儿 socket,清理后重建
		log.Printf("[DISPLAY-MGR] orphan socket %s (Xvfb dead), cleaning up", socketPath)
		os.Remove(socketPath)
		os.Remove("/tmp/.X99-lock")
	}

	cmd := exec.Command(xvfbPath, display, "-screen", "0", "1920x1080x24"
		"-nolisten", "tcp"
		"+extension", "GLX"
		"+extension", "RANDR"
	)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start; err != nil {
		log.Printf("[DISPLAY-MGR] WARNING: Xvfb start failed: %v — should fall back to headless", err)
		return false
	}
	dm.xvfbCmd = cmd

	for i := 0; i < 30; i++ {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	os.Setenv("DISPLAY", display)
	dm.display = display
	dm.ready = true
	log.Printf("[DISPLAY-MGR] Xvfb+EGL: started Xvfb on %s (pid=%d), GPU via EGL /dev/dri/renderD128", display, cmd.Process.Pid)
	return true
}

// injectSnapEnvIfNeeded 检测 snap Chromium 并注入其运行时 GPU 环境变量 。
func injectSnapEnvIfNeeded {
	if _, err := os.Stat("/snap/bin/chromium"); err != nil {
		return
	}
	if os.Getenv("LIBGL_DRIVERS_PATH") != "" {
		return
	}

	log.Printf("[DISPLAY-MGR] snap Chromium detected, injecting GPU runtime environment")

	cmd := exec.Command("snap", "run", "--shell", "chromium")
	cmd.Stdin = strings.NewReader("env\n")
	out, err := cmd.Output
	if err != nil {
		log.Printf("[DISPLAY-MGR] WARNING: snap run --shell failed: %v", err)
		return
	}

	gpuKeys := map[string]bool{
		"LIBGL_DRIVERS_PATH": true
		"GBM_BACKENDS_PATH": true
		"__EGL_VENDOR_LIBRARY_DIRS": true
		"__EGL_EXTERNAL_PLATFORM_CONFIG_DIRS": true
		"DRIRC_CONFIGDIR": true
		"LIBVA_DRIVERS_PATH": true
		"VK_LAYER_PATH": true
		"LD_LIBRARY_PATH": true
		"GDK_BACKEND": true
		"CLUTTER_BACKEND": true
	}

	injected := 0
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := parts[0]
		if gpuKeys[key] {
			os.Setenv(key, parts[1])
			injected++
		}
	}
	log.Printf("[DISPLAY-MGR] injected %d snap GPU environment variables", injected)
}

// findXAuthFromProcess 从 Xwayland/Xorg 进程的 -auth 参数中提取 XAUTHORITY 路径。
func findXAuthFromProcess(display string) string {
	displayNum := display[1:] // ":0" → "0"
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		if !entry.IsDir {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + entry.Name + "/cmdline")
		if err != nil {
			continue
		}
		args := strings.Split(string(cmdline), "\x00")
		isX := false
		for _, a := range args {
			if strings.Contains(a, "Xwayland") || strings.Contains(a, "Xorg") {
				isX = true
				break
			}
		}
		if !isX {
			continue
		}
		matchDisplay := false
		for _, a := range args {
			if a == ":"+displayNum {
				matchDisplay = true
				break
			}
		}
		if !matchDisplay {
			continue
		}
		for i, a := range args {
			if a == "-auth" && i+1 < len(args) {
				return args[i+1]
			}
		}
	}
	return ""
}
