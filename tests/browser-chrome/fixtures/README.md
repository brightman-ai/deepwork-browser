# 回归夹具 oracle 表

每个夹具是一个最小复现页，用于锚定 chrome 仿真（persona mobile）的截图/audit/act 行为。
「预期」列描述的是 chrome 仿真**应当如实复现的真机体验**——健康页应显形为健康，病灶页应显形为病灶；两者互相误判都算 bug。

| 文件 | 模拟的真实场景 | chrome 仿真下预期（截图/audit/act） | 对应 REQ |
|---|---|---|---|
| `vv-adaptive.html` | 真机 Safari 正常页：消费 `visualViewport.height` 自适应布局，底部工具条始终可见可点 | 截图底部工具条完全可见、不被 chrome 层遮挡；audit 无遮挡告警；act 能命中 `#btn-send` | REQ-BC-11 🟢 |
| `vh-fixed.html` | 经典 `100vh` 病灶页：真机 iPhone Safari 上底部工具条被浏览器底栏遮挡、点不到 | 截图必须把遮挡显形——底部工具条沉入 chrome 底栏之下；audit 应告警；act 命中该区域应 fail-loud | REQ-BC-01 / REQ-BC-13 🔴 |
| `kb-naive.html` | 纯键盘病灶页：`window.innerHeight` 钉高（pre-visualViewport 经典写法）——底栏适配正确，但 iOS 上 innerHeight 不随键盘变，软键盘弹起后 composer 被键盘吞掉 | 无键盘时 composer 可见可点；click `#msg` 自动弹键盘后截图 composer 必须被键盘区显形遮挡；audit 应告警（键盘类文案）；act 命中键盘区应 fail-loud | REQ-BC-12 🔴 |
| `kb-adaptive.html` | 聊天 composer 正常页：用 `visualViewport.height` 钉高 + 监听 `resize`/`scroll`，键盘弹起时 composer 自动上浮到键盘上方 | `act "keyboard show"` 后截图 composer 完全可见、位于键盘上方；audit 无遮挡告警；act 能命中 `#msg`/`#btn-send` | REQ-BC-12 🟢 |
| `fixed-bottom.html` | `position:fixed;bottom:0` 底锚工具条（无 dvh/safe-area）：真机 bars-expanded 时 layout viewport=svh，fixed 元素在底栏上方**可见**——双视口模型的引擎级残留假阳通道 | act 点击应豁免放行；audit 应 pass + `fixed-anchored` 标记（降级注记，非 fail）；终判交 `--engine safari` 真机对标 | REQ-BC-05 MODIFIED(2) 🟢 |
| `units-probe.html` | 视口事实自报页（E2E 断言用）：把测得的 `100svh/100dvh/100vh/innerHeight/vv.height/env(safe-area-inset-bottom)` 与最近一次点击坐标（`lastClick`，tapxy 契约）写进 DOM，`observe --tree` 读回 | 默认 `svh=659 dvh=759 vh=759 innerH=659 vvH=659 saB=34`；`keyboard show` 后仅 `vvH=457` 变（iOS 语义：innerHeight 不随键盘）；`tapxy 0.5 0.5` → `lastClick=196,379`（布局视口基准，不受 shim/zoom 干扰）| REQ-BC-11 🟢 |

## 假阳 bug 最小复现

修复前（≤0.8.0），`vv-adaptive.html` 与 `vh-fixed.html` 的仿真截图**完全一样**——chrome 只画像素、
不变换页面感知的视口，自适应页照全高布局后被画层吞掉，与病灶页不可区分 = 健康页被误判遮挡（假阳）；
同时键盘态完全不存在 = 整类键盘 bug 不可见（假阴）。
基线证据见 `docs/.tg/work/20260719-192107-browser-chrome-vv-fidelity/evidence/`。
