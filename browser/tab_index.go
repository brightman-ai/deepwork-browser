// Package browser — TabIndex: Tab 四元身份注册表 [v2 ]
//
// 来源:
// - (TabHandle / TabRole / PoolSnapshot 类型契约)
// - (Tab 四元身份: TargetID, IdentityKey, WorkspaceID, Role)
// - + §V2.E (TabIndex 新增)
//
// 范围 :
// - TabHandle 类型 + 4 Role 枚举 (CAP §2.bis lines 127-150)
// - TabIndex 接口 + 内存实现: Register / Lookup / Unregister / ByIdentity / ByWorkspace
// - PoolSnapshot / IdentityPoolStatus 类型 (CAP §2.bis lines 143-150)
//
// 不在范围:
// - Pool 主流程集成由 pool.go 完成 (本文件仅提供索引数据结构)
// - first-claim-wins 协调逻辑在 input_gateway.go (+, J4 默认值)
//
// 设计说明:
// - TabIndex 是 BrowserPool 内部依赖, 不直接暴露给业务调用方.
// - 业务调用方拿到 *TabHandle 后通过 TargetID 反查 (Pool.Inspect / Pool.ReleaseTab).
// - "唯一性"约束: 同 TargetID 不可重复 Register ( 守护).
//
// 关于 BrowserCore 句柄:
// - : TabHandle 暂不携带 BrowserCore 引用 (CAP §2.bis 字段集 final).
// - Pool 内部维护并行 map[TargetID]BrowserCore (通过 BrowserPool.GetCore(targetID) 暴露)
// ( 4 入口迁移完成后, BrowserCore 句柄会从业务消失 — 走 SessionCore 模型).
package browser

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ============================================================
// § Tab 四元身份类型
// ============================================================

// TabRole — Tab 角色枚举 (4 枚举之一, T5 §0.3 + D11 强约束).
//
// 不变式 (D11):
// - RoleHuman Tab: Agent ActionEngine 直调必须被 InputGateway 拒绝.
// - RoleAgent Tab: Human 可观察, 不可接管 (除 D10 受控 override).
// - RoleCouncil Tab: 必须配合 Profile 前缀 "council-*" (Pool 校验, P1).
// - RoleBackground Tab: SyncManager 等长跑路径; 无 LiveView, 不接受 takeover.
type TabRole string

const (
	RoleHuman TabRole = "human" // Human only — Agent 不可直调
	RoleAgent TabRole = "agent" // Agent only — Human 可观察, 不可接管 (除受控 override)
	RoleCouncil TabRole = "council" // Council Agent (per platform 限定, 必须配合 council-* identity)
	RoleBackground TabRole = "background" // SyncManager 长跑无 LiveView
)

// validRoles — 4 枚举矩阵. AcquireTab 入参 Role 不在此集合 → ErrInvalidRole.
var validRoles = map[TabRole]struct{}{
	RoleHuman: {}
	RoleAgent: {}
	RoleCouncil: {}
	RoleBackground: {}
}

// IsValidRole 判断 role 是否在 4 枚举矩阵内.
func IsValidRole(r TabRole) bool {
	_, ok := validRoles[r]
	return ok
}

// TabHandle — Tab 四元身份 (CAP §2.bis lines 127-133).
//
// 不变式:
// - TargetID 唯一 (TabIndex 注册时校验, 重复 → ErrTabAlreadyRegistered).
// - IdentityKey 来自 IdentityRegistry.Resolve (上层拿到后透传, 不修改).
// - WorkspaceID 任务隔离维度 — 不影响 Chrome 资源边界 (T5 §0.3).
// - Role 必须 IsValidRole(r) == true (Pool 入口校验).
type TabHandle struct {
	TargetID string // CDP TargetID
	IdentityKey IdentityKey // 来自 IdentityRegistry
	WorkspaceID string // 任务隔离 (跨 ws 同 identity 复用 Chrome 但 Tab 隔离)
	Role TabRole
	SessionID string // BrowserSession 三层架构 SessionID ; 暂为空串
	AcquiredAt time.Time // Pool.AcquireTab 创建时戳 (诊断用)
}

// ============================================================
// § Pool Snapshot 类型
// ============================================================

// PoolSnapshot — Pool 当前状态的只读视图 (诊断 / 可观测).
//
// 注: Identities 按 IdentityKey 字典序排序 (确定输出, 便于诊断对比).
type PoolSnapshot struct {
	Identities IdentityPoolStatus
	TotalTabs int
}

// IdentityPoolStatus — 单个 identity 在 Pool 中的状态.
type IdentityPoolStatus struct {
	Key IdentityKey
	ChromePID int // Chrome 主进程 PID; 0 = 尚未启动 / 已退出
	TabCount int // 当前活跃 Tab 数
	Mode BrowserMode // 当前 Chrome 进程运行模式: human/headless
	Tabs TabHandle // 该 identity 下所有 Tab (按 TargetID 字典序)
}

// ============================================================
// § TabIndex 接口 [Ref: + 实施范围]
// ============================================================

