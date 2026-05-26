//go:build darwin

package browser

/*
#cgo LDFLAGS: -framework CoreFoundation -framework CoreGraphics -framework ApplicationServices

#include <stdint.h>

typedef struct {
	int windowID;
	int pid;
	int x;
	int y;
	int w;
	int h;
	int targetX;
	int targetY;
	int targetW;
	int targetH;
	int moved;
	int reason;
	double virtualRatio;
	char owner[128];
	char title[256];
} dw_vd_rescue_record;

// C declarations for functions implemented in virtual_display_darwin_bridge.m.
// The CGo preamble is compiled as C (not Objective-C), so we use plain C
// prototypes here. The actual Objective-C implementation lives in the .m file,
// which CGo compiles separately with clang -x objective-c.

void *dw_vd_create(int width, int height, uint32_t *outDisplayID);
int   dw_vd_bounds(uint32_t displayID, int *outX, int *outY, int *outW, int *outH);
int   dw_vd_quarantine(uint32_t displayID);
int   dw_vd_inspect_windows_for_pid(int pid, uint32_t displayID, int *outTotal, int *outOutside);
int   dw_vd_count_windows_outside_display(int pid, uint32_t displayID);
int   dw_vd_rescue_foreign_windows(uint32_t displayID, const int *protectedPIDs, int protectedPIDCount, dw_vd_rescue_record *records, int maxRecords, int *outDisplayID, int *outScanned, int *outMatched, int *outMoved, int *outSkipped);
void  dw_vd_destroy(void *displayRef);
*/
import "C"

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

const virtualDisplayOrphanedSentinel = uintptr(1)
const virtualDisplayRescueRecordLimit = 96
const virtualDisplayContainmentStablePolls = 2

// virtualDisplayBoundsRetryAttempts / virtualDisplayBoundsRetryDelay: macOS
// updates CGDirectDisplay bounds asynchronously after dw_vd_quarantine. Three
// retries at 300 ms cover the observed 200-400 ms lag on M1/M2 hardware.
const virtualDisplayBoundsRetryAttempts = 3
const virtualDisplayBoundsRetryDelay = 300 * time.Millisecond

func RescueForeignWindowsFromVirtualDisplay() (*VirtualDisplayWindowRescueResult, error) {
	records := make([]C.dw_vd_rescue_record, virtualDisplayRescueRecordLimit)
	protectedPIDs := activeHeadedBrowserRuntimePIDs()
	protected := make([]C.int, 0, len(protectedPIDs))
	for _, pid := range protectedPIDs {
		protected = append(protected, C.int(pid))
	}
	var protectedPtr *C.int
	if len(protected) > 0 {
		protectedPtr = &protected[0]
	}
	var displayID C.int
	var scanned, matched, moved, skipped C.int
	rc := C.dw_vd_rescue_foreign_windows(0, protectedPtr, C.int(len(protected)), &records[0], C.int(len(records)), &displayID, &scanned, &matched, &moved, &skipped)
	result := &VirtualDisplayWindowRescueResult{
		Platform:             "darwin",
		DisplayID:            uint32(displayID),
		ProtectedBrowserPIDs: protectedPIDs,
		Scanned:              int(scanned),
		Matched:              int(matched),
		Moved:                int(moved),
		Skipped:              int(skipped),
		Windows:              make([]VirtualDisplayWindowRescueRecord, 0, min(int(matched), len(records))),
	}
	if rc == 1 {
		result.UnavailableReason = "no_deepwork_virtual_display"
		return result, nil
	}
	if rc != 0 {
		return result, fmt.Errorf("virtual_display: rescue foreign windows failed rc=%d", int(rc))
	}
	limit := min(int(matched), len(records))
	for i := 0; i < limit; i++ {
		rec := records[i]
		result.Windows = append(result.Windows, VirtualDisplayWindowRescueRecord{
			WindowID:     int(rec.windowID),
			PID:          int(rec.pid),
			Owner:        cCharArrayString(unsafe.Pointer(&rec.owner[0])),
			Title:        cCharArrayString(unsafe.Pointer(&rec.title[0])),
			X:            int(rec.x),
			Y:            int(rec.y),
			Width:        int(rec.w),
			Height:       int(rec.h),
			TargetX:      int(rec.targetX),
			TargetY:      int(rec.targetY),
			TargetWidth:  int(rec.targetW),
			TargetHeight: int(rec.targetH),
			Moved:        rec.moved != 0,
			Reason:       virtualDisplayRescueReason(int(rec.reason)),
			VirtualRatio: float64(rec.virtualRatio),
		})
	}
	return result, nil
}

