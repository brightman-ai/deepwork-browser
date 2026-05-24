package safari

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SimulatorDevice 表示一个 iOS Simulator 设备。
type SimulatorDevice struct {
	UDID           string `json:"udid"`
	Name           string `json:"name"`
	State          string `json:"state"` // "Booted" | "Shutdown"
	IsAvailable    bool   `json:"isAvailable"`
	Runtime        string `json:"-"` // runtime identifier
	RuntimeVersion string `json:"-"` // e.g. "18.0"
}

// DevicePresets 是设备预设名到 Simulator 设备类型的映射。
var DevicePresets = map[string]string{
	"iphone-se":         "iPhone SE (3rd generation)",
	"iphone-16":         "iPhone 16",
	"iphone-17":         "iPhone 17",
	"iphone-17-pro":     "iPhone 17 Pro",
	"iphone-17-pro-max": "iPhone 17 Pro Max",
	"ipad-pro":          "iPad Pro (12.9-inch) (6th generation)",
	"ipad-air":          "iPad Air (5th generation)",
}

// SimctlManager 封装 xcrun simctl 命令。
type SimctlManager struct{}

// NewSimctlManager 创建 SimctlManager。
func NewSimctlManager() *SimctlManager {
	return &SimctlManager{}
}

// execCmd 执行 simctl 子命令。
func (m *SimctlManager) execCmd(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "xcrun", append([]string{"simctl"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("simctl %s: %w\n%s", args[0], err, string(out))
	}
	return out, nil
}

// execJSON 执行 simctl 子命令并解析 JSON 输出。
func (m *SimctlManager) execJSON(ctx context.Context, result interface{}, args ...string) error {
	out, err := m.execCmd(ctx, args...)
	if err != nil {
		return err
	}
	return json.Unmarshal(out, result)
}

// ListDevices 列出所有可用设备。
func (m *SimctlManager) ListDevices(ctx context.Context) ([]SimulatorDevice, error) {
	var result struct {
		Devices map[string][]struct {
			UDID        string `json:"udid"`
			Name        string `json:"name"`
			State       string `json:"state"`
			IsAvailable bool   `json:"isAvailable"`
		} `json:"devices"`
	}
	if err := m.execJSON(ctx, &result, "list", "devices", "-j"); err != nil {
		return nil, err
	}
	var devices []SimulatorDevice
	for runtimeID, devList := range result.Devices {
		version := ""
		// 解析 runtime version: "com.apple.CoreSimulator.SimRuntime.iOS-18-0" → "18.0"
		if idx := strings.LastIndex(runtimeID, "iOS-"); idx >= 0 {
			parts := strings.Split(runtimeID[idx+4:], "-")
			if len(parts) >= 2 {
				version = parts[0] + "." + parts[1]
			}
		}
		for _, d := range devList {
			if d.IsAvailable {
				devices = append(devices, SimulatorDevice{
					UDID:           d.UDID,
					Name:           d.Name,
					State:          d.State,
					IsAvailable:    d.IsAvailable,
					Runtime:        runtimeID,
					RuntimeVersion: version,
				})
			}
		}
	}
	return devices, nil
}

// ListBooted 列出已启动的设备。
func (m *SimctlManager) ListBooted(ctx context.Context) ([]SimulatorDevice, error) {
	all, err := m.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	var booted []SimulatorDevice
	for _, d := range all {
		if d.State == "Booted" {
			booted = append(booted, d)
		}
	}
	return booted, nil
}

// ResolveDevice 根据预设名或模糊名找到设备。
// 优先级: 精确 UDID → 预设名 → 模糊名称匹配。
func (m *SimctlManager) ResolveDevice(ctx context.Context, query string) (*SimulatorDevice, error) {
	devices, err := m.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	// 精确 UDID 匹配
	for i, d := range devices {
		if d.UDID == query {
			return &devices[i], nil
		}
	}
	// 预设名匹配
	if fullName, ok := DevicePresets[strings.ToLower(query)]; ok {
		query = fullName
	}
	// 模糊名称匹配 (case-insensitive contains)
	queryLower := strings.ToLower(query)
	for i, d := range devices {
		if strings.Contains(strings.ToLower(d.Name), queryLower) {
			return &devices[i], nil
		}
	}
	return nil, fmt.Errorf("simctl: device %q not found", query)
}

// Boot 启动设备。如果已经启动则 no-op。
func (m *SimctlManager) Boot(ctx context.Context, udid string) error {
	_, err := m.execCmd(ctx, "boot", udid)
	if err != nil && strings.Contains(err.Error(), "current state: Booted") {
		return nil // 已经启动
	}
	return err
}

// Shutdown 关闭设备。
func (m *SimctlManager) Shutdown(ctx context.Context, udid string) error {
	_, err := m.execCmd(ctx, "shutdown", udid)
	if err != nil && strings.Contains(err.Error(), "current state: Shutdown") {
		return nil
	}
	return err
}

// TerminateApp terminates a running app on the simulator.
func (m *SimctlManager) TerminateApp(ctx context.Context, udid, bundleID string) error {
	_, err := m.execCmd(ctx, "terminate", udid, bundleID)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "found nothing to terminate") ||
			strings.Contains(msg, "not running") ||
			strings.Contains(msg, "no such process") {
			return nil
		}
	}
	return err
}

// OpenURL 在设备的 Safari 中打开 URL。
func (m *SimctlManager) OpenURL(ctx context.Context, udid, url string) error {
	_, err := m.execCmd(ctx, "openurl", udid, url)
	return err
}

// Screenshot 截取设备屏幕，返回 PNG 字节。
func (m *SimctlManager) Screenshot(ctx context.Context, udid string) ([]byte, error) {
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("dw-safari-%d.png", time.Now().UnixNano()))
	defer os.Remove(tmpFile)
	if _, err := m.execCmd(ctx, "io", udid, "screenshot", "--type=png", tmpFile); err != nil {
		return nil, err
	}
	return os.ReadFile(tmpFile)
}

// InputTap 在指定坐标点击。
func (m *SimctlManager) InputTap(ctx context.Context, udid string, x, y float64) error {
	_, err := m.execCmd(ctx, "io", udid, "input", "tap", fmt.Sprintf("%.0f", x), fmt.Sprintf("%.0f", y))
	return err
}

// InputText 输入文本。
func (m *SimctlManager) InputText(ctx context.Context, udid, text string) error {
	_, err := m.execCmd(ctx, "io", udid, "input", "text", text)
	return err
}

// InputSwipe 执行滑动手势。
func (m *SimctlManager) InputSwipe(ctx context.Context, udid string, x1, y1, x2, y2 float64) error {
	_, err := m.execCmd(ctx, "io", udid, "input", "swipe",
		fmt.Sprintf("%.0f", x1), fmt.Sprintf("%.0f", y1),
		fmt.Sprintf("%.0f", x2), fmt.Sprintf("%.0f", y2))
	return err
}

// OpenSafari 启动 Safari app。
func (m *SimctlManager) OpenSafari(ctx context.Context, udid string) error {
	_, err := m.execCmd(ctx, "launch", udid, "com.apple.mobilesafari")
	return err
}
