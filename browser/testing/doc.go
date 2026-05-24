// Package testing 实现 dw-browser 的 AI-native 测试引擎增强层。
//
// 该包在现有浏览器引擎之上提供"看做判证"四步闭环：
//   - observe: 多通道终态快照（structural + visual + behavior + telemetry）
//   - check:   确定性断言引擎（Assertion DSL）
//   - diff:    前后状态对比
//   - journey: BDD YAML 旅程执行
//   - evidence: 证据持久化和回放
//
// 设计原则：
//   - 增强不降级：不修改 browser/ 包现有行为
//   - 确定性优先：断言引擎零 LLM 依赖（VLM 仅做 soft triage）
//   - 开源通用：零业务依赖，可被任意 Web 应用复用
package testing
