// virtual_display_bridge_darwin.m — Objective-C implementation for CGVirtualDisplay.
// Compiled by CGo as a separate translation unit (clang -x objective-c).
// All functions are declared in virtual_display_darwin.go's CGo preamble.
// Compiled with -fno-objc-arc (set via cgo directive) — manual retain/release.

#import <Foundation/Foundation.h>
#import <CoreGraphics/CoreGraphics.h>
#import <ApplicationServices/ApplicationServices.h>
#import <float.h>
#import <math.h>
#import <unistd.h>

#define DW_VD_MAX_DISPLAY_PROBE 64
#define DW_VD_DIMENSION_TOLERANCE 2
#define DW_VD_VENDOR_ID 0x1234
#define DW_VD_PRODUCT_ID 0x5678
#define DW_VD_SERIAL_NUM 0x0001
#define DW_VD_ORPHANED_DISPLAY_SENTINEL ((void *)1)
#define DW_VD_QUARANTINE_GAP 80
#define DW_VD_RESCUE_MIN_WIDTH 240
#define DW_VD_RESCUE_MIN_HEIGHT 180
#define DW_VD_RESCUE_MIN_RATIO 0.80
#define DW_VD_RESCUE_MARGIN 80
#define DW_VD_RESCUE_CASCADE 32

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

enum {
    DW_VD_RESCUE_REASON_MOVED = 0,
    DW_VD_RESCUE_REASON_AX_NOT_TRUSTED = 1,
    DW_VD_RESCUE_REASON_AX_WINDOW_NOT_FOUND = 2,
    DW_VD_RESCUE_REASON_AX_RESIZE_FAILED = 3,
    DW_VD_RESCUE_REASON_AX_MOVE_FAILED = 4,
    DW_VD_RESCUE_REASON_NOT_CANDIDATE = 5
};

// ─── CGVirtualDisplay private API forward declarations ─────────────────────
// These classes exist in CoreGraphics.framework on macOS 11.0+ (Big Sur+).
// The @interface declarations expose the public surface; ObjC runtime resolves
// the implementations via the existing framework dylib.

@interface CGVirtualDisplayMode : NSObject
- (instancetype)initWithWidth:(NSInteger)w height:(NSInteger)h refreshRate:(double)rate;
@property (readonly) NSInteger width;
@property (readonly) NSInteger height;
@property (readonly) double refreshRate;
@end

@interface CGVirtualDisplaySettings : NSObject
@property (nonatomic, copy) NSArray *modes;
@property (nonatomic) BOOL hiDPI;
@end

@interface CGVirtualDisplayDescriptor : NSObject
@property (nonatomic, copy) NSString *name;
@property (nonatomic) NSInteger maxPixelsWide;
@property (nonatomic) NSInteger maxPixelsHigh;
@property (nonatomic) CGSize sizeInMillimeters;
@property (nonatomic) uint32_t vendorID;
@property (nonatomic) uint32_t productID;
@property (nonatomic) uint32_t serialNum;
@property (nonatomic, copy) dispatch_queue_t queue;
@end

@interface CGVirtualDisplay : NSObject
- (instancetype)initWithDescriptor:(CGVirtualDisplayDescriptor *)descriptor;
- (BOOL)applySettings:(CGVirtualDisplaySettings *)settings;
@property (readonly) CGDirectDisplayID displayID;
@property (readonly, copy) NSString *name;
@end

static CGDirectDisplayID dw_vd_prior_display_ids[DW_VD_MAX_DISPLAY_PROBE];
static CGRect dw_vd_prior_display_bounds[DW_VD_MAX_DISPLAY_PROBE];
static uint32_t dw_vd_prior_display_count = 0;
static int dw_vd_prior_layout_valid = 0;

static double dw_vd_now_ms(void) {
    return CFAbsoluteTimeGetCurrent() * 1000.0;
}

// dw_vd_find_existing_display_id scans the online display list for the
// Deepwork virtual display identity. Online, not active, is intentional:
// macOS reports all displays inactive when the physical displays are asleep,
// while the virtual display is still present and safe to reuse.
//
// Used as a fallback when CGVirtualDisplay alloc-init fails because an orphaned
// virtual display from a prior process is still registered with WindowServer.
// In that case we cannot create a new one, but we can reuse the existing slot.
static int dw_vd_dimension_matches(CGFloat actual, int expected) {
    CGFloat diff = actual - (CGFloat)expected;
    if (diff < 0) diff = -diff;
    return diff <= (CGFloat)DW_VD_DIMENSION_TOLERANCE;
}

static int dw_vd_identity_matches(CGDirectDisplayID displayID) {
    return CGDisplayVendorNumber(displayID) == DW_VD_VENDOR_ID &&
           CGDisplayModelNumber(displayID) == DW_VD_PRODUCT_ID &&
           CGDisplaySerialNumber(displayID) == DW_VD_SERIAL_NUM;
}

static void dw_vd_capture_prior_layout(void) {
    dw_vd_prior_display_count = 0;
    dw_vd_prior_layout_valid = 0;

    CGDirectDisplayID displays[DW_VD_MAX_DISPLAY_PROBE];
    uint32_t count = 0;
    if (CGGetActiveDisplayList(DW_VD_MAX_DISPLAY_PROBE, displays, &count) != kCGErrorSuccess) {
        return;
    }
    if (count > DW_VD_MAX_DISPLAY_PROBE) count = DW_VD_MAX_DISPLAY_PROBE;
    for (uint32_t i = 0; i < count; i++) {
        dw_vd_prior_display_ids[i] = displays[i];
        dw_vd_prior_display_bounds[i] = CGDisplayBounds(displays[i]);
    }
    dw_vd_prior_display_count = count;
    dw_vd_prior_layout_valid = 1;
}

