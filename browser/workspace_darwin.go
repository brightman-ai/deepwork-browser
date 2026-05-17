//go:build darwin

// Package browser — macOS Workspace via SkyLight CGSPrivate API (V2: fullscreen + fast switchback).
//
// Strategy V2 :
//
// 1. SLSCopyManagedDisplaySpaces → capture displayUUID + currentSpaceID
// (Human's original Space).
// 2. LaunchInSpace:
// a. fork Chrome with --start-fullscreen (added in chrome_launcher.go).
// macOS deterministically creates a NEW fullscreen Space (type=4) for it.
// b. macOS auto-shifts view to that fullscreen Space (~3s animation).
// c. POLL dw_ws_inspect every 30ms: as soon as currentSpace != original
// immediately call SLSManagedDisplaySetCurrentSpace(uuid, original).
// → switchback latency = (animation start) + ≤30ms detection + 1 syscall.
// d. Chrome remains alive in its own fullscreen Space, invisible to Human.
//
// Why V2 over V1 (switch-launch-switchback):
// - V1 became unreliable on macOS 26: SLSManagedDisplaySetCurrentSpace
// is no-op for Desktop→Desktop transitions. Chrome ended up on Human's Space.
// - PoC (/tmp/sls_fs_sw2) confirmed: SLS-set still works for fullscreen→Desktop.
// - --start-fullscreen creates a Space deterministically (no "non-current Space"
// prerequisite), so Human no longer needs ≥2 Spaces in Mission Control.
//
// Why not SLSMoveWindowsToManagedSpace: cross-process window writes are silent
// no-ops under SIP-enabled macOS Sequoia/Tahoe. Own-process Chrome inherits the
// fullscreen Space at NSWindow creation time — SIP-safe.
//
// Constraints:
// - User sees ~1-3s of fullscreen animation as macOS shifts view; then snaps
// back. Chrome's own Space persists (visible in Mission Control).
//
//

package browser

