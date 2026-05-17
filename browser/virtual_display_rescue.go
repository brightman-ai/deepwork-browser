package browser

type VirtualDisplayWindowRescueResult struct {
	Platform string `json:"platform"`
	DisplayID uint32 `json:"display_id,omitempty"`
	ProtectedBrowserPIDs int `json:"protected_browser_pids,omitempty"`
	Scanned int `json:"scanned"`
	Matched int `json:"matched"`
	Moved int `json:"moved"`
	Skipped int `json:"skipped"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	Windows VirtualDisplayWindowRescueRecord `json:"windows"`
}

type VirtualDisplayWindowRescueRecord struct {
	WindowID int `json:"window_id"`
	PID int `json:"pid"`
	Owner string `json:"owner"`
	Title string `json:"title,omitempty"`
	X int `json:"x"`
	Y int `json:"y"`
	Width int `json:"width"`
	Height int `json:"height"`
	TargetX int `json:"target_x,omitempty"`
	TargetY int `json:"target_y,omitempty"`
	TargetWidth int `json:"target_width,omitempty"`
	TargetHeight int `json:"target_height,omitempty"`
	Moved bool `json:"moved"`
	Reason string `json:"reason"`
	VirtualRatio float64 `json:"virtual_ratio"`
}