static int dw_vd_prior_layout_contains_display(CGDirectDisplayID displayID) {
    if (!dw_vd_prior_layout_valid) return 0;
    for (uint32_t i = 0; i < dw_vd_prior_display_count; i++) {
        if (dw_vd_prior_display_ids[i] == displayID) return 1;
    }
    return 0;
}

static CGRect dw_vd_physical_union_from_current(CGDirectDisplayID virtualID, int *hasPhysical) {
    *hasPhysical = 0;
    CGRect unionBounds = CGRectZero;
    CGDirectDisplayID displays[DW_VD_MAX_DISPLAY_PROBE];
    uint32_t count = 0;
    if (CGGetActiveDisplayList(DW_VD_MAX_DISPLAY_PROBE, displays, &count) != kCGErrorSuccess) {
        return unionBounds;
    }
    for (uint32_t i = 0; i < count; i++) {
        CGDirectDisplayID id = displays[i];
        if (id == virtualID || dw_vd_identity_matches(id)) continue;
        CGRect b = CGDisplayBounds(id);
        if (!*hasPhysical) {
            unionBounds = b;
            *hasPhysical = 1;
        } else {
            unionBounds = CGRectUnion(unionBounds, b);
        }
    }
    return unionBounds;
}

static CGRect dw_vd_physical_union_from_prior(CGDirectDisplayID virtualID, int *hasPhysical) {
    *hasPhysical = 0;
    CGRect unionBounds = CGRectZero;
    if (!dw_vd_prior_layout_valid) {
        return unionBounds;
    }
    for (uint32_t i = 0; i < dw_vd_prior_display_count; i++) {
        CGDirectDisplayID id = dw_vd_prior_display_ids[i];
        if (id == virtualID || dw_vd_identity_matches(id)) continue;
        CGRect b = dw_vd_prior_display_bounds[i];
        if (!*hasPhysical) {
            unionBounds = b;
            *hasPhysical = 1;
        } else {
            unionBounds = CGRectUnion(unionBounds, b);
        }
    }
    return unionBounds;
}

static int dw_vd_configure_quarantine(CGDirectDisplayID virtualID, int restorePrior) {
    if (virtualID == 0) return 1;
    double totalStart = dw_vd_now_ms();

    int hasPhysical = 0;
    double unionStart = dw_vd_now_ms();
    CGRect unionBounds = restorePrior
        ? dw_vd_physical_union_from_prior(virtualID, &hasPhysical)
        : dw_vd_physical_union_from_current(virtualID, &hasPhysical);
    if (!hasPhysical) {
        unionBounds = CGDisplayBounds(CGMainDisplayID());
        hasPhysical = 1;
    }
    double unionMS = dw_vd_now_ms() - unionStart;

    int quarantineX = (int)CGRectGetMinX(unionBounds);
    int quarantineY = (int)CGRectGetMaxY(unionBounds) + DW_VD_QUARANTINE_GAP;
    CGRect currentBounds = CGDisplayBounds(virtualID);
    if (!restorePrior && !CGRectIsEmpty(currentBounds) && !CGRectIntersectsRect(currentBounds, unionBounds)) {
        fprintf(stderr, "[VIRTUAL-DISPLAY] quarantine timings path=already_quarantined displayID=%u restore_prior=%d current_origin=(%.0f,%.0f) current_size=(%.0fx%.0f) physical_union=(%.0f,%.0f,%.0fx%.0f) rc=0 physical_union_ms=%.1f total_ms=%.1f\n",
                (uint32_t)virtualID, restorePrior,
                currentBounds.origin.x, currentBounds.origin.y, currentBounds.size.width, currentBounds.size.height,
                unionBounds.origin.x, unionBounds.origin.y, unionBounds.size.width, unionBounds.size.height,
                unionMS, dw_vd_now_ms() - totalStart);
        return 0;
    }

    CGDisplayConfigRef config = NULL;
    double beginStart = dw_vd_now_ms();
    if (CGBeginDisplayConfiguration(&config) != kCGErrorSuccess || config == NULL) {
        fprintf(stderr, "[VIRTUAL-DISPLAY] quarantine timings displayID=%u restore_prior=%d rc=2 physical_union_ms=%.1f begin_ms=%.1f total_ms=%.1f\n",
                (uint32_t)virtualID, restorePrior, unionMS, dw_vd_now_ms() - beginStart, dw_vd_now_ms() - totalStart);
        return 2;
    }
    double beginMS = dw_vd_now_ms() - beginStart;

    CGError err = kCGErrorSuccess;
    double restoreStart = dw_vd_now_ms();
    int restoreCount = 0;
    if (restorePrior && dw_vd_prior_layout_valid) {
        for (uint32_t i = 0; i < dw_vd_prior_display_count; i++) {
            CGDirectDisplayID id = dw_vd_prior_display_ids[i];
            if (id == virtualID || dw_vd_identity_matches(id)) continue;
            CGRect b = dw_vd_prior_display_bounds[i];
            restoreCount++;
            CGError e = CGConfigureDisplayOrigin(config, id, (int32_t)b.origin.x, (int32_t)b.origin.y);
            if (e != kCGErrorSuccess) err = e;
        }
    }
    double restoreMS = dw_vd_now_ms() - restoreStart;

    double virtualOriginStart = dw_vd_now_ms();
    CGError e = CGConfigureDisplayOrigin(config, virtualID, (int32_t)quarantineX, (int32_t)quarantineY);
    if (e != kCGErrorSuccess) err = e;
    double virtualOriginMS = dw_vd_now_ms() - virtualOriginStart;

    double completeStart = dw_vd_now_ms();
    CGError complete = CGCompleteDisplayConfiguration(config, kCGConfigureForSession);
    double completeMS = dw_vd_now_ms() - completeStart;
    int rc = 0;
    if (complete != kCGErrorSuccess) rc = 3;
    else if (err != kCGErrorSuccess) rc = 4;
    fprintf(stderr, "[VIRTUAL-DISPLAY] quarantine timings displayID=%u restore_prior=%d restore_count=%d target_origin=(%d,%d) rc=%d configure_err=%d complete_err=%d physical_union_ms=%.1f begin_ms=%.1f restore_ms=%.1f virtual_origin_ms=%.1f complete_ms=%.1f total_ms=%.1f\n",
            (uint32_t)virtualID, restorePrior, restoreCount, quarantineX, quarantineY, rc, (int)err, (int)complete,
            unionMS, beginMS, restoreMS, virtualOriginMS, completeMS, dw_vd_now_ms() - totalStart);
    if (rc != 0) return rc;
    return 0;
}

