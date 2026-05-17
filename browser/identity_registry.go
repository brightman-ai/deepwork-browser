// Package browser — IdentityRegistry: identity 三元组解析与归一化 [v2 ]
//
// 来源:
// - (IdentityRegistry interface 契约)
// - + ( 必须实现)
//
// 范围 :
// - 接口 + 内存实现 (sync.Map 后备 store).
// - Resolve / Inspect / List 三方法.
// - 不做 Pool 集成 ( 才把 IdentityKey 传给 BrowserPool.AcquireTab).
//
// 推迟标记:
// - 推迟项: 持久化 (跨进程 IdentityRegistry 共享).
// - 理由分类: 技术依赖 — 需要 browser Pool 重构后才能确定持久化粒度.
// - 触发条件: 启动 + Pool snapshot 持久化协议确认.
// - 验证标准: (重启后 Pool 状态恢复 + IdentityKey 同前一致).
package browser

import (
	"errors"
	"sort"
	"sync"
)

// ============================================================
// § IdentityRegistry 接口
// ============================================================

// IdentityRegistry — identity 三元组解析与归一化 (D1).
//
// 对照 Cap §2.bis:
//
//	Resolve(profile, preset, policy) → IdentityKey // 确定性归一化
//	Inspect(key) → *IdentityDescriptor // 反查
//	List → IdentityDescriptor // 全量列表 (诊断/可观测)
//
// 线程安全: 实现必须支持并发 Resolve / Inspect / List.
type IdentityRegistry interface {
	// Resolve 把 (profile, preset, policy) 三元组归一化为 IdentityKey.
	// 同三元组多次 Resolve 必须返回同一 IdentityKey (确定性).
	// 同时把 IdentityDescriptor 注册到内部 store (供 Inspect / List 反查).
	Resolve(profile string, preset Preset, policy IsolationPolicy) (IdentityKey, error)

	// Inspect 反查 IdentityKey 对应的三元组. 未注册 → ErrIdentityNotFound.
	Inspect(key IdentityKey) (*IdentityDescriptor, error)

	// List 列出当前 Registry 已知的所有 identity (按 IdentityKey 字典序).
	List IdentityDescriptor
}

// ErrIdentityNotFound — Inspect 未命中.
var ErrIdentityNotFound = errors.New("identity not found in registry")

// ============================================================
// § 内存实现 ( 默认)
// ============================================================

// memoryIdentityRegistry — 线程安全的 in-memory IdentityRegistry.
type memoryIdentityRegistry struct {
	mu sync.RWMutex
	store map[IdentityKey]IdentityDescriptor
}

// NewIdentityRegistry 返回一个新的内存 IdentityRegistry.
//
// 初始为空; 首次 Resolve 即 lazy-register.
func NewIdentityRegistry IdentityRegistry {
	return &memoryIdentityRegistry{
		store: make(map[IdentityKey]IdentityDescriptor)
	}
}

// Resolve 实现 IdentityRegistry.Resolve.
//
// 算法:
// 1. 计算 IdentityKey = NewIdentityKey(profile, preset, policy)
// 2. 写锁后 upsert: 若 key 已存在 → 直接返回 (不覆盖, 保证幂等)
// 否则 → 注册 IdentityDescriptor, 然后返回 key.
//
// 幂等性 ( 要求): 同三元组多次 Resolve 不重复注册 (store 大小不变).
func (r *memoryIdentityRegistry) Resolve(profile string, preset Preset, policy IsolationPolicy) (IdentityKey, error) {
	if policy == nil {
		policy = NoopPolicy{}
	}
	key := NewIdentityKey(profile, preset, policy)

	r.mu.Lock
	defer r.mu.Unlock

	if _, exists := r.store[key]; !exists {
		r.store[key] = IdentityDescriptor{
			Key: key
			Profile: profile
			Preset: preset
			Policy: policy
		}
	}
	return key, nil
}

// Inspect 实现 IdentityRegistry.Inspect.
//
// 注: 返回的是 store 内值的副本 (struct value), 调用方可安全持有.
// Policy 字段是接口, 仍指向同一 IsolationPolicy 实例 — 调用方不应修改.
func (r *memoryIdentityRegistry) Inspect(key IdentityKey) (*IdentityDescriptor, error) {
	r.mu.RLock
	defer r.mu.RUnlock

	desc, ok := r.store[key]
	if !ok {
		return nil, ErrIdentityNotFound
	}
	return &desc, nil
}

// List 实现 IdentityRegistry.List.
//
// 返回按 IdentityKey 字典序排序的副本 (确定顺序 — 便于诊断输出比对).
func (r *memoryIdentityRegistry) List IdentityDescriptor {
	r.mu.RLock
	defer r.mu.RUnlock

	out := make(IdentityDescriptor, 0, len(r.store))
	for _, desc := range r.store {
		out = append(out, desc)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Key < out[j].Key
	})
	return out
}
