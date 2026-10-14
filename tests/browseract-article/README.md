# BrowserAct Article Test Cases

Status: typed v2 authored fixtures; current live execution pending
Source: user-provided text from `https://mp.weixin.qq.com/s/Q5eWBCIXQ8MjJPKijJqrLw`

Historical v1 local report (does not verify the current v2 runtime): `tests/browseract-article/reports/20260528-023606-93463/summary.md`
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

`run_comparison.sh` runs two typed authored journeys and one informational visual check against the local fixture. Set `DW` to the candidate executable; the script writes a new report directory and preserves historical artifacts.

| Route | Purpose | Gate |
|---|---|---|
| Deterministic article journey | Filter, clear, switch layer, generate summary | Required |
| Caller-authored typed journey | Filter remote-assist, assert one card, generate summary | Required |
| Visual advice | Screenshot review with explicit provider evidence | Informational |

`journeys/nl-step.yaml` retains its historical filename for callers but contains explicit typed actions. Runtime NL planning and `plan/do` are retired. A caller may author a typed plan from natural-language intent before submitting a journey; `app-test-baseline` invokes no internal LLM.

Plain headings/cards are verified through rendered regions. The summary assertion checks the previously empty output region, rather than the permanent button label containing the same words. A CSS probe checks the exact filter status. Initial-load telemetry is not claimed as zero without collection evidence.

Run:

```bash
DW=/path/to/candidate tests/browseract-article/run_comparison.sh
```

The fixture contains a dense article layout, search/filter interaction, disclosure panels, comparison table and fake video block. These cases validate local browser behavior, not BrowserAct's external anti-detection or captcha claims.