static uint32_t dw_vd_find_existing_display_id(int width, int height) {
    CGDirectDisplayID displays[DW_VD_MAX_DISPLAY_PROBE];
    uint32_t count = 0;
    CGGetOnlineDisplayList(DW_VD_MAX_DISPLAY_PROBE, displays, &count);
    for (uint32_t i = 0; i < count; i++) {
        if (CGDisplayIsMain(displays[i])) continue; // skip main physical display
        if (!dw_vd_identity_matches(displays[i])) continue;
        CGRect b = CGDisplayBounds(displays[i]);
        if (dw_vd_dimension_matches(b.size.width, width) &&
            dw_vd_dimension_matches(b.size.height, height)) {
            return displays[i];
        }
    }
    return 0;
}

static uint32_t dw_vd_find_any_existing_display_id(void) {
    CGDirectDisplayID displays[DW_VD_MAX_DISPLAY_PROBE];
    uint32_t count = 0;
    CGGetOnlineDisplayList(DW_VD_MAX_DISPLAY_PROBE, displays, &count);
    for (uint32_t i = 0; i < count; i++) {
        if (CGDisplayIsMain(displays[i])) continue;
        if (!dw_vd_identity_matches(displays[i])) continue;
        return displays[i];
    }
    return 0;
}

static void *dw_vd_reuse_existing_if_available(int width, int height, uint32_t *outDisplayID) {
    uint32_t existingID = dw_vd_find_existing_display_id(width, height);
    if (existingID == 0) {
        return NULL;
    }
    *outDisplayID = existingID;
    return DW_VD_ORPHANED_DISPLAY_SENTINEL;
}