/*
#cgo LDFLAGS: -framework CoreFoundation -framework CoreGraphics
#include <stdlib.h>
#include <string.h>
#include <dlfcn.h>
#include <CoreFoundation/CoreFoundation.h>
#include <CoreGraphics/CoreGraphics.h>

// ─── SkyLight private types ───────────────────────────────────────────────────
typedef int CGSConnectionID;
typedef uint64_t CGSSpaceID;

typedef CGSConnectionID (*SLSMainConnectionID_t)(void);
typedef CFArrayRef (*SLSCopyManagedDisplaySpaces_t)(CGSConnectionID cid);
typedef void (*SLSManagedDisplaySetCurrentSpace_t)(CGSConnectionID cid, CFStringRef displayUUID, CGSSpaceID spaceID);

// ─── Global function pointers (loaded once via dw_ws_init) ───────────────────
static void *g_slsLib = NULL;
static SLSMainConnectionID_t g_main = NULL;
static SLSCopyManagedDisplaySpaces_t g_copy = NULL;
static SLSManagedDisplaySetCurrentSpace_t g_set = NULL;

// dw_ws_init loads SkyLight symbols. Returns 0 on success. Idempotent.
int dw_ws_init {
 if (g_slsLib) return 0;
 g_slsLib = dlopen("/System/Library/PrivateFrameworks/SkyLight.framework/SkyLight"
 RTLD_LAZY | RTLD_LOCAL);
 if (!g_slsLib) return 1;
 g_main = (SLSMainConnectionID_t) dlsym(g_slsLib, "SLSMainConnectionID");
 g_copy = (SLSCopyManagedDisplaySpaces_t) dlsym(g_slsLib, "SLSCopyManagedDisplaySpaces");
 g_set = (SLSManagedDisplaySetCurrentSpace_t) dlsym(g_slsLib, "SLSManagedDisplaySetCurrentSpace");
 if (!g_main || !g_copy || !g_set) return 2;
 return 0;
}

// dw_ws_inspect populates *outUUIDBuf with the first display's UUID (UTF-8)
// *outCurrent with its current Space ID, and *outTarget with the first
// non-current normal (type 0) Space ID. Returns 0 on success.
// uuidBufSz: caller-allocated buffer size (≥64 recommended)
int dw_ws_inspect(char *outUUIDBuf, int uuidBufSz, uint64_t *outCurrent, uint64_t *outTarget) {
 if (!g_main || !g_copy) return 1;
 if (outUUIDBuf && uuidBufSz > 0) outUUIDBuf[0] = '\0';
 *outCurrent = 0;
 *outTarget = 0;

 CGSConnectionID cid = g_main;
 CFArrayRef displays = g_copy(cid);
 if (!displays) return 2;

 CFIndex nDisp = CFArrayGetCount(displays);
 for (CFIndex i = 0; i < nDisp; i++) {
 CFDictionaryRef disp = CFArrayGetValueAtIndex(displays, i);

 // First display only — UUID + current
 if (i == 0) {
 CFStringRef uuid = CFDictionaryGetValue(disp, CFSTR("Display Identifier"));
 if (uuid && outUUIDBuf) {
 CFStringGetCString(uuid, outUUIDBuf, uuidBufSz, kCFStringEncodingUTF8);
 }
 CFDictionaryRef curDict = CFDictionaryGetValue(disp, CFSTR("Current Space"));
 if (curDict) {
 CFNumberRef cur = CFDictionaryGetValue(curDict, CFSTR("ManagedSpaceID"));
 if (cur) CFNumberGetValue(cur, kCFNumberSInt64Type, outCurrent);
 }
 }

 // Pick target from first display's Spaces
 if (i == 0) {
 CFArrayRef spaces = CFDictionaryGetValue(disp, CFSTR("Spaces"));
 if (spaces) {
 CFIndex nSp = CFArrayGetCount(spaces);
 for (CFIndex j = 0; j < nSp; j++) {
 CFDictionaryRef sp = CFArrayGetValueAtIndex(spaces, j);
 int spType = -1;
 CFNumberRef tn = CFDictionaryGetValue(sp, CFSTR("type"));
 if (tn) CFNumberGetValue(tn, kCFNumberIntType, &spType);
 // type 0 = normal user Space; type 4 = fullscreen (skip)
 if (spType != 0 && spType != -1) continue;
 uint64_t sid = 0;
 CFNumberRef idn = CFDictionaryGetValue(sp, CFSTR("ManagedSpaceID"));
 if (idn) CFNumberGetValue(idn, kCFNumberSInt64Type, &sid);
 if (sid != 0 && sid != *outCurrent) {
 *outTarget = sid;
 break;
 }
 }
 }
 }
 }

 CFRelease(displays);
 return 0;
}

// dw_ws_set_space switches the given display's current Space.
// Returns 0 on success.
int dw_ws_set_space(const char *uuidUTF8, uint64_t spaceID) {
 if (!g_main || !g_set || !uuidUTF8) return 1;
 CFStringRef uuid = CFStringCreateWithCString(NULL, uuidUTF8, kCFStringEncodingUTF8);
 if (!uuid) return 2;
 g_set(g_main, uuid, (CGSSpaceID)spaceID);
 CFRelease(uuid);
 return 0;
}

// dw_ws_count_visible_windows_for_pid counts normal-layer windows owned by
// pid that are currently visible (i.e. on the active Space). Uses public
// CoreGraphics CGWindowListCopyWindowInfo — no SkyLight private API, no
// Screen Recording permission required for PID/bounds/layer (only window
// titles need permission, which we don't read).
//
// Filtering:
// - kCGWindowListOptionOnScreenOnly: only windows on the active Space
// - ownerPID == pid: belong to spawned Chrome
// - layer == 0: normal app windows (filter out menubar/dock/popups)
// - bounds.width >= 200 && height >= 200: filter zero-size hidden windows
//
// Returns count (0 if no qualifying window yet, ≥1 once Chrome window
// is fully composited and bound to current Space).
int dw_ws_count_visible_windows_for_pid(int pid) {
 CFArrayRef windows = CGWindowListCopyWindowInfo(
 kCGWindowListOptionOnScreenOnly | kCGWindowListExcludeDesktopElements
 kCGNullWindowID);
 if (!windows) return 0;
 int count = 0;
 CFIndex n = CFArrayGetCount(windows);
 for (CFIndex i = 0; i < n; i++) {
 CFDictionaryRef w = (CFDictionaryRef)CFArrayGetValueAtIndex(windows, i);

 CFNumberRef ownerRef = (CFNumberRef)CFDictionaryGetValue(w, kCGWindowOwnerPID);
 if (!ownerRef) continue;
 int owner = 0;
 CFNumberGetValue(ownerRef, kCFNumberIntType, &owner);
 if (owner != pid) continue;

 // Layer 0 = normal app window (Chrome browser windows).
 // Layer != 0: menubar, dock, popups — skip.
 CFNumberRef layerRef = (CFNumberRef)CFDictionaryGetValue(w, kCGWindowLayer);
 int layer = 1;
 if (layerRef) CFNumberGetValue(layerRef, kCFNumberIntType, &layer);
 if (layer != 0) continue;

 // bounds.width/height threshold filters Chrome's helper/zero-size windows.
 CFDictionaryRef boundsDict = (CFDictionaryRef)CFDictionaryGetValue(w, kCGWindowBounds);
 if (boundsDict) {
 CGRect r;
 if (CGRectMakeWithDictionaryRepresentation(boundsDict, &r)) {
 if (r.size.width < 200 || r.size.height < 200) continue;
 }
 }
 count++;
 }
 CFRelease(windows);
 return count;
}
*/
import "C"

