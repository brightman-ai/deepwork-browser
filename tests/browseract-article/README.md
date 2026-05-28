# BrowserAct Article Test Cases

Status: active baseline
Source: user-provided text from `https://mp.weixin.qq.com/s/Q5eWBCIXQ8MjJPKijJqrLw`

Latest local verified report: `tests/browseract-article/reports/20260528-023606-93463/summary.md`
(`reports/` contains generated run artifacts and is intentionally not part of the source baseline.)

Jina read result on 2026-05-28 returned the WeChat environment verification page only:

```text
Weixin Official Accounts Platform / 环境异常 / 当前环境异常，完成验证后即可继续访问。
```

The cases below are therefore compiled from the pasted article text.

## Business Facts

The article describes BrowserAct as an AI-agent browser automation CLI with these core claims:

- Three breakthrough layers:
  - environment: stealth browser, fingerprint camouflage, dynamic proxy, session/cookie/profile isolation.
  - execution: `solve-captcha`, `stealth-extract`, protected-page content extraction.
  - human collaboration: `remote-assist` link for SMS, scan login, and other manual steps, then agent resumes the same session.
- Browser modes:
  - `stealth`: anti-detection browser plus proxy rotation.
  - `chrome`: isolated imported local Chrome profile and login state reuse.
  - `chrome-direct`: direct control of the currently running Chrome.
- Representative scenarios:
  - `httpbin.org/headers` shows coherent Firefox/Windows request headers.
  - Xiaohongshu Explore extraction succeeds where curl only returns obfuscated JavaScript.
  - Zhihu search can use remote assist when login is required.
  - Sogou WeChat search can list recent AI Agent articles by authors such as 苍何, JavaGuide, 沉默王二.
  - Product Hunt may require stealth-extract plus dynamic proxy when full browser mode is blocked.
- Comparison with agent-browser:
  BrowserAct adds stealth browser, captcha solving, dynamic proxy, remote assist, and universal `stealth-extract`.

## Test Routes

`run_comparison.sh` runs four approaches against the same non-trivial local fixture:

| Route | Purpose | Gate |
|---|---|---|
| A11y deterministic | Stable structural controls and article facts | Required |
| NL step journey | Natural-language step inside BDD journey | Informational-to-required once stable |
| Plan/do | Direct planner output + execution boundary | Required for AI-native CLI viability |
| Vision LLM | Screenshot-level oracle over a rendered page | Informational; reports BLOCKED when provider unavailable |

The fixture intentionally contains a dense article layout, search/filter interaction, disclosure panels, a comparison table, and a fake video block. It is more complex than a static `example.com` smoke page while remaining deterministic enough to isolate `dw-browser` behavior.