// ─── dw_vd_create ──────────────────────────────────────────────────────────
// Creates a CGVirtualDisplay with the given resolution. If creation fails
// because an existing virtual display with matching dimensions is already
// registered (orphaned from a prior process), falls back to reusing that
// display's ID.
//
// Returns a +1 retained pointer that Go must eventually pass to dw_vd_destroy.
// When the fallback path is taken (reuse of orphaned display), the returned
// pointer is a sentinel value (non-NULL, not a real ObjC object) and
// outDisplayID points at the reused display. dw_vd_destroy is a no-op for
// orphaned pointers.
//
// On total failure returns NULL; *outDisplayID is always set.
void *dw_vd_create(int width, int height, uint32_t *outDisplayID) {
    double totalStart = dw_vd_now_ms();
    *outDisplayID = 0;
    double priorStart = dw_vd_now_ms();
    dw_vd_capture_prior_layout();
    double priorMS = dw_vd_now_ms() - priorStart;

    @autoreleasepool {
        CGVirtualDisplayDescriptor *desc = [[CGVirtualDisplayDescriptor alloc] init];
        if (!desc) {
            void *fallback = dw_vd_reuse_existing_if_available(width, height, outDisplayID);
            fprintf(stderr, "[VIRTUAL-DISPLAY] create timings path=descriptor_failed fallback=%d displayID=%u prior_layout_ms=%.1f total_ms=%.1f\n",
                    fallback != NULL, *outDisplayID, priorMS, dw_vd_now_ms() - totalStart);
            return fallback;
        }

        desc.name              = @"DeepworkVirtualBrowser";
        desc.maxPixelsWide     = (NSInteger)width;
        desc.maxPixelsHigh     = (NSInteger)height;
        desc.sizeInMillimeters = CGSizeMake(530.0, 300.0); // ~24-inch at 96 dpi
        desc.vendorID          = DW_VD_VENDOR_ID;
        desc.productID         = DW_VD_PRODUCT_ID;
        desc.serialNum         = DW_VD_SERIAL_NUM;
        desc.queue             = dispatch_get_main_queue();

        CGVirtualDisplayMode *mode =
            [[CGVirtualDisplayMode alloc] initWithWidth:(NSInteger)width
                                                 height:(NSInteger)height
                                            refreshRate:60.0];
        if (!mode) {
            void *fallback = dw_vd_reuse_existing_if_available(width, height, outDisplayID);
            fprintf(stderr, "[VIRTUAL-DISPLAY] create timings path=mode_failed fallback=%d displayID=%u prior_layout_ms=%.1f total_ms=%.1f\n",
                    fallback != NULL, *outDisplayID, priorMS, dw_vd_now_ms() - totalStart);
            return fallback;
        }

        CGVirtualDisplaySettings *settings = [[CGVirtualDisplaySettings alloc] init];
        if (!settings) {
            void *fallback = dw_vd_reuse_existing_if_available(width, height, outDisplayID);
            fprintf(stderr, "[VIRTUAL-DISPLAY] create timings path=settings_failed fallback=%d displayID=%u prior_layout_ms=%.1f total_ms=%.1f\n",
                    fallback != NULL, *outDisplayID, priorMS, dw_vd_now_ms() - totalStart);
            return fallback;
        }
        settings.modes = @[mode];
        settings.hiDPI = NO;

        double initStart = dw_vd_now_ms();
        CGVirtualDisplay *display = [[CGVirtualDisplay alloc] initWithDescriptor:desc];
        double initMS = dw_vd_now_ms() - initStart;
        if (!display) {
            void *fallback = dw_vd_reuse_existing_if_available(width, height, outDisplayID);
            fprintf(stderr, "[VIRTUAL-DISPLAY] create timings path=init_failed fallback=%d displayID=%u prior_layout_ms=%.1f init_display_ms=%.1f total_ms=%.1f\n",
                    fallback != NULL, *outDisplayID, priorMS, initMS, dw_vd_now_ms() - totalStart);
            return fallback;
        }

        double applyStart = dw_vd_now_ms();
        if (![display applySettings:settings]) {
            double applyMS = dw_vd_now_ms() - applyStart;
            [display release];
            void *fallback = dw_vd_reuse_existing_if_available(width, height, outDisplayID);
            fprintf(stderr, "[VIRTUAL-DISPLAY] create timings path=apply_failed fallback=%d displayID=%u prior_layout_ms=%.1f init_display_ms=%.1f apply_settings_ms=%.1f total_ms=%.1f\n",
                    fallback != NULL, *outDisplayID, priorMS, initMS, applyMS, dw_vd_now_ms() - totalStart);
            return fallback;
        }
        double applyMS = dw_vd_now_ms() - applyStart;
        *outDisplayID = (uint32_t)display.displayID;
        if (*outDisplayID == 0) {
            [display release];
            void *fallback = dw_vd_reuse_existing_if_available(width, height, outDisplayID);
            fprintf(stderr, "[VIRTUAL-DISPLAY] create timings path=zero_display_id fallback=%d displayID=%u prior_layout_ms=%.1f init_display_ms=%.1f apply_settings_ms=%.1f total_ms=%.1f\n",
                    fallback != NULL, *outDisplayID, priorMS, initMS, applyMS, dw_vd_now_ms() - totalStart);
            return fallback;
        }

        // Retain so the object survives the autoreleasepool drain.
        // Balanced by [release] in dw_vd_destroy.
        [display retain];
        void *retained = (__bridge void *)display;
        fprintf(stderr, "[VIRTUAL-DISPLAY] create timings path=created displayID=%u prior_layout_ms=%.1f init_display_ms=%.1f apply_settings_ms=%.1f total_ms=%.1f\n",
                *outDisplayID, priorMS, initMS, applyMS, dw_vd_now_ms() - totalStart);
        return retained;
    }
}

// ─── dw_vd_bounds ──────────────────────────────────────────────────────────
// Queries CGDisplayBounds for displayID and fills the output pointers.
// Returns 0 on success, 1 if displayID is 0.
int dw_vd_bounds(uint32_t displayID, int *outX, int *outY, int *outW, int *outH) {
    if (displayID == 0) return 1;
    CGRect b = CGDisplayBounds((CGDirectDisplayID)displayID);
    *outX = (int)b.origin.x;
    *outY = (int)b.origin.y;
    *outW = (int)b.size.width;
    *outH = (int)b.size.height;
    return 0;
}

// ─── dw_vd_quarantine ─────────────────────────────────────────────────────
// Places the virtual display outside the Human's normal horizontal monitor
// path. When this process created the display, restore all pre-existing display
// origins captured before creation so the virtual display does not wedge
// itself between the built-in screen and a physical external screen.
int dw_vd_quarantine(uint32_t displayID) {
    if (displayID == 0) return 1;
    int restorePrior = dw_vd_prior_layout_valid && !dw_vd_prior_layout_contains_display((CGDirectDisplayID)displayID);
    return dw_vd_configure_quarantine((CGDirectDisplayID)displayID, restorePrior);
}

