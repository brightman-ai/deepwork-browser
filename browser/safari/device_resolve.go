package safari

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// udidPattern matches the standard UUID format: 8-4-4-4-12 hex characters.
var udidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// isUDID reports whether s is a UUID-formatted string.
func isUDID(s string) bool {
	return udidPattern.MatchString(s)
}

// parseFamily returns the normalised family keyword from a query string.
// Returns one of "ios", "iphone", "ipad", or "" if not a family keyword.
func parseFamily(query string) string {
	switch strings.ToLower(strings.TrimSpace(query)) {
	case "ios", "auto":
		return "ios"
	case "iphone":
		return "iphone"
	case "ipad":
		return "ipad"
	}
	return ""
}

// matchFamily reports whether a device name belongs to the given family.
// family must be one of "ios", "iphone", "ipad".
func matchFamily(name, family string) bool {
	lower := strings.ToLower(name)
	switch family {
	case "ios":
		return strings.Contains(lower, "iphone") || strings.Contains(lower, "ipad")
	case "iphone":
		return strings.Contains(lower, "iphone")
	case "ipad":
		return strings.Contains(lower, "ipad")
	}
	return false
}

// compareRuntime compares two runtime version strings such as "iOS 18.5" and "iOS 17.0".
// Returns positive if a > b, negative if a < b, 0 if equal.
func compareRuntime(a, b string) int {
	parseVersion := func(s string) (major, minor int) {
		// Strip leading non-digit prefix (e.g. "iOS ")
		for i, r := range s {
			if r >= '0' && r <= '9' {
				s = s[i:]
				break
			}
		}
		parts := strings.SplitN(s, ".", 2)
		if len(parts) >= 1 {
			major, _ = strconv.Atoi(parts[0])
		}
		if len(parts) >= 2 {
			minor, _ = strconv.Atoi(parts[1])
		}
		return
	}
	aMaj, aMin := parseVersion(a)
	bMaj, bMin := parseVersion(b)
	if aMaj != bMaj {
		return aMaj - bMaj
	}
	return aMin - bMin
}

// runtimeLabel returns a human-readable runtime label from a SimulatorDevice.
// Prefers RuntimeVersion if available, falls back to the raw Runtime identifier.
func runtimeLabel(d SimulatorDevice) string {
	if d.RuntimeVersion != "" {
		return "iOS " + d.RuntimeVersion
	}
	return d.Runtime
}

// SmartResolveDevice intelligently resolves a device query string to a booted simulator.
//
// Resolution chain:
//  1. UDID — if query matches UUID format, verify and return directly.
//  2. Exact name — find a device whose Name equals query (case-insensitive).
//  3. Family keyword — "ios"/"auto" match all iPhone+iPad; "iphone" matches iPhone only;
//     "ipad" matches iPad only. Within matches, Booted devices are preferred; ties broken
//     by RuntimeVersion descending.
//  4. No match — return an error listing available devices.
//
// If the resolved device is not Booted, Boot is called automatically.
func SmartResolveDevice(ctx context.Context, simctl *SimctlManager, query string) (SimulatorDevice, error) {
	devices, err := simctl.ListDevices(ctx)
	if err != nil {
		return SimulatorDevice{}, fmt.Errorf("smart resolve device: %w", err)
	}

	// 1. UDID match.
	if isUDID(query) {
		for _, d := range devices {
			if strings.EqualFold(d.UDID, query) {
				return bootIfNeeded(ctx, simctl, d)
			}
		}
		return SimulatorDevice{}, fmt.Errorf("no simulator with UDID %q", query)
	}

	// 2. Exact name match (case-insensitive).
	queryLower := strings.ToLower(query)
	for _, d := range devices {
		if strings.ToLower(d.Name) == queryLower {
			return bootIfNeeded(ctx, simctl, d)
		}
	}

	// 3. Family keyword match.
	if family := parseFamily(query); family != "" {
		var candidates []SimulatorDevice
		for _, d := range devices {
			if matchFamily(d.Name, family) {
				candidates = append(candidates, d)
			}
		}
		if len(candidates) > 0 {
			// Sort: Booted first, then by runtime version descending.
			sort.Slice(candidates, func(i, j int) bool {
				if candidates[i].State != candidates[j].State {
					// "Booted" < "Shutdown" in sort order so Booted sorts first.
					return candidates[i].State == "Booted"
				}
				return compareRuntime(runtimeLabel(candidates[i]), runtimeLabel(candidates[j])) > 0
			})
			return bootIfNeeded(ctx, simctl, candidates[0])
		}
	}

	// 4. No match — build a helpful error message.
	names := make([]string, 0, len(devices))
	for _, d := range devices {
		names = append(names, d.Name)
	}
	return SimulatorDevice{}, fmt.Errorf("no simulator matching %q; available: [%s]",
		query, strings.Join(names, ", "))
}

// bootIfNeeded boots the device if it is not already booted, then returns it.
func bootIfNeeded(ctx context.Context, simctl *SimctlManager, d SimulatorDevice) (SimulatorDevice, error) {
	if d.State == "Booted" {
		return d, nil
	}
	if err := simctl.Boot(ctx, d.UDID); err != nil {
		return SimulatorDevice{}, fmt.Errorf("boot simulator %q (%s): %w", d.Name, d.UDID, err)
	}
	d.State = "Booted"
	return d, nil
}