func cCharArrayString(ptr unsafe.Pointer) string {
	if ptr == nil {
		return ""
	}
	return C.GoString((*C.char)(ptr))
}

func virtualDisplayRescueReason(reason int) string {
	switch reason {
	case 0:
		return "moved"
	case 1:
		return "accessibility_not_trusted"
	case 2:
		return "ax_window_not_found"
	case 3:
		return "ax_resize_failed"
	case 4:
		return "ax_move_failed"
	case 5:
		return "not_candidate"
	default:
		return fmt.Sprintf("unknown_%d", reason)
	}
}

// VirtualDisplayManager manages a single CGVirtualDisplay shared by all
// Chrome instances running in ModeHeaded on macOS.
//
// A single virtual display is sufficient: each Chrome instance receives a
// unique --window-position coordinate within the virtual display's coordinate
// space so windows don't perfectly overlap.
//
// The display is created lazily on the first Ensure() call and lives until
// Close() or process exit. All exported methods are goroutine-safe.
//
// Reference: DELTA-BS09-THREE-MODE §4, poc/macos-virtual-display/poc.m
type VirtualDisplayManager struct {
	mu        sync.Mutex
	display   unsafe.Pointer // retained CGVirtualDisplay* (via CFBridgingRetain)
	displayID uint32
	x, y      int // virtual display origin (left-top) in global screen coordinates
	w, h      int // virtual display dimensions

	// instanceCount is incremented atomically on each WindowPosition() call to
	// stagger multiple Chrome windows within the virtual display. 50 px steps
	// prevent complete overlap without exceeding the default viewport bounds for typical
	// use (≤20 concurrent instances).
	instanceCount int64
}