// ─── dw_vd_count_windows_outside_display ──────────────────────────────────
// Counts normal Chrome windows for pid that are currently on-screen but not
// fully contained by the virtual display bounds. This is the postcondition
// guard for macOS headed mode: if Chrome leaks to the Human's physical display,
// kill it instead of leaving an intrusive window behind.
int dw_vd_inspect_windows_for_pid(int pid, uint32_t displayID, int *outTotal, int *outOutside) {
    if (outTotal) *outTotal = 0;
    if (outOutside) *outOutside = 0;
    if (pid <= 0 || displayID == 0) return 1;
    CGRect vBounds = CGDisplayBounds((CGDirectDisplayID)displayID);
    CGRect allowed = CGRectInset(vBounds, -8.0, -8.0);

    CFArrayRef windows = CGWindowListCopyWindowInfo(
        kCGWindowListOptionOnScreenOnly | kCGWindowListExcludeDesktopElements,
        kCGNullWindowID);
    if (!windows) return 2;

    int total = 0;
    int outside = 0;
    CFIndex n = CFArrayGetCount(windows);
    for (CFIndex i = 0; i < n; i++) {
        CFDictionaryRef w = (CFDictionaryRef)CFArrayGetValueAtIndex(windows, i);

        CFNumberRef ownerRef = (CFNumberRef)CFDictionaryGetValue(w, kCGWindowOwnerPID);
        if (!ownerRef) continue;
        int owner = 0;
        CFNumberGetValue(ownerRef, kCFNumberIntType, &owner);
        if (owner != pid) continue;

        CFNumberRef layerRef = (CFNumberRef)CFDictionaryGetValue(w, kCGWindowLayer);
        int layer = 1;
        if (layerRef) CFNumberGetValue(layerRef, kCFNumberIntType, &layer);
        if (layer != 0) continue;

        CFDictionaryRef boundsDict = (CFDictionaryRef)CFDictionaryGetValue(w, kCGWindowBounds);
        if (!boundsDict) continue;
        CGRect r;
        if (!CGRectMakeWithDictionaryRepresentation(boundsDict, &r)) continue;
        if (r.size.width < 200 || r.size.height < 200) continue;

        total++;
        if (!CGRectContainsRect(allowed, r)) {
            outside++;
        }
    }
    CFRelease(windows);
    if (outTotal) *outTotal = total;
    if (outOutside) *outOutside = outside;
    return 0;
}

int dw_vd_count_windows_outside_display(int pid, uint32_t displayID) {
    int total = 0;
    int outside = 0;
    if (dw_vd_inspect_windows_for_pid(pid, displayID, &total, &outside) != 0) {
        return 0;
    }
    return outside;
}

static int dw_vd_pid_is_protected(int pid, const int *protectedPIDs, int protectedPIDCount) {
    if (pid <= 0 || !protectedPIDs || protectedPIDCount <= 0) return 0;
    for (int i = 0; i < protectedPIDCount; i++) {
        if (protectedPIDs[i] == pid) return 1;
    }
    return 0;
}

static double dw_vd_intersection_ratio(CGRect a, CGRect b) {
    CGRect intersection = CGRectIntersection(a, b);
    if (CGRectIsNull(intersection) || CGRectIsEmpty(intersection)) return 0.0;
    double area = (double)a.size.width * (double)a.size.height;
    if (area <= 0.0) return 0.0;
    return ((double)intersection.size.width * (double)intersection.size.height) / area;
}

static void dw_vd_copy_cfstring(CFDictionaryRef w, const void *key, char *dst, size_t dstLen) {
    if (!dst || dstLen == 0) return;
    dst[0] = '\0';
    CFStringRef value = (CFStringRef)CFDictionaryGetValue(w, key);
    if (!value || CFGetTypeID(value) != CFStringGetTypeID()) return;
    CFStringGetCString(value, dst, dstLen, kCFStringEncodingUTF8);
}

static int dw_vd_ax_get_point(AXUIElementRef win, CFStringRef attr, CGPoint *out) {
    if (!win || !out) return 0;
    CFTypeRef value = NULL;
    if (AXUIElementCopyAttributeValue(win, attr, &value) != kAXErrorSuccess || !value) return 0;
    int ok = AXValueGetType(value) == kAXValueCGPointType && AXValueGetValue(value, kAXValueCGPointType, out);
    CFRelease(value);
    return ok;
}

static int dw_vd_ax_get_size(AXUIElementRef win, CFStringRef attr, CGSize *out) {
    if (!win || !out) return 0;
    CFTypeRef value = NULL;
    if (AXUIElementCopyAttributeValue(win, attr, &value) != kAXErrorSuccess || !value) return 0;
    int ok = AXValueGetType(value) == kAXValueCGSizeType && AXValueGetValue(value, kAXValueCGSizeType, out);
    CFRelease(value);
    return ok;
}

static AXUIElementRef dw_vd_find_ax_window_for_rect(int pid, CGRect cgRect, CGRect vBounds) {
    AXUIElementRef app = AXUIElementCreateApplication((pid_t)pid);
    if (!app) return NULL;
    CFTypeRef value = NULL;
    if (AXUIElementCopyAttributeValue(app, kAXWindowsAttribute, &value) != kAXErrorSuccess || !value) {
        CFRelease(app);
        return NULL;
    }
    if (CFGetTypeID(value) != CFArrayGetTypeID()) {
        CFRelease(value);
        CFRelease(app);
        return NULL;
    }
    CFArrayRef windows = (CFArrayRef)value;
    AXUIElementRef best = NULL;
    double bestScore = DBL_MAX;
    CFIndex count = CFArrayGetCount(windows);
    for (CFIndex i = 0; i < count; i++) {
        AXUIElementRef win = (AXUIElementRef)CFArrayGetValueAtIndex(windows, i);
        CGPoint p = CGPointZero;
        CGSize s = CGSizeZero;
        if (!dw_vd_ax_get_point(win, kAXPositionAttribute, &p)) continue;
        if (!dw_vd_ax_get_size(win, kAXSizeAttribute, &s)) continue;
        if (s.width < DW_VD_RESCUE_MIN_WIDTH || s.height < DW_VD_RESCUE_MIN_HEIGHT) continue;
        CGRect axRect = CGRectMake(p.x, p.y, s.width, s.height);
        double axRatio = dw_vd_intersection_ratio(axRect, vBounds);
        double sizeDelta = fabs(s.width - cgRect.size.width) + fabs(s.height - cgRect.size.height);
        double posDelta = fabs(p.x - cgRect.origin.x) + fabs(p.y - cgRect.origin.y);
        double score = sizeDelta + posDelta;
        if (axRatio >= 0.50) score -= 100000.0;
        if (score < bestScore) {
            bestScore = score;
            best = win;
        }
    }
    if (best) CFRetain(best);
    CFRelease(value);
    CFRelease(app);
    return best;
}

