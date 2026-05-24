---
name: dw-browser-skills
description: Browser automation skills for dw-browser CLI. Read before operating websites.
when_to_use: When about to automate a website with dw-browser, check for available domain skills first.
---

# dw-browser Browser Skills

每个子目录是一个站点 skill，包含该站点的操作处方 (action recipes)。

## 使用流程

1. `dw-browser snap` 显示当前 URL 的可用 skill
2. 读取对应 `SKILL.md` 获取 action recipes
3. 替换 `{参数}` → 执行 dw-browser 命令
4. 任务成功后，如有非显然发现 → 写回

## 写回规则

- **时机**: 任务结束时，不是每轮
- **粒度**: "下一个 Agent 会踩坑吗？" YES → 写。NO → 不写
- **内容**: 最简成功路径 + 非显然 gotcha，不写操作日记
- **命令**: `dw-browser skills write {domain} --action {name}`

## Skill 格式

每个站点 skill = 一个目录 + `SKILL.md`：

```
{domain}/SKILL.md
  frontmatter: name, description, domain, dependencies, actions, verified_at
  ## Site — 站点上下文 (framework, auth, anti-bot)
  ## {action} — intent + recipe (dw-browser 命令) + gotchas
```

## 依赖

所有 action recipe 使用 `dw-browser` CLI 命令语法：
- `fill {selector} '{text}'` — 填入文本
- `click {selector}` — 点击元素
- `clickat {selector} {x} {y}` — 坐标点击 (anti-bot 场景)
- `press {key}` — 按键
- `wait "{condition}" --timeout {seconds}` — 等待条件
- `scroll up|down` — 滚动
- `eval "{js}"` — 执行 JavaScript

## 可观测: --skill flag (stdout event stream)

执行 skill recipe 时，每条 act 命令附加 `--skill {action-name}` → stdout JSON 自动携带 skill 上下文。

### 用法

```bash
dw-browser act --session s1 --skill send-message "fill #prompt-textarea 'Hello'"
dw-browser act --session s1 --skill send-message "press Enter"
dw-browser act --session s1 --skill send-message "wait 'not [aria-busy]' --timeout 30"
```

### stdout JSON 规范

```json
{
  "session_id": "s1",
  "success": true,
  "skill": {
    "action": "send-message",
    "intent": "发送消息",
    "domain": "chatgpt.com",
    "step": 2,
    "total": 3
  },
  "snap": "...",
  "user_state": { "..." }
}
```

| 字段 | 类型 | 来源 | 说明 |
|------|------|------|------|
| `skill.action` | string | `--skill` flag | 正在执行的 action 名 |
| `skill.intent` | string | SKILL.md 自动读取 | action 的中文意图 |
| `skill.domain` | string | session URL 自动推导 | 站点域名 |
| `skill.step` | int | session 内自增 | 当前步骤 (1-based) |
| `skill.total` | int | SKILL.md recipe 行数 | 总步数 (0=未知) |

**自动推导规则**:
- domain: 从 session 当前 URL 提取 → 转换为 skill 目录名 (`.` → `-`)
- intent: 从 `{domain}/SKILL.md` 的 `## {action}` 节读取 `intent:` 行
- total: 从 ` ```dw-browser` 代码块内计数非空行
- step: 同一 session + 同一 action name 的 act 调用计数

**无 `--skill` 时**: `skill` 字段不出现，输出完全兼容。
**SKILL.md 不存在时**: 仅携带 `action` 字段，其余为空/0（优雅降级）。

### 消费者模式 (Unix stdout 哲学)

dw-browser 只写 stdout JSON。谁读是谁的事:

| 消费者 | 读取方式 | 展示 |
|--------|---------|------|
| Claude Code Agent | 读 JSON, 内联展示 | `[send-message 1/3] fill ✓ (120ms)` |
| go-claudecode SDK | 解析 skill 字段 | Go struct → 调用方 |
| Deepwork Workforce (BS-04) | 不走 CLI, 走 ToolRegistry | StageEvent → SSEHub → UI |
| Human terminal | 直接看 JSON | skill.action + step 可读 |

## 目录说明

- `interaction/` — 通用交互技能 (shadow-dom, iframe, scroll, anti-bot)
- `{domain}/` — 站点特定 skill (域名中 `.` → `-`)

## Recording → Skill 提炼规则

当收到 `trace.json` 录制文件时，按以下规则提炼为 SKILL.md action:

### 1. 意图推断
从操作序列推断 Human 的意图。
- trace 显示 click(#prompt-textarea) → type("Hello") → Enter → wait → 推断 intent: "发送消息 / send a message"
- intent 用中英双语，匹配自然语言查询

### 2. 步骤蒸馏
去掉探索/纠错步骤，只保留最终成功路径的最简命令序列。
- 如果 Human 对同一 target 操作了多次（2次失败1次成功），只保留成功的那次
- 时间间隔 >3s 的停顿暗示"这里需要 wait"

### 3. 参数化
将具体输入值替换为 `{param}` 槽位。
- "Hello, how are you?" → `{message}`
- 保留有意义的默认值说明

### 4. Gotcha 发现
从 target 信息中提取非显然知识:
- tag="div" 且 role="textbox" → "输入框是 contenteditable div, NOT textarea"
- 操作多次才成功 → "此元素选择器可能不稳定"
- 用 Enter 而非点击 Send → "press Enter 比 click send 按钮更可靠"

### 5. 原子拆分
一次录制可能包含多个独立操作。按以下信号拆分为多个 action:
- URL 变化（页面导航）= 不同 action
- 长停顿 >3s = 自然分界
- 操作 target 语义域变化（导航栏→内容区）

展示拆分建议让 Human 确认:
```
从你的录制中我识别了 2 个 action:
  1. search-user (fill search + Enter + 等结果)
  2. send-connection (click Connect + click "Send without a note")
要分别保存为独立 skill 吗？
```

### 6. 输出格式
生成标准 `## {action-name}` 节:
```
## send-message
intent: 发送消息 / send a message
  ```dw-browser
  fill #prompt-textarea '{message}'
  press Enter
  wait "not [aria-busy]" --timeout 15
  ```
Precondition: 已打开 chatgpt.com 对话页面 + 已登录
Gotchas:
- 输入框是 contenteditable div, NOT textarea
- press Enter 比 click send 按钮更可靠
```

### 7. 对话调整
生成草稿后，展示给 Human 并邀请调整:
- "这个 recipe 准确吗？有什么需要调整的？"
- Human 反馈后更新草稿，直到确认
- 确认后: `dw-browser skills write {domain} {action}`