// Ensure idempotently creates the default-size virtual display.
// Subsequent calls are no-ops and return nil immediately.
//
// Sequence:
//  1. Call dw_vd_create → obtains retained CGVirtualDisplay* and displayID.
//  2. Sleep 1 s to allow WindowServer to register the new display in the
//     active display list (observed in PoC: ~1-2 s for registration).
//  3. Query bounds via dw_vd_bounds — fills (x, y, w, h) for WindowPosition.
func (vdm *VirtualDisplayManager) Ensure() error {
	startedAt := time.Now()
	vdm.mu.Lock()
	defer vdm.mu.Unlock()

	fastPathStartedAt := time.Now()
	if vdm.display != nil && vdm.displayID != 0 {
		// refreshBoundsLocked updates vdm.x/y/w/h — not just a presence check.
		// Using displayBoundsLocked (query-only) here would leave stale (0,0)
		// coords if the first Ensure() failed to read bounds.
		if err := vdm.refreshBoundsLocked(); err == nil {
			log.Printf("[VIRTUAL-DISPLAY] ensure fast-path displayID=%d x=%d y=%d w=%d h=%d elapsed_ms=%d bounds_ms=%d",
				vdm.displayID, vdm.x, vdm.y, vdm.w, vdm.h,
				time.Since(startedAt).Milliseconds(), time.Since(fastPathStartedAt).Milliseconds())
			return nil
		}
		// Bounds unavailable — display may have been dropped by macOS; fall through to recreate.
		log.Printf("[VIRTUAL-DISPLAY] fast-path bounds failed for displayID=%d, recreating", vdm.displayID)
	}
	vdm.destroyDisplayLocked()
	vdm.resetLocked()

	log.Printf("[VIRTUAL-DISPLAY] creating %dx%d CGVirtualDisplay", DefaultViewportWidth, DefaultViewportHeight)

	var cDisplayID C.uint32_t
	createStartedAt := time.Now()
	retained := C.dw_vd_create(DefaultViewportWidth, DefaultViewportHeight, &cDisplayID)
	createElapsed := time.Since(createStartedAt)
	if retained == nil {
		return fmt.Errorf("virtual_display: dw_vd_create returned nil — CGVirtualDisplay init failed")
	}
	if uint32(cDisplayID) == 0 {
		C.dw_vd_destroy(retained)
		return fmt.Errorf("virtual_display: dw_vd_create succeeded but displayID is 0")
	}

	// virtualDisplayOrphanedSentinel means we reused an already registered
	// display. We don't own the ObjC reference; dw_vd_destroy is a no-op.
	orphaned := uintptr(retained) == virtualDisplayOrphanedSentinel
	if orphaned {
		log.Printf("[VIRTUAL-DISPLAY] reusing orphaned virtual display displayID=%d (prior process did not clean up)", uint32(cDisplayID))
	}
	log.Printf("[VIRTUAL-DISPLAY] create completed displayID=%d orphaned=%t elapsed_ms=%d",
		uint32(cDisplayID), orphaned, createElapsed.Milliseconds())

	vdm.display = retained
	vdm.displayID = uint32(cDisplayID)

	if !orphaned {
		log.Printf("[VIRTUAL-DISPLAY] created displayID=%d, sleeping 1s for WindowServer registration", vdm.displayID)
		// Release the lock while sleeping so other goroutines don't stall.
		sleepStartedAt := time.Now()
		vdm.mu.Unlock()
		time.Sleep(VirtualDisplayRegistrationDelay)
		vdm.mu.Lock()
		log.Printf("[VIRTUAL-DISPLAY] registration delay completed displayID=%d elapsed_ms=%d",
			vdm.displayID, time.Since(sleepStartedAt).Milliseconds())
	}

	quarantineStartedAt := time.Now()
	if rc := C.dw_vd_quarantine(C.uint32_t(vdm.displayID)); rc != 0 {
		log.Printf("[VIRTUAL-DISPLAY] WARNING: quarantine placement failed displayID=%d rc=%d elapsed_ms=%d",
			vdm.displayID, int(rc), time.Since(quarantineStartedAt).Milliseconds())
	} else {
		log.Printf("[VIRTUAL-DISPLAY] quarantine placement completed displayID=%d elapsed_ms=%d",
			vdm.displayID, time.Since(quarantineStartedAt).Milliseconds())
	}

	boundsStartedAt := time.Now()
	var boundsErr error
	for attempt := 0; attempt < virtualDisplayBoundsRetryAttempts; attempt++ {
		if attempt > 0 {
			// Release lock while sleeping so other goroutines don't stall.
			vdm.mu.Unlock()
			time.Sleep(virtualDisplayBoundsRetryDelay)
			vdm.mu.Lock()
		}
		boundsErr = vdm.refreshBoundsLocked()
		if boundsErr == nil {
			break
		}
		log.Printf("[VIRTUAL-DISPLAY] bounds attempt %d/%d failed displayID=%d: %v",
			attempt+1, virtualDisplayBoundsRetryAttempts, vdm.displayID, boundsErr)
	}
	log.Printf("[VIRTUAL-DISPLAY] ensure completed displayID=%d total_elapsed_ms=%d bounds_elapsed_ms=%d",
		vdm.displayID, time.Since(startedAt).Milliseconds(), time.Since(boundsStartedAt).Milliseconds())
	if boundsErr != nil {
		// Fail-closed: (0,0) would place Chrome on the main screen. Surface the
		// error so the caller can abort rather than silently leaking a Chrome
		// window onto the user's workspace.
		vdm.destroyDisplayLocked()
		vdm.resetLocked()
		return fmt.Errorf("virtual_display: bounds unavailable after %d attempts (displayID %d): %w",
			virtualDisplayBoundsRetryAttempts, vdm.displayID, boundsErr)
	}
	return nil
}

