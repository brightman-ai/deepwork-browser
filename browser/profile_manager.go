package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// ============================================================
// § ProfileManager [Ref: CAP-BS09-C4, T5-B7]
// ============================================================

// Profile 是 Browser Profile 实体 [Ref: T5-B2.1]。
type Profile struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	UserDataDir string    `json:"user_data_dir"`
	Status      string    `json:"status"` // "active" | "crashed" | "inactive"
	CreatedAt   time.Time `json:"created_at"`
}

// profileMetadata 是元数据文件结构 [Ref: BP §A4]。
type profileMetadata struct {
	Profiles []*Profile `json:"profiles"`
}

// ProfileManager 管理 Browser Profile 生命周期（元数据用 JSON 文件，不用 SQLite）[Ref: BP §B3]。
type ProfileManager struct {
	mu       sync.RWMutex
	baseDir  string // ~/.deepwork/browser-data/
	metaPath string // ~/.deepwork/browser-profiles.json
}

const defaultLogicalProfileID = "default"

// 系统级 Browser Profile 常量 — 三域隔离模型。
// Human 浏览（Browser Portal）使用单独的 browser-sidebar-main，由各路由自行维护。
const (
	// BrowserProfileWorkspace 供所有 Workspace AI session 共用（持久，共享站点登录态）。
	BrowserProfileWorkspace = "workspace"
	// BrowserProfileWebChat 供 Council / WebChat AI 使用（持久，保持 Gemini/ChatGPT 等登录）。
	BrowserProfileWebChat = "webchat"
)

// NewProfileManager 创建 ProfileManager 实例。
func NewProfileManager() (*ProfileManager, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("browser: get home dir: %w", err)
	}
	return NewProfileManagerForDataDir(filepath.Join(homeDir, ".deepwork"))
}

// NewProfileManagerForDataDir 基于指定 deepwork dataDir 创建 ProfileManager。
func NewProfileManagerForDataDir(dataDir string) (*ProfileManager, error) {
	baseDir := filepath.Join(dataDir, "browser-data", "profiles")
	metaPath := filepath.Join(dataDir, "browser-profiles.json")

	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("browser: create base dir: %w", err)
	}

	return &ProfileManager{
		baseDir:  baseDir,
		metaPath: metaPath,
	}, nil
}

// NewProfileManagerWithBase 创建指定 baseDir 的 ProfileManager（测试用）。
func NewProfileManagerWithBase(baseDir string) (*ProfileManager, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("browser: create base dir: %w", err)
	}
	metaPath := filepath.Join(baseDir, "browser-profiles.json")
	return &ProfileManager{
		baseDir:  baseDir,
		metaPath: metaPath,
	}, nil
}

// DefaultProfileID 返回逻辑默认 profile ID。
func DefaultProfileID() string {
	return defaultLogicalProfileID
}

// NormalizeProfileID 规范化逻辑 profile ID。
// 允许用户直接输入显示名，最终转换为稳定的 ASCII slug。
func NormalizeProfileID(id string) string {
	id = strings.TrimSpace(strings.ToLower(id))
	if id == "" {
		return defaultLogicalProfileID
	}

	var b strings.Builder
	lastDash := false
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
			lastDash = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == ' ':
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return defaultLogicalProfileID
	}
	return out
}

// GetOrCreate 获取或创建 Profile，确保目录存在 [TC-09-U-12, TC-09-U-13]。
func (m *ProfileManager) GetOrCreate(id string) (*Profile, error) {
	id = NormalizeProfileID(id)
	m.mu.Lock()
	defer m.mu.Unlock()

	meta, err := m.loadMeta()
	if err != nil {
		meta = &profileMetadata{}
	}

	// 查找已有 Profile [TC-09-U-13]
	for _, p := range meta.Profiles {
		if p.ID == id {
			// 已有 Profile，确认目录存在
			if _, err := os.Stat(p.UserDataDir); err == nil {
				return p, nil
			}
			// 目录不存在，重建
			if err := os.MkdirAll(p.UserDataDir, 0755); err != nil {
				return nil, fmt.Errorf("browser: recreate profile dir: %w", err)
			}
			return p, nil
		}
	}

	// 首次创建 [TC-09-U-12]
	userDataDir := filepath.Join(m.baseDir, id)
	if err := os.MkdirAll(userDataDir, 0755); err != nil {
		return nil, fmt.Errorf("browser: create profile dir: %w", err)
	}

	name := id
	if id == defaultLogicalProfileID {
		name = "Default"
	}
	profile := &Profile{
		ID:          id,
		Name:        name,
		UserDataDir: userDataDir,
		Status:      "active",
		CreatedAt:   time.Now(),
	}
	meta.Profiles = append(meta.Profiles, profile)

	if err := m.saveMeta(meta); err != nil {
		return nil, err
	}

	return profile, nil
}

