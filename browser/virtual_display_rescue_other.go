//go:build !darwin

package browser

func RescueForeignWindowsFromVirtualDisplay (*VirtualDisplayWindowRescueResult, error) {
	return &VirtualDisplayWindowRescueResult{
		Platform: "unsupported"
		UnavailableReason: "virtual_display_rescue_only_supported_on_macos"
		Windows: VirtualDisplayWindowRescueRecord{}
	}, nil
}