static int dw_vd_ax_set_bool(AXUIElementRef win, CFStringRef attr, Boolean value) {
    CFBooleanRef v = value ? kCFBooleanTrue : kCFBooleanFalse;
    AXError err = AXUIElementSetAttributeValue(win, attr, v);
    return err == kAXErrorSuccess || err == kAXErrorAttributeUnsupported || err == kAXErrorIllegalArgument;
}

// ─── dw_vd_rescue_foreign_windows ─────────────────────────────────────────
// Moves large normal windows that substantially fell into Deepwork's virtual
// display back to the main display. The protected PID list is supplied by the
// BrowserMuxHost manifest and represents dw-browser-owned headed browser
// runtime processes, regardless of the concrete browser engine.
int dw_vd_rescue_foreign_windows(uint32_t displayID, const int *protectedPIDs, int protectedPIDCount, dw_vd_rescue_record *records, int maxRecords, int *outDisplayID, int *outScanned, int *outMatched, int *outMoved, int *outSkipped) {
    if (outDisplayID) *outDisplayID = 0;
    if (outScanned) *outScanned = 0;
    if (outMatched) *outMatched = 0;
    if (outMoved) *outMoved = 0;
    if (outSkipped) *outSkipped = 0;

    CGDirectDisplayID virtualID = (CGDirectDisplayID)displayID;
    if (virtualID == 0) virtualID = (CGDirectDisplayID)dw_vd_find_any_existing_display_id();
    if (virtualID == 0) return 1;
    if (outDisplayID) *outDisplayID = (int)virtualID;

    // CGDisplayBounds(virtualID) always returns main-screen rect for CGVirtualDisplay
    // (macOS bug). Recompute the actual quarantine bounds from the physical display union.
    int hasPhysical = 0;
    CGRect physUnion = dw_vd_physical_union_from_current(virtualID, &hasPhysical);
    if (!hasPhysical) physUnion = CGDisplayBounds(CGMainDisplayID());
    CGFloat vW = (CGFloat)MAX(1, (int)CGDisplayPixelsWide(virtualID));
    CGFloat vH = (CGFloat)MAX(1, (int)CGDisplayPixelsHigh(virtualID));
    if (vW <= 0 || vH <= 0) return 2;
    CGRect vBounds = CGRectMake(
        CGRectGetMinX(physUnion),
        CGRectGetMaxY(physUnion) + (CGFloat)DW_VD_QUARANTINE_GAP,
        vW, vH);
    CGRect mainBounds = CGDisplayBounds(CGMainDisplayID());
    if (mainBounds.size.width <= 0 || mainBounds.size.height <= 0) return 3;

    NSDictionary *trustOptions = @{(__bridge NSString *)kAXTrustedCheckOptionPrompt: @YES};
    Boolean axTrusted = AXIsProcessTrustedWithOptions((__bridge CFDictionaryRef)trustOptions);

    CFArrayRef windows = CGWindowListCopyWindowInfo(
        kCGWindowListOptionOnScreenOnly | kCGWindowListExcludeDesktopElements,
        kCGNullWindowID);
    if (!windows) return 4;

    int matched = 0;
    int moved = 0;
    int skipped = 0;
    CFIndex n = CFArrayGetCount(windows);
    for (CFIndex i = 0; i < n; i++) {
        CFDictionaryRef w = (CFDictionaryRef)CFArrayGetValueAtIndex(windows, i);
        if (outScanned) (*outScanned)++;

        CFNumberRef ownerRef = (CFNumberRef)CFDictionaryGetValue(w, kCGWindowOwnerPID);
        int owner = 0;
        if (!ownerRef || !CFNumberGetValue(ownerRef, kCFNumberIntType, &owner)) continue;
        if (dw_vd_pid_is_protected(owner, protectedPIDs, protectedPIDCount)) continue;

        CFNumberRef layerRef = (CFNumberRef)CFDictionaryGetValue(w, kCGWindowLayer);
        int layer = 1;
        if (layerRef) CFNumberGetValue(layerRef, kCFNumberIntType, &layer);
        if (layer != 0) continue;

        CFDictionaryRef boundsDict = (CFDictionaryRef)CFDictionaryGetValue(w, kCGWindowBounds);
        if (!boundsDict) continue;
        CGRect r = CGRectZero;
        if (!CGRectMakeWithDictionaryRepresentation(boundsDict, &r)) continue;
        if (r.size.width < DW_VD_RESCUE_MIN_WIDTH || r.size.height < DW_VD_RESCUE_MIN_HEIGHT) continue;

        double ratio = dw_vd_intersection_ratio(r, vBounds);
        if (ratio < DW_VD_RESCUE_MIN_RATIO) continue;

        int recordIndex = matched < maxRecords ? matched : -1;
        dw_vd_rescue_record *rec = recordIndex >= 0 && records ? &records[recordIndex] : NULL;
        if (rec) {
            memset(rec, 0, sizeof(*rec));
            CFNumberRef numberRef = (CFNumberRef)CFDictionaryGetValue(w, kCGWindowNumber);
            if (numberRef) CFNumberGetValue(numberRef, kCFNumberIntType, &rec->windowID);
            rec->pid = owner;
            rec->x = (int)r.origin.x;
            rec->y = (int)r.origin.y;
            rec->w = (int)r.size.width;
            rec->h = (int)r.size.height;
            rec->virtualRatio = ratio;
            rec->reason = DW_VD_RESCUE_REASON_NOT_CANDIDATE;
            dw_vd_copy_cfstring(w, kCGWindowOwnerName, rec->owner, sizeof(rec->owner));
            dw_vd_copy_cfstring(w, kCGWindowName, rec->title, sizeof(rec->title));
        }
        matched++;

        int slot = matched - 1;
        int targetW = (int)MIN(r.size.width, MAX(320.0, mainBounds.size.width - (DW_VD_RESCUE_MARGIN * 2)));
        int targetH = (int)MIN(r.size.height, MAX(240.0, mainBounds.size.height - (DW_VD_RESCUE_MARGIN * 2)));
        int targetX = (int)mainBounds.origin.x + DW_VD_RESCUE_MARGIN + (slot % 5) * DW_VD_RESCUE_CASCADE;
        int targetY = (int)mainBounds.origin.y + DW_VD_RESCUE_MARGIN + (slot % 5) * DW_VD_RESCUE_CASCADE;
        if (rec) {
            rec->targetX = targetX;
            rec->targetY = targetY;
            rec->targetW = targetW;
            rec->targetH = targetH;
        }

        if (!axTrusted) {
            skipped++;
            if (rec) rec->reason = DW_VD_RESCUE_REASON_AX_NOT_TRUSTED;
            continue;
        }

        AXUIElementRef axWin = dw_vd_find_ax_window_for_rect(owner, r, vBounds);
        if (!axWin) {
            skipped++;
            if (rec) rec->reason = DW_VD_RESCUE_REASON_AX_WINDOW_NOT_FOUND;
            continue;
        }

        dw_vd_ax_set_bool(axWin, kAXMinimizedAttribute, false);
        dw_vd_ax_set_bool(axWin, CFSTR("AXFullScreen"), false);
        usleep(120000);

        CGSize targetSize = CGSizeMake((CGFloat)targetW, (CGFloat)targetH);
        CGPoint targetPoint = CGPointMake((CGFloat)targetX, (CGFloat)targetY);
        AXValueRef sizeValue = AXValueCreate(kAXValueCGSizeType, &targetSize);
        AXValueRef posValue = AXValueCreate(kAXValueCGPointType, &targetPoint);
        AXError sizeErr = sizeValue ? AXUIElementSetAttributeValue(axWin, kAXSizeAttribute, sizeValue) : kAXErrorFailure;
        usleep(50000);
        AXError moveErr = posValue ? AXUIElementSetAttributeValue(axWin, kAXPositionAttribute, posValue) : kAXErrorFailure;
        if (moveErr != kAXErrorSuccess) {
            usleep(150000);
            moveErr = posValue ? AXUIElementSetAttributeValue(axWin, kAXPositionAttribute, posValue) : kAXErrorFailure;
        }
        if (sizeValue) CFRelease(sizeValue);
        if (posValue) CFRelease(posValue);
        CFRelease(axWin);

        if (moveErr == kAXErrorSuccess) {
            moved++;
            if (rec) {
                rec->moved = 1;
                rec->reason = DW_VD_RESCUE_REASON_MOVED;
            }
        } else {
            skipped++;
            if (rec) {
                rec->reason = sizeErr != kAXErrorSuccess ? DW_VD_RESCUE_REASON_AX_RESIZE_FAILED : DW_VD_RESCUE_REASON_AX_MOVE_FAILED;
            }
        }
    }
    CFRelease(windows);
    if (outMatched) *outMatched = matched;
    if (outMoved) *outMoved = moved;
    if (outSkipped) *outSkipped = skipped;
    return 0;
}