// EnsureDefault 确保默认 profile 存在。
func (m *ProfileManager) EnsureDefault() (*Profile, error) {
	return m.GetOrCreate(DefaultProfileID())
}

// Create 创建新的逻辑 profile。
// 若同名 slug 已存在，则自动追加数值后缀，避免覆盖现有用户状态。
func (m *ProfileManager) Create(name string) (*Profile, error) {
	baseID := NormalizeProfileID(name)
	displayName := strings.TrimSpace(name)
	if displayName == "" {
		displayName = baseID
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	meta, err := m.loadMeta()
	if err != nil {
		meta = &profileMetadata{}
	}

	existing := make(map[string]bool, len(meta.Profiles))
	for _, p := range meta.Profiles {
		existing[p.ID] = true
	}

	id := baseID
	for n := 2; existing[id]; n++ {
		id = fmt.Sprintf("%s-%d", baseID, n)
	}

	userDataDir := filepath.Join(m.baseDir, id)
	if err := os.MkdirAll(userDataDir, 0755); err != nil {
		return nil, fmt.Errorf("browser: create profile dir: %w", err)
	}

	profile := &Profile{
		ID:          id,
		Name:        displayName,
		UserDataDir: userDataDir,
		Status:      "inactive",
		CreatedAt:   time.Now(),
	}
	meta.Profiles = append(meta.Profiles, profile)

	if err := m.saveMeta(meta); err != nil {
		return nil, err
	}
	return profile, nil
}

// Repair 修复损坏的 Profile（备份旧目录 + 重建）[TC-09-U-14]。
func (m *ProfileManager) Repair(id string) (*Profile, error) {
	id = NormalizeProfileID(id)
	m.mu.Lock()
	defer m.mu.Unlock()

	meta, err := m.loadMeta()
	if err != nil {
		meta = &profileMetadata{}
	}

	userDataDir := filepath.Join(m.baseDir, id)

	// 备份旧目录 [TC-09-U-14]
	if _, err := os.Stat(userDataDir); err == nil {
		backupDir := fmt.Sprintf("%s.bak.%d", userDataDir, time.Now().Unix())
		if err := os.Rename(userDataDir, backupDir); err != nil {
			return nil, fmt.Errorf("browser: backup profile: %w", err)
		}
	}

	// 重建目录
	if err := os.MkdirAll(userDataDir, 0755); err != nil {
		return nil, fmt.Errorf("browser: rebuild profile dir: %w", err)
	}

	// 更新或创建 Profile 记录
	var found *Profile
	for _, p := range meta.Profiles {
		if p.ID == id {
			p.Status = "active"
			p.UserDataDir = userDataDir
			found = p
			break
		}
	}
	if found == nil {
		found = &Profile{
			ID: id,
			Name: func() string {
				if id == defaultLogicalProfileID {
					return "Default"
				}
				return id
			}(),
			UserDataDir: userDataDir,
			Status:      "active",
			CreatedAt:   time.Now(),
		}
		meta.Profiles = append(meta.Profiles, found)
	}

	if err := m.saveMeta(meta); err != nil {
		return nil, err
	}

	return found, nil
}

// List 返回所有 Profile [TC-09-I-14]。
func (m *ProfileManager) List() ([]*Profile, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	meta, err := m.loadMeta()
	if err != nil {
		return nil, err
	}
	return meta.Profiles, nil
}

// Delete 删除 Profile（元数据 + user-data-dir）[TC-09-I-14]。
func (m *ProfileManager) Delete(id string) error {
	id = NormalizeProfileID(id)
	m.mu.Lock()
	defer m.mu.Unlock()

	meta, err := m.loadMeta()
	if err != nil {
		return err
	}

	var remaining []*Profile
	var deleted *Profile
	for _, p := range meta.Profiles {
		if p.ID == id {
			deleted = p
		} else {
			remaining = append(remaining, p)
		}
	}

	if deleted == nil {
		return fmt.Errorf("browser: profile %q not found", id)
	}

	// 删除 user-data-dir
	if err := os.RemoveAll(deleted.UserDataDir); err != nil {
		return fmt.Errorf("browser: delete profile dir: %w", err)
	}

	meta.Profiles = remaining
	return m.saveMeta(meta)
}

// InjectStealth 注入 Stealth 脚本（webdriver 伪装）[TC-09-U-15]。
func (m *ProfileManager) InjectStealth(ctx context.Context) error {
	return chromedp.Run(ctx, chromedp.Evaluate(stealthScript, nil))
}

// stealthScript webdriver 伪装脚本 [TC-09-U-15]。
// 包含 navigator.webdriver + chrome.runtime 模拟。
const stealthScript = `
(function() {
    // === navigator.webdriver 伪装 ===
    Object.defineProperty(navigator, 'webdriver', {
        get: () => undefined,
        configurable: true,
    });

    // === chrome.runtime 模拟 ===
    if (!window.chrome) window.chrome = {};
    if (!window.chrome.runtime) {
        window.chrome.runtime = {
            id: undefined,
            connect: function() { return { onMessage: { addListener: function() {} }, postMessage: function() {} }; },
            sendMessage: function() {},
            onConnect: { addListener: function() {} },
            onMessage: { addListener: function() {} },
        };
    }

    // === plugins 伪装 (模拟真实 PluginArray) ===
    Object.defineProperty(navigator, 'plugins', {
        get: () => {
            var p = {
                0: {type: 'application/x-google-chrome-pdf', suffixes: 'pdf', description: 'Portable Document Format', name: 'Chrome PDF Plugin'},
                1: {type: 'application/pdf', suffixes: 'pdf', description: '', name: 'Chrome PDF Viewer'},
                length: 2,
                item: function(i) { return this[i]; },
                namedItem: function(n) { for(var i=0;i<this.length;i++) if(this[i].name===n) return this[i]; return null; },
                refresh: function() {},
            };
            return p;
        },
        configurable: true,
    });

    // === languages ===
    Object.defineProperty(navigator, 'languages', {
        get: () => ['zh-CN', 'zh', 'en-US', 'en'],
        configurable: true,
    });

    // === platform ===
    Object.defineProperty(navigator, 'platform', {
        get: () => 'Linux x86_64',
        configurable: true,
    });

    // === WebGL vendor/renderer 伪装 (关键 — SwiftShader 是最大暴露点) ===
    var getParameter = WebGLRenderingContext.prototype.getParameter;
    WebGLRenderingContext.prototype.getParameter = function(param) {
        if (param === 37445) return 'Google Inc. (NVIDIA)';           // UNMASKED_VENDOR_WEBGL
        if (param === 37446) return 'ANGLE (NVIDIA, NVIDIA GeForce GTX 1060 6GB Direct3D11 vs_5_0 ps_5_0, D3D11)'; // UNMASKED_RENDERER_WEBGL
        return getParameter.call(this, param);
    };
    var getParameter2 = WebGL2RenderingContext.prototype.getParameter;
    WebGL2RenderingContext.prototype.getParameter = function(param) {
        if (param === 37445) return 'Google Inc. (NVIDIA)';
        if (param === 37446) return 'ANGLE (NVIDIA, NVIDIA GeForce GTX 1060 6GB Direct3D11 vs_5_0 ps_5_0, D3D11)';
        return getParameter2.call(this, param);
    };

    // === Permissions API 伪装 ===
    if (navigator.permissions) {
        var origQuery = navigator.permissions.query;
        navigator.permissions.query = function(params) {
            if (params.name === 'notifications') {
                return Promise.resolve({state: Notification.permission});
            }
            return origQuery.call(this, params);
        };
    }

    // === 隐藏 automation flags ===
    delete navigator.__proto__.webdriver;

    // === window.outerWidth/outerHeight (headless 通常为 0) ===
    if (window.outerWidth === 0) {
        Object.defineProperty(window, 'outerWidth', { get: () => window.innerWidth });
        Object.defineProperty(window, 'outerHeight', { get: () => window.innerHeight + 85 });
    }
})();
`

// loadMeta 从 JSON 文件加载元数据。
func (m *ProfileManager) loadMeta() (*profileMetadata, error) {
	data, err := os.ReadFile(m.metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &profileMetadata{}, nil
		}
		return nil, fmt.Errorf("browser: read profile meta: %w", err)
	}
	var meta profileMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("browser: parse profile meta: %w", err)
	}
	return &meta, nil
}

// saveMeta 保存元数据到 JSON 文件（零 SQLite）[Ref: BP §B3]。
func (m *ProfileManager) saveMeta(meta *profileMetadata) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("browser: marshal profile meta: %w", err)
	}
	// 确保父目录存在
	if err := os.MkdirAll(filepath.Dir(m.metaPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(m.metaPath, data, 0644)
}