// EnsurePresent recreates the virtual display if the current displayID no
// longer appears in WindowServer's online display list. This repairs the
// orphaned-display reuse path: BrowserMuxHost must keep Chrome and display as a
// valid pair even when macOS drops a previously reused display.
func (vdm *VirtualDisplayManager) EnsurePresent() error {
	vdm.mu.Lock()
	needsRepair := vdm.display == nil || vdm.displayID == 0 || vdm.displayBoundsLocked() != nil
	if needsRepair {
		log.Printf("[VIRTUAL-DISPLAY] display missing, recreating displayID=%d", vdm.displayID)
		vdm.destroyDisplayLocked()
		vdm.resetLocked()
	}
	vdm.mu.Unlock()
	if !needsRepair {
		return nil
	}
	return vdm.Ensure()
}

func (vdm *VirtualDisplayManager) refreshBoundsLocked() error {
	var cx, cy, cw, ch C.int
	if rc := C.dw_vd_bounds(C.uint32_t(vdm.displayID), &cx, &cy, &cw, &ch); rc != 0 || cw <= 0 || ch <= 0 {
		return fmt.Errorf("virtual_display: displayID %d bounds unavailable rc=%d size=%dx%d", vdm.displayID, int(rc), int(cw), int(ch))
	} else {
		vdm.x, vdm.y = int(cx), int(cy)
		vdm.w, vdm.h = int(cw), int(ch)
		log.Printf("[VIRTUAL-DISPLAY] bounds: origin=(%d,%d) size=(%dx%d)", vdm.x, vdm.y, vdm.w, vdm.h)
	}
	return nil
}

func (vdm *VirtualDisplayManager) displayBoundsLocked() error {
	var cx, cy, cw, ch C.int
	if rc := C.dw_vd_bounds(C.uint32_t(vdm.displayID), &cx, &cy, &cw, &ch); rc != 0 || cw <= 0 || ch <= 0 {
		return fmt.Errorf("displayID %d unavailable rc=%d size=%dx%d", vdm.displayID, int(rc), int(cw), int(ch))
	}
	return nil
}

func (vdm *VirtualDisplayManager) resetLocked() {
	vdm.display = nil
	vdm.displayID = 0
	vdm.x, vdm.y = 0, 0
	vdm.w, vdm.h = 0, 0
}

func (vdm *VirtualDisplayManager) destroyDisplayLocked() {
	if vdm.display != nil && uintptr(vdm.display) != virtualDisplayOrphanedSentinel {
		C.dw_vd_destroy(vdm.display)
	}
}

// WindowPosition returns an (x, y) coordinate within the virtual display
// suitable for Chrome's --window-position flag.
//
// Each call auto-increments an internal counter by 50 px so that multiple
// concurrent Chrome instances are staggered and don't perfectly overlap inside
// the virtual display. The offset wraps when it would exceed the display width.
func (vdm *VirtualDisplayManager) WindowPosition() (x, y int) {
	n := int(atomic.AddInt64(&vdm.instanceCount, 1) - 1)
	const step = 50

	vdm.mu.Lock()
	vx, vy, vw, vh := vdm.x, vdm.y, vdm.w, vdm.h
	vdm.mu.Unlock()

	offset := n * step
	if vw > 0 && offset+step > vw {
		offset = offset % vw
	}
	if vh > 0 && offset+step > vh {
		offset = offset % vh
	}
	return vx + offset, vy + offset
}

// WindowPosition returns (offset, offset) — exported helper for tests that
// need a deterministic position without consuming an instance slot.
//
// Deprecated: prefer WindowPosition() for production use.
func (vdm *VirtualDisplayManager) WindowPositionAt(offset int) (x, y int) {
	vdm.mu.Lock()
	defer vdm.mu.Unlock()
	return vdm.x + offset, vdm.y + offset
}

// DisplayID returns the CGDirectDisplayID of the virtual display.
// Returns 0 if the display has not yet been created via Ensure().
func (vdm *VirtualDisplayManager) DisplayID() uint32 {
	vdm.mu.Lock()
	defer vdm.mu.Unlock()
	return vdm.displayID
}

func (vdm *VirtualDisplayManager) CountWindowsOutsideDisplay(pid int) int {
	vdm.mu.Lock()
	displayID := vdm.displayID
	vdm.mu.Unlock()
	if pid <= 0 || displayID == 0 {
		return 0
	}
	return int(C.dw_vd_count_windows_outside_display(C.int(pid), C.uint32_t(displayID)))
}

