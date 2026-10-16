//go:build darwin

package browser

/*
#cgo LDFLAGS: -framework CFNetwork -framework CoreFoundation
#include <CFNetwork/CFNetwork.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
static int dw_public_proxy_int(CFDictionaryRef d, const char *name) {
 CFStringRef key = CFStringCreateWithCString(NULL, name, kCFStringEncodingUTF8);
 CFTypeRef value = CFDictionaryGetValue(d, key);
 CFRelease(key);
 int result = 0;
 if (value && CFGetTypeID(value) == CFNumberGetTypeID()) { if (!CFNumberGetValue(value, kCFNumberIntType, &result)) return -1; }
 else if (value && CFGetTypeID(value) == CFBooleanGetTypeID()) result = CFBooleanGetValue(value);
 else if (value) return -1;
 return result;
}
static int dw_public_proxy_string(CFDictionaryRef d, const char *name, char *out, int size) {
 CFStringRef key = CFStringCreateWithCString(NULL, name, kCFStringEncodingUTF8);
 CFTypeRef value = CFDictionaryGetValue(d, key);
 CFRelease(key);
 if (!value) { out[0] = 0; return 1; }
 if (CFGetTypeID(value) != CFStringGetTypeID()) return 0;
 return CFStringGetCString(value, out, size, kCFStringEncodingUTF8);
}
*/
import "C"

import (
	"errors"
	"unsafe"
)

// Snapshot only: never modifies system settings or executes PAC.
func readSystemProxySettings() (publicSystemProxySettings, string, error) {
	var result publicSystemProxySettings
	d := C.CFNetworkCopySystemProxySettings()
	if d == 0 {
		return result, "macos-system", errors.New("cannot read system proxy settings")
	}
	defer C.CFRelease(C.CFTypeRef(d))
	number := func(key string) int {
		k := C.CString(key)
		defer C.free(unsafe.Pointer(k))
		return int(C.dw_public_proxy_int(d, k))
	}
	value := func(key string) (string, error) {
		k := C.CString(key)
		defer C.free(unsafe.Pointer(k))
		var b [4096]C.char
		if C.dw_public_proxy_string(d, k, &b[0], C.int(len(b))) == 0 {
			return "", errors.New("invalid system proxy host setting")
		}
		return C.GoString(&b[0]), nil
	}
	result.HTTPEnabled, result.HTTPSEnabled = number("HTTPEnable") != 0, number("HTTPSEnable") != 0
	for _, key := range []string{"HTTPEnable", "HTTPSEnable", "SOCKSEnable", "ProxyAutoConfigEnable", "ProxyAutoDiscoveryEnable"} {
		if n := number(key); n != 0 && n != 1 {
			return result, "macos-system", errors.New("unknown system proxy enable setting")
		}
	}
	result.SOCKSEnabled = number("SOCKSEnable") != 0
	result.PACEnabled, result.AutoDiscoveryEnabled = number("ProxyAutoConfigEnable") != 0, number("ProxyAutoDiscoveryEnable") != 0
	result.HTTPPort, result.HTTPSPort = number("HTTPPort"), number("HTTPSPort")
	var err error
	if result.HTTPHost, err = value("HTTPProxy"); err != nil {
		return result, "macos-system", err
	}
	if result.HTTPSHost, err = value("HTTPSProxy"); err != nil {
		return result, "macos-system", err
	}
	return result, "macos-system", nil
}