import (
	"fmt"
	"log"
	"sync"
	"time"
	"unsafe"
)

// viewShiftTimeout bounds the wait for macOS to (a) create the new fullscreen
// Space for Chrome and (b) auto-shift view to it. Cold-start Chrome + fullscreen
// animation typically completes within 2-3s; 8s tolerates slow first-launch.
const viewShiftTimeout = 8 * time.Second

// viewShiftPollInterval — 30ms aggressive polling. Switchback latency is
// dominated by this interval (after view actually shifts). Must be fast enough
// that Human barely sees the fullscreen Space before view snaps back.
const viewShiftPollInterval = 30 * time.Millisecond

// visibleWindowSettleDelay gives WindowServer a short beat after switchback
// before we enforce the "Chrome must not be on Human's current Space" postcondition.
const visibleWindowSettleDelay = 250 * time.Millisecond

// visibleWindowCheckWindow is intentionally bounded: a visible Chrome that
// remains on the restored Space means the trusted/headed path would disturb the
// Human and must fail instead of pretending isolation succeeded.
const visibleWindowCheckWindow = 2500 * time.Millisecond

const visibleWindowPollInterval = 75 * time.Millisecond

// darwinWorkspace implements Workspace for macOS via SkyLight (D1 strategy).
// [darwin-specific bug class: ]
type darwinWorkspace struct {
	mu sync.Mutex
	loaded bool
	displayUUID string // first display's UUID (CFString → UTF-8)
	currentSp int64 // Space the Human is on (we switch back to this)
	targetSp int64 // Space we launch Chrome into
}

// NewWorkspace returns a macOS-native Workspace using SkyLight private APIs.
func NewWorkspace Workspace {
	return &darwinWorkspace{}
}

// ensureLoaded loads SkyLight + populates UUID + spaces (idempotent).
func (w *darwinWorkspace) ensureLoaded error {
	if w.loaded {
		return nil
	}
	if rc := C.dw_ws_init; rc != 0 {
		return fmt.Errorf("workspace: SkyLight dlopen failed (code %d)", rc)
	}

	if err := w.refreshSpaceSnapshotLocked; err != nil {
		return err
	}
	w.loaded = true
	log.Printf("[WORKSPACE-OSX] display=%s current=%d (V2 fullscreen strategy)", w.displayUUID, w.currentSp)
	return nil
}