func (vdm *VirtualDisplayManager) inspectWindowsForPID(pid int) (total int, outside int, err error) {
	vdm.mu.Lock()
	displayID := vdm.displayID
	vdm.mu.Unlock()
	if pid <= 0 {
		return 0, 0, fmt.Errorf("virtual_display: invalid Chrome pid %d", pid)
	}
	if displayID == 0 {
		return 0, 0, fmt.Errorf("virtual_display: display is not active")
	}
	var cTotal, cOutside C.int
	if rc := C.dw_vd_inspect_windows_for_pid(C.int(pid), C.uint32_t(displayID), &cTotal, &cOutside); rc != 0 {
		return 0, 0, fmt.Errorf("virtual_display: inspect Chrome windows failed rc=%d", int(rc))
	}
	return int(cTotal), int(cOutside), nil
}

func (vdm *VirtualDisplayManager) VerifyChromeContained(pid int, timeout time.Duration) error {
	startedAt := time.Now()
	if timeout <= 0 {
		timeout = BrowserMuxHostWindowContainmentTimeout
	}
	vdm.mu.Lock()
	displayID := vdm.displayID
	vdm.mu.Unlock()
	if pid <= 0 {
		return fmt.Errorf("virtual_display: invalid Chrome pid %d", pid)
	}
	if displayID == 0 {
		return fmt.Errorf("virtual_display: display is not active")
	}
	var cx, cy, cw, ch C.int
	if rc := C.dw_vd_bounds(C.uint32_t(displayID), &cx, &cy, &cw, &ch); rc != 0 || cw <= 0 || ch <= 0 {
		return fmt.Errorf("virtual_display: displayID %d is not present in active display list", displayID)
	}

	deadline := time.Now().Add(timeout)
	polls := 0
	lastTotal := 0
	lastOutside := 0
	stable := 0
	for {
		polls++
		total, outside, err := vdm.inspectWindowsForPID(pid)
		if err != nil {
			return err
		}
		lastTotal, lastOutside = total, outside
		if total > 0 && outside == 0 {
			stable++
			if stable >= virtualDisplayContainmentStablePolls {
				log.Printf("[VIRTUAL-DISPLAY] verified Chrome windows contained pid=%d windows=%d outside=%d polls=%d stable_polls=%d elapsed_ms=%d",
					pid, total, outside, polls, stable, time.Since(startedAt).Milliseconds())
				return nil
			}
		} else {
			stable = 0
		}
		if time.Now().After(deadline) {
			if lastOutside > 0 {
				return fmt.Errorf("virtual_display: Chrome pid %d has %d/%d window(s) outside CGVirtualDisplay after %d polls", pid, lastOutside, lastTotal, polls)
			}
			if lastTotal == 0 {
				return fmt.Errorf("virtual_display: Chrome pid %d has no normal on-screen window after %d polls", pid, polls)
			}
			log.Printf("[VIRTUAL-DISPLAY] verified Chrome windows contained pid=%d windows=%d outside=%d polls=%d stable_polls=%d elapsed_ms=%d",
				pid, lastTotal, lastOutside, polls, stable, time.Since(startedAt).Milliseconds())
			return nil
		}
		time.Sleep(VirtualDisplayContainmentPollInterval)
	}
}

// Close releases the CGVirtualDisplay. WindowServer automatically removes the
// virtual display from the active display list once the last reference is
// dropped. Safe to call multiple times.
func (vdm *VirtualDisplayManager) Close() error {
	vdm.mu.Lock()
	defer vdm.mu.Unlock()

	if vdm.display == nil {
		return nil
	}

	if uintptr(vdm.display) == virtualDisplayOrphanedSentinel {
		log.Printf("[VIRTUAL-DISPLAY] releasing reused display handle displayID=%d", vdm.displayID)
	} else {
		log.Printf("[VIRTUAL-DISPLAY] destroying displayID=%d", vdm.displayID)
		vdm.destroyDisplayLocked()
	}
	vdm.resetLocked()
	return nil
}
