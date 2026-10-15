# Screenshot loading failures fixed in 0.12.1-v2

This is a loading-state-dependent failure, not a failure of every website.
The same Readwise Appearance URL successfully produced a screenshot and 9,841
characters of document text in a fresh headless session using the installed fix.
No claim is made that the historical Readwise request's exact stalled resource
can be recovered: that session was closed and its artifacts lacked network and
initial-render diagnostics.

## Reproduced mechanism

A real Chrome page with a render-blocking stylesheet and a following head script
reproduces the reported artifact shape. While the stylesheet response is held:

- The title and URL are available, but document.readyState is loading.
- There is no document body, no paint timing entry, and one pending stylesheet.
- Two Page.captureScreenshot requests each time out after five seconds.
- The old CLI reports empty actionable structural evidence and complete text,
  overwrites the screenshot cause with image: unknown format, then waits for
  a visible body during content extraction until its outer deadline expires.

Releasing the stylesheet lets the same browser return a real JPEG in 25ms.
This separates renderer readiness from CDP connection recovery. A new socket
cannot make an unresolved stylesheet finish loading. Ordinary completed pages
remain captureable; a separate urllib HTTP 403 is not evidence of this failure.

## Changes

- Empty initial-render facts remain progressive/incomplete and grant no refs.
- Real capture is attempted first, with its existing five-second budget.
  Only failure triggers a separate, bounded, read-only readiness diagnostic.
  An unavailable diagnostic cannot block a successful screenshot.
- Explicitly pending initial render suppresses the redundant identical retry.
  Unknown renderer state and genuine transport failures retain isolated recovery.
- Canonical artifacts preserve the actual screenshot error and readiness facts.
- Missing document bodies make content unavailable immediately. A genuinely empty,
  completed body returns captured empty text, not an implicit wait for visibility.
  Focused selector behavior remains unchanged.

## Verification

The fixed CLI returned the controlled pending case in 5.19s with
initial_render_pending, loading, has_body=false and pending_stylesheets=1.
Its structural channel was unavailable, text incomplete and actual screenshot
cause preserved. After releasing CSS, observe succeeded in 136ms in the same
session. Completed about:blank with content text succeeded in 195ms.

The installed 0.12.1-v2 captured the Readwise page with 46 visible controls.
An earlier navigation exceeded its existing 15-second load watchdog; the next
observation succeeded without replaying navigation. A slow load-completion event
therefore remains distinct from screenshot or connection failure.

Targeted browser contracts and root/CLI/journey unit packages passed. The actual
installed binary also passed the native edit/save/revisit baseline with expected
exits 2/0/1/0 and exact UTF-8 plus trailing-newline persistence on healthy runs.
These checks cover macOS arm64 Chrome headless, not every site or display mode.

Relevant guards include TestScreenshotTimeoutRetainsPageStateAndRetryDecision,
TestCaptureDiagnosticsCannotPreventSuccessfulScreenshot,
TestInitialRenderProgressiveFactsRemainIncomplete,
TestDocumentTextDistinguishesAbsentAndEmptyBody, and
TestObservationPreservesScreenshotCaptureFailure.