// refreshSpaceSnapshotLocked samples the Human's current Space at launch time.
// Caching this only once is wrong: the Human may move Deepwork to another Space
// between launches, and the switchback target must be the Space they are
// actually using now.
func (w *darwinWorkspace) refreshSpaceSnapshotLocked error {
	var uuidBuf [128]C.char
	var current, target C.uint64_t
	rc := C.dw_ws_inspect(&uuidBuf[0], C.int(len(uuidBuf)), &current, &target)
	if rc != 0 {
		return fmt.Errorf("workspace: SLSCopyManagedDisplaySpaces failed (code %d)", rc)
	}
	uuid := C.GoString(&uuidBuf[0])
	if uuid == "" {
		return fmt.Errorf("workspace: no display UUID found")
	}
	// V2: target Space no longer required — --start-fullscreen creates its own
	// fullscreen Space deterministically. We still capture target opportunistically
	// for diagnostics but do not fail if Human has only one Space.
	w.displayUUID = uuid
	w.currentSp = int64(current)
	w.targetSp = int64(target) // may be 0; not used by V2 strategy
	return nil
}

// EnsureSpace populates the cached UUID + Space IDs and returns target.
func (w *darwinWorkspace) EnsureSpace (int64, error) {
	w.mu.Lock
	defer w.mu.Unlock
	if err := w.ensureLoaded; err != nil {
		return 0, err
	}
	return w.targetSp, nil
}

// setSpace switches the cached display's view to spaceID.
func (w *darwinWorkspace) setSpace(spaceID int64) error {
	cstr := C.CString(w.displayUUID)
	defer C.free(unsafe.Pointer(cstr))
	if rc := C.dw_ws_set_space(cstr, C.uint64_t(spaceID)); rc != 0 {
		return fmt.Errorf("workspace: SLSManagedDisplaySetCurrentSpace failed (code %d)", rc)
	}
	return nil
}

// LaunchChromeInSpace forks Chrome with --start-fullscreen so macOS gives it
// a dedicated fullscreen Space. We then race to switch the Human's view back
// to the original Space as soon as macOS shifts view to Chrome's new Space.
//
// Sequence (V2):
// 1. Capture original Space ID (Human's current Space).
// 2. Fork Chrome via startChromeProcess. chrome_launcher.go has already
// appended --start-fullscreen for darwin human mode.
// 3. Race loop: poll dw_ws_inspect every 30ms.
// - As soon as currentSpace != original → fire SLSManagedDisplaySetCurrentSpace
// to snap view back. Total switchback latency ≈ animation start +
// ≤30ms detection + 1 syscall.
// - If view never shifts within viewShiftTimeout, log warning and force a
// switchback anyway (defensive — rare cases where Chrome failed to enter
// fullscreen, e.g. user denied window permission).
// 4. Chrome lives on in its own fullscreen Space; CDP is fully usable from
// the now-restored Human view (input/keyboard go via CDP, not pixels).
//
// On launch failure: the partial Chrome process (if any) is already killed
// inside startChromeProcess. We do NOT need a switchback (no view shift
// occurred without Chrome's fullscreen Space).
//
//
func (w *darwinWorkspace) LaunchChromeInSpace(spec ChromeLaunchSpec) (ChromeHandle, error) {
	w.mu.Lock
	if err := w.ensureLoaded; err != nil {
		w.mu.Unlock
		return nil, err
	}
	if err := w.refreshSpaceSnapshotLocked; err != nil {
		w.mu.Unlock
		return nil, err
	}
	original := w.currentSp
	displayUUID := w.displayUUID
	w.mu.Unlock
	log.Printf("[WORKSPACE-OSX] launch request captured current Space %d on display=%s", original, displayUUID)

	// Step 1: fork Chrome (own-process exec; --start-fullscreen already in args).
	h, launchErr := startChromeProcess(spec)
	if launchErr != nil {
		return nil, launchErr
	}

	// Step 2: race to detect view shift to Chrome's fullscreen Space, then snap back.
	t0 := time.Now
	shifted, shiftedTo, waitErr := w.waitViewShifted(original, viewShiftTimeout)
	switch {
	case waitErr != nil && shifted:
		// shouldn't happen (waitViewShifted returns nil err when shifted); be defensive.
		log.Printf("[WORKSPACE-OSX] WARNING: ambiguous shift state: %v", waitErr)
	case !shifted:
		log.Printf("[WORKSPACE-OSX] WARNING: view did not shift away from Space %d within %v (pid=%d) — Chrome may not have entered fullscreen", original, viewShiftTimeout, h.pid)
	default:
		log.Printf("[WORKSPACE-OSX] view shifted %d → %d after %dms (pid=%d), snapping back", original, shiftedTo, time.Since(t0).Milliseconds, h.pid)
	}

	// Step 3: switch view back to original (always — defensive even if no shift detected).
	if backErr := w.setSpace(original); backErr != nil {
		log.Printf("[WORKSPACE-OSX] WARNING: switch back to Space %d failed: %v", original, backErr)
	} else {
		log.Printf("[WORKSPACE-OSX] view back to Space %d (total: %dms)", original, time.Since(t0).Milliseconds)
	}

	if err := w.verifyChromeHiddenFromCurrentSpace(h.pid, original, time.Since(t0)); err != nil {
		_ = h.Kill
		return nil, err
	}
	return h, nil
}

