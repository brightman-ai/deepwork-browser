# BrowserAct 文章多路径测试任务矩阵

状态: 当前基线通过
事实源: 用户粘贴的微信公众号正文；Jina 读取原 URL 只返回“环境异常/去验证”页面。

最新本地基线: `tests/browseract-article/reports/20260528-023606-93463/summary.md`
说明: `reports/` 为运行产物，不纳入源码基线提交。

最新结果:

- A11y 确定性路径: PASS
- NL step BDD 路径: PASS
- plan/do 路径: PASS
- Vision LLM 路径: PASS

## 目标

先用一篇复杂中文文章场景验证 `dw-browser` CLI 的多路径测试能力和可靠性，再把稳定组合迁移到 `deepwork-pro` 的 BS Portal CUJ。

## 30 项任务队列

### A. 事实源与 fixture

1. 记录 Jina 读取结果和用户粘贴正文来源。
2. 抽取 BrowserAct 核心业务事实: 三层突破、三种浏览器模式、典型场景、对比表。
3. 构建复杂中文文章 fixture，覆盖正文、视频占位、卡片、表格、筛选、折叠面板。
4. 给所有关键控件加稳定 `data-testid`。
5. fixture 必须能无网络本地运行，避免外站波动污染 CLI 能力测试。
6. fixture 必须比 `example.com` 更复杂: 至少 8 个断言事实、3 个交互控件、1 个表格、1 个动态筛选。

### B. dw-browser CLI 多路径

7. A11y 确定性路径: `open -> observe -> check -> act -> check`。
8. A11y 路径验证结构化中文内容和控件可操作性。
9. NL step 路径: 用 BDD `journey` 的自然语言 `do` 触发 planner。
10. NL step 路径必须保存 journey trace、report、before/after observation。
11. Plan/do 路径: 直接 `plan` 生成计划并 `do` 执行同一业务目标。
12. Plan/do 路径必须保存 plan JSON、do JSON 和执行后 observation。
13. Vision LLM 路径: `observe --layers visual` + `check --using visual`。
14. Vision 路径必须使用已配置的 Gemma 4 26B A4B VLM；不可用时明确 BLOCKED。
15. 四路径必须共用同一 fixture 和同一类业务目标，便于可比。
16. 四路径结果必须输出中文对比报告。
17. 对比报告包含通过率、失败原因、可靠性、可维护性、推荐用途。

### C. 发现问题即修

18. `check --using visual` 自动加载 `~/.deepwork/testing-llm.env`。
19. planner 输出必须通过 `dw-browser act` 语法校验。
20. planner 的 `none/no wait/n/a` 等 filler wait 归一为空。
21. planner 不允许不可执行 wait prose，例如 `page load`。
22. NL step / plan-do 中 wait 失败必须显式失败，不能吞掉。
23. 需要时补 CLI help，使用户知道 LLM/VLM 参数和默认 env。
24. 每个修复都补单元测试或脚本级回归。

### D. deepwork-pro BS Portal 迁移

25. 根据 BS-15/CUJ-15 设计写中文 CUJ 任务矩阵。
26. 把可靠机制组合成 BS Portal 推荐测试栈: A11y 基线 + plan/do 探索 + vision 布局巡检 + runtime oracle。
27. 设计 Browser Portal 地址栏导航/阅读、搜索/滚动、多 tab、AI Sidebar、Skill、恢复六类 CUJ。
28. 为每类 CUJ 定义 Human action、可见事实、runtime oracle、失败归因。
29. 先落一条可执行 BS Portal 脚本，再逐步扩展高风险路径。
30. 所有基线、报告、验收说明用中文保存。

## 验收标准

- `tests/browseract-article/run_comparison.sh` 能一键运行并生成中文报告。
- 至少 A11y 和 plan/do 两条路径通过；Vision 如 provider 不可用必须记录 BLOCKED 而不是静默跳过。
- 任何 `dw-browser` CLI 缺陷要么修复并回归，要么在报告中列为明确缺口。
- 后续 BS Portal CUJ 不从空白开始，而是复用本目录验证过的能力组合。
