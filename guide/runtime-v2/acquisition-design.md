# Content acquisition and DW-Browser's purpose

Research date: 2026-10-16. This is design rationale, not a new command contract.
Public-reader contracts remain in [document](document.md) and [fetch](fetch.md).

DW-Browser's primary task is to let an AI experience a Web product, obtain real
evidence, explore behavior, and replay accepted journeys deterministically.
Public website research and embedded reading are auxiliary use cases. A body
download or DOM capture cannot establish visible UI quality, interaction success
or baseline acceptance. If required screenshots are unavailable, the relevant UI
acceptance remains blocked until that evidence is recovered.

## What existing projects solve

| Task | Established open-source options | Relationship to DW-Browser |
| --- | --- | --- |
| HTTP response-body transfer | [curl/libcurl](https://curl.se/libcurl/), [Go net/http](https://pkg.go.dev/net/http) | Reuse an HTTP/TLS implementation. DW's fetch currently uses net/http and the existing shared guard. |
| Execute JavaScript and read DOM | [Playwright](https://playwright.dev/docs/api/class-page#page-content), [chromedp](https://github.com/chromedp/chromedp) | DW already uses chromedp/CDP and Chrome. Document reuses that browser stack. |
| Extract readable article content from HTML/DOM | [Mozilla Readability](https://github.com/mozilla/readability), [Trafilatura](https://github.com/adbar/trafilatura) | A consumer can apply an extraction library after acquisition. Extraction is a different result from DOM or original bytes. |
| Crawl multiple pages and produce Markdown/structured data | [Crawl4AI](https://github.com/unclecode/crawl4ai), [Firecrawl](https://github.com/firecrawl/firecrawl) | Appropriate candidates for a dedicated crawling/extraction workflow, rather than expansion of the experience runtime. |
| AI-driven browser interaction | [Playwright CLI](https://github.com/microsoft/playwright-cli), [agent-browser](https://github.com/vercel-labs/agent-browser) | Useful references for task entry, snapshots, refs, screenshots and persistent sessions. Their automation contracts do not establish DW's accepted experience-fidelity guarantees. |

These are different layers, not a ranking of interchangeable tools. This review
read official documentation/source; it did not run a comparative performance or
article-quality benchmark, and does not establish an absolute best project.

## Evidence behind the split

[Browser Use's official skill](https://github.com/browser-use/browser-use/blob/main/skills/browser-use/SKILL.md)
recommends plain HTTP tools for public information that needs no browser, and
reserves browser control for interaction, rendering or browser state. That
supports a lightweight byte path; it does not require every browser tool to own
its own generic downloader.

[Playwright separates APIRequestContext from page interaction](https://playwright.dev/docs/api/class-apirequestcontext).
It supports isolated HTTP request contexts as well as contexts sharing browser
cookies. Its [page.content](https://playwright.dev/docs/api/class-page#page-content)
returns page HTML. Those distinctions corroborate separating HTTP entity bytes,
rendered DOM and interactive evidence. They do not make an HTTP response a UI
test result.

Comparable network protection already exists elsewhere:
[Crawl4AI's security policy](https://github.com/unclecode/crawl4ai/blob/main/SECURITY.md)
documents a resolve-and-pin egress proxy and routing of related HTTP clients
through it, with redirect/peer checks in relevant download paths. These are
valuable prior art. Deployment and release scope matter: a Docker server policy
must not be assumed to apply to every direct library invocation.

## Current engineering choice

Keep the existing Go HTTP client and chromedp browser stack, with one shared
public-network guard. DW owns the task contract around them: explicit routing
and resolver selection, public-address authorization, bounded acquisition,
actual accounting, resource ownership, cleanup and typed receipts. It does not
implement an HTTP/TLS engine or a second browser driver.

`fetch` earns its place for embedded callers that need those same boundaries
without maintaining a parallel HTTP policy. For a one-off download without DW's
contract, curl remains a suitable independent tool. `document` earns its place
for browser-rendered DOM under the same acquisition policy, without granting
interaction, caller scripts or an existing profile.

Do not replace the engine merely because another project has more features.
A libcurl adapter or dedicated crawler should first be tested against the same
representative URLs, public/private redirect cases, proxy behavior, failure
accounting, cleanup and file-publication contract. It must show a concrete
benefit worth the additional dependency/lifecycle boundary. No such superiority
has been measured in this review.

For article extraction, evaluate existing libraries in the consuming reader
using its real multilingual articles and challenge/list pages. For a batch
crawling product, compare Crawl4AI and Firecrawl separately, including their
deployment and license requirements. Neither capability is introduced into
DW's experience core by this release.

The AI protocol therefore presents one UI-first task chooser and the interaction
workflow before auxiliary acquisition details. Acquisition failures retain
their own failure state; they cannot be substituted for missing UI evidence.