// waitViewShifted polls dw_ws_inspect every viewShiftPollInterval until the
// current Space differs from `original` (i.e. macOS auto-shifted view to
// Chrome's new fullscreen Space), or until timeout.
//
// Returns (shifted=true, newSpaceID, nil) on success; (false, 0, err) on timeout.
// Switchback latency is dominated by viewShiftPollInterval — keep it small.
func (w *darwinWorkspace) waitViewShifted(original int64, timeout time.Duration) (bool, int64, error) {
	deadline := time.Now.Add(timeout)
	attempts := 0
	for {
		attempts++
		var uuidBuf [128]C.char
		var current, target C.uint64_t
		if rc := C.dw_ws_inspect(&uuidBuf[0], C.int(len(uuidBuf)), &current, &target); rc == 0 {
			cur := int64(current)
			if cur != 0 && cur != original {
				return true, cur, nil
			}
		}
		if time.Now.After(deadline) {
			return false, 0, fmt.Errorf("no view shift from Space %d within %v (%d polls)", original, timeout, attempts)
		}
		time.Sleep(viewShiftPollInterval)
	}
}

// verifyChromeHiddenFromCurrentSpace enforces the human-facing contract of the
// darwin headed path: after switchback, the spawned Chrome must not own a
// visible normal-layer window on the active Space. If it does, killing Chrome is
// less surprising than silently leaving a full browser over Deepwork.
func (w *darwinWorkspace) verifyChromeHiddenFromCurrentSpace(pid int, original int64, elapsed time.Duration) error {
	time.Sleep(visibleWindowSettleDelay)
	deadline := time.Now.Add(visibleWindowCheckWindow)
	lastCount := 0
	polls := 0
	for {
		polls++
		_ = w.setSpace(original)
		count := int(C.dw_ws_count_visible_windows_for_pid(C.int(pid)))
		lastCount = count
		if time.Now.After(deadline) {
			if count > 0 {
				return fmt.Errorf("workspace: Chrome pid %d has %d visible window(s) on restored Space %d after %dms", pid, count, original, elapsed.Milliseconds)
			}
			log.Printf("[WORKSPACE-OSX] verified Chrome hidden from current Space pid=%d space=%d polls=%d visible_windows=%d", pid, original, polls, lastCount)
			return nil
		}
		time.Sleep(visibleWindowPollInterval)
	}
}

// Close releases cached state. The isolation Space itself persists.
func (w *darwinWorkspace) Close error {
	w.mu.Lock
	defer w.mu.Unlock
	w.loaded = false
	w.displayUUID = ""
	w.currentSp = 0
	w.targetSp = 0
	return nil
}