// TabIndex — TargetID → *TabHandle 索引 (Pool 内部依赖, 业务不直接持有).
//
// 线程安全: 实现必须支持并发 Register / Lookup / Unregister / ByIdentity / ByWorkspace.
type TabIndex interface {
	// Register 注册一个 Tab. 同 TargetID 第二次 Register → ErrTabAlreadyRegistered.
	Register(handle *TabHandle) error

	// Lookup 反查 TargetID 对应的 TabHandle. 不存在 → (nil, false).
	Lookup(targetID string) (*TabHandle, bool)

	// Unregister 注销 Tab. 不存在的 TargetID → ErrTabNotFound.
	Unregister(targetID string) error

	// ByIdentity 列出某 identity 下所有 Tab (按 TargetID 字典序; 副本).
	ByIdentity(key IdentityKey) TabHandle

	// ByWorkspace 列出某 ws 下所有 Tab (按 TargetID 字典序; 副本).
	ByWorkspace(ws string) TabHandle

	// Snapshot 全量索引快照 (按 IdentityKey 字典序 → 内层按 TargetID 字典序).
	// 不含 ChromePID (Pool 注入); IdentityPoolStatus.ChromePID 在 Pool.Inspect 阶段填充.
	Snapshot IdentityPoolStatus

	// Len 返回当前注册的 Tab 总数 (诊断用).
	Len int
}

// ErrTabAlreadyRegistered — Register 重复 TargetID.
var ErrTabAlreadyRegistered = errors.New("tab already registered for TargetID")

// ErrTabNotFound — Unregister / Lookup 未命中.
var ErrTabNotFound = errors.New("tab not found in index")

// ============================================================
// § 内存实现 ( 默认)
// ============================================================

// memoryTabIndex — 线程安全的 in-memory TabIndex.
type memoryTabIndex struct {
	mu sync.RWMutex
	store map[string]*TabHandle // TargetID → handle (单一权威; Lookup/Unregister 走此 map)
}

// NewTabIndex 返回一个新的内存 TabIndex.
func NewTabIndex TabIndex {
	return &memoryTabIndex{
		store: make(map[string]*TabHandle)
	}
}

// Register 实现 TabIndex.Register.
//
// 校验:
// - handle 非 nil.
// - handle.TargetID 非空.
// - handle.IdentityKey 非空.
// - handle.Role 在 4 枚举矩阵内.
// - 同 TargetID 不可重复注册 ( 守护).
func (idx *memoryTabIndex) Register(handle *TabHandle) error {
	if handle == nil {
		return errors.New("tab index: handle is nil")
	}
	if handle.TargetID == "" {
		return errors.New("tab index: TargetID is empty")
	}
	if handle.IdentityKey == "" {
		return errors.New("tab index: IdentityKey is empty")
	}
	if !IsValidRole(handle.Role) {
		return fmt.Errorf("tab index: invalid role %q (allowed: human/agent/council/background)", handle.Role)
	}

	idx.mu.Lock
	defer idx.mu.Unlock

	if _, exists := idx.store[handle.TargetID]; exists {
		return ErrTabAlreadyRegistered
	}
	// 防御: 拷贝 handle 避免外部 mutate.
	copyHandle := *handle
	if copyHandle.AcquiredAt.IsZero {
		copyHandle.AcquiredAt = time.Now
	}
	idx.store[copyHandle.TargetID] = &copyHandle
	return nil
}

// Lookup 实现 TabIndex.Lookup.
//
// 注: 返回的是 store 内 *TabHandle, 调用方不应 mutate (struct 字段视为只读).
func (idx *memoryTabIndex) Lookup(targetID string) (*TabHandle, bool) {
	idx.mu.RLock
	defer idx.mu.RUnlock
	handle, ok := idx.store[targetID]
	if !ok {
		return nil, false
	}
	// 返回副本指针以隔离内部 store.
	out := *handle
	return &out, true
}

// Unregister 实现 TabIndex.Unregister.
func (idx *memoryTabIndex) Unregister(targetID string) error {
	idx.mu.Lock
	defer idx.mu.Unlock
	if _, ok := idx.store[targetID]; !ok {
		return ErrTabNotFound
	}
	delete(idx.store, targetID)
	return nil
}

// ByIdentity 实现 TabIndex.ByIdentity.
func (idx *memoryTabIndex) ByIdentity(key IdentityKey) TabHandle {
	idx.mu.RLock
	defer idx.mu.RUnlock

	var out TabHandle
	for _, h := range idx.store {
		if h.IdentityKey == key {
			out = append(out, *h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TargetID < out[j].TargetID })
	return out
}

// ByWorkspace 实现 TabIndex.ByWorkspace.
func (idx *memoryTabIndex) ByWorkspace(ws string) TabHandle {
	idx.mu.RLock
	defer idx.mu.RUnlock

	var out TabHandle
	for _, h := range idx.store {
		if h.WorkspaceID == ws {
			out = append(out, *h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TargetID < out[j].TargetID })
	return out
}

// Snapshot 实现 TabIndex.Snapshot — 按 IdentityKey 分组, 每组按 TargetID 字典序.
//
// 注: 不含 ChromePID; 由 Pool.Inspect 在外层补充注入.
func (idx *memoryTabIndex) Snapshot IdentityPoolStatus {
	idx.mu.RLock
	defer idx.mu.RUnlock

	grouped := make(map[IdentityKey]TabHandle)
	for _, h := range idx.store {
		grouped[h.IdentityKey] = append(grouped[h.IdentityKey], *h)
	}

	out := make(IdentityPoolStatus, 0, len(grouped))
	for key, tabs := range grouped {
		sort.Slice(tabs, func(i, j int) bool { return tabs[i].TargetID < tabs[j].TargetID })
		out = append(out, IdentityPoolStatus{
			Key: key
			TabCount: len(tabs)
			Tabs: tabs
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Len 实现 TabIndex.Len.
func (idx *memoryTabIndex) Len int {
	idx.mu.RLock
	defer idx.mu.RUnlock
	return len(idx.store)
}