// ─── dw_vd_is_online ──────────────────────────────────────────────────────
// Returns 1 if displayID appears in CGGetOnlineDisplayList, 0 otherwise.
// CGGetOnlineDisplayList (not Active) is intentional: macOS reports all
// displays inactive when physical displays sleep, while the virtual display
// is still present.
int dw_vd_is_online(uint32_t displayID) {
    if (displayID == 0) return 0;
    CGDirectDisplayID displays[DW_VD_MAX_DISPLAY_PROBE];
    uint32_t count = 0;
    CGGetOnlineDisplayList(DW_VD_MAX_DISPLAY_PROBE, displays, &count);
    for (uint32_t i = 0; i < count; i++) {
        if (displays[i] == (CGDirectDisplayID)displayID) return 1;
    }
    return 0;
}

// ─── dw_vd_is_mirror ──────────────────────────────────────────────────────
// Returns 1 if displayID is in a mirror set (System Settings > Displays →
// "Mirror Displays"), 0 otherwise. Mirror mode causes CGDisplayBounds to
// return the same rect as the main display, making quarantine impossible.
int dw_vd_is_mirror(uint32_t displayID) {
    if (displayID == 0) return 0;
    return CGDisplayIsInMirrorSet((CGDirectDisplayID)displayID) ? 1 : 0;
}

// ─── dw_vd_exit_mirror ─────────────────────────────────────────────────────
// Exits mirror mode for displayID by calling CGConfigureDisplayMirrorOfDisplay
// with kCGNullDirectDisplay as the mirror target (= extended mode).
// Return codes: 0=ok, 1=null displayID, 2=BeginConfig failed, 3=Mirror config
// failed, 4=CompleteConfig failed.
int dw_vd_exit_mirror(uint32_t displayID) {
    if (displayID == 0) return 1;
    CGDisplayConfigRef config = NULL;
    if (CGBeginDisplayConfiguration(&config) != kCGErrorSuccess || config == NULL) return 2;
    CGError err = CGConfigureDisplayMirrorOfDisplay(config, (CGDirectDisplayID)displayID, kCGNullDirectDisplay);
    if (err != kCGErrorSuccess) {
        CGCancelDisplayConfiguration(config);
        return 3;
    }
    CGError complete = CGCompleteDisplayConfiguration(config, kCGConfigureForSession);
    if (complete != kCGErrorSuccess) return 4;
    return 0;
}

// ─── dw_vd_compute_quarantine_origin ───────────────────────────────────────
// Computes the quarantine origin for virtualID WITHOUT relying on
// CGDisplayBounds(virtualID) — which always returns (0,0,W,H) for
// CGVirtualDisplay due to a macOS bug. Uses the same physical-union logic as
// dw_configure_quarantine to determine where the virtual display should be
// placed (below the union of all physical displays + DW_VD_QUARANTINE_GAP).
int dw_vd_compute_quarantine_origin(uint32_t virtualID, int *outX, int *outY) {
    if (virtualID == 0 || !outX || !outY) return 1;
    int hasPhysical = 0;
    CGRect unionBounds = dw_vd_physical_union_from_current((CGDirectDisplayID)virtualID, &hasPhysical);
    if (!hasPhysical) {
        unionBounds = CGDisplayBounds(CGMainDisplayID());
    }
    *outX = (int)CGRectGetMinX(unionBounds);
    *outY = (int)CGRectGetMaxY(unionBounds) + DW_VD_QUARANTINE_GAP;
    return 0;
}

// ─── dw_vd_inspect_windows_for_pid_bounds ──────────────────────────────────
// Same logic as dw_vd_inspect_windows_for_pid but uses EXPLICIT bounds
// (boundsX, boundsY, boundsW, boundsH) instead of CGDisplayBounds(displayID).
// This is correct for CGVirtualDisplay because CGDisplayBounds always returns
// the main display's rect regardless of the configured origin.
int dw_vd_inspect_windows_for_pid_bounds(int pid, int boundsX, int boundsY, int boundsW, int boundsH, int *outTotal, int *outOutside) {
    if (outTotal) *outTotal = 0;
    if (outOutside) *outOutside = 0;
    if (pid <= 0 || boundsW <= 0 || boundsH <= 0) return 1;
    CGRect vBounds = CGRectMake((CGFloat)boundsX, (CGFloat)boundsY, (CGFloat)boundsW, (CGFloat)boundsH);
    // Asymmetric tolerance: allow the window FRAME (title bar ~38-52px) to extend
    // above virtual display top. --window-position sets the content area, not the
    // frame, so the frame top lands ~50px above vBounds.origin.y.
    // Bottom/left/right get the standard 8px tolerance.
    CGRect allowed = CGRectMake(
        vBounds.origin.x - 8.0,
        vBounds.origin.y - 60.0,
        vBounds.size.width + 16.0,
        vBounds.size.height + 68.0);

    CFArrayRef windows = CGWindowListCopyWindowInfo(
        kCGWindowListOptionOnScreenOnly | kCGWindowListExcludeDesktopElements,
        kCGNullWindowID);
    if (!windows) return 2;

    int total = 0;
    int outside = 0;
    CFIndex n = CFArrayGetCount(windows);
    for (CFIndex i = 0; i < n; i++) {
        CFDictionaryRef w = (CFDictionaryRef)CFArrayGetValueAtIndex(windows, i);

        CFNumberRef ownerRef = (CFNumberRef)CFDictionaryGetValue(w, kCGWindowOwnerPID);
        if (!ownerRef) continue;
        int owner = 0;
        CFNumberGetValue(ownerRef, kCFNumberIntType, &owner);
        if (owner != pid) continue;

        CFNumberRef layerRef = (CFNumberRef)CFDictionaryGetValue(w, kCGWindowLayer);
        int layer = 1;
        if (layerRef) CFNumberGetValue(layerRef, kCFNumberIntType, &layer);
        if (layer != 0) continue;

        CFDictionaryRef boundsDict = (CFDictionaryRef)CFDictionaryGetValue(w, kCGWindowBounds);
        if (!boundsDict) continue;
        CGRect r;
        if (!CGRectMakeWithDictionaryRepresentation(boundsDict, &r)) continue;
        if (r.size.width < 200 || r.size.height < 200) continue;

        total++;
        if (!CGRectContainsRect(allowed, r)) {
            outside++;
        }
    }
    CFRelease(windows);
    if (outTotal) *outTotal = total;
    if (outOutside) *outOutside = outside;
    return 0;
}

// ─── dw_vd_destroy ─────────────────────────────────────────────────────────
// Releases the CGVirtualDisplay* previously retained by dw_vd_create.
// After the last reference drops, WindowServer automatically removes the
// virtual display from the active display list.
void dw_vd_destroy(void *displayRef) {
    if (!displayRef) return;
    if (displayRef == DW_VD_ORPHANED_DISPLAY_SENTINEL) {
        // Sentinel: we didn't create the CGVirtualDisplay (orphaned reuse path).
        // We have no ObjC ownership, so nothing to release. The display was
        // already orphaned before we ran; it will persist until system restart.
        return;
    }
    // Normal path: release the ObjC object retained in dw_vd_create.
    // After the retain count drops to zero, CGVirtualDisplay deallocs and
    // WindowServer removes the virtual display from the active display list.
    id obj = (__bridge id)displayRef;
    [obj release];
}
