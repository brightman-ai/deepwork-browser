package browser

// ActionDefinition is the executable action vocabulary used by validation,
// command discovery, completion and the same-version tool protocol.
type ActionDefinition struct {
	Kind        string   `json:"kind"`
	Summary     string   `json:"summary"`
	TargetKinds []string `json:"target_kinds,omitempty"`
	Fields      []string `json:"fields,omitempty"`
	Mutates     bool     `json:"mutates"`
}

func ActionDefinitions() []ActionDefinition {
	pointer := []string{"ref", "locator", "point"}
	element := []string{"ref", "locator"}
	return []ActionDefinition{
		{"click", "Click a visible target; count 2 or button right selects its mouse variant", pointer, []string{"target", "button", "count"}, true},
		{"tap", "Touch a visible target", pointer, []string{"target"}, true},
		{"hover", "Move the pointer to a visible target", pointer, []string{"target"}, false},
		{"fill", "Replace editable text using verified physical selection and input", element, []string{"target", "text", "sensitive"}, true},
		{"type", "Insert text into a target or the physically focused editor", element, []string{"target", "text", "focused"}, true},
		{"press", "Dispatch a platform key or shortcut", element, []string{"target", "key"}, true},
		{"focus", "Physically focus an editable target", element, []string{"target"}, false},
		{"select", "Select a native option using verified keyboard interaction; explicit baseline element steps provide synthetic functional evidence", element, []string{"target", "option"}, true},
		{"set-checked", "Set a checkbox to an explicit boolean", element, []string{"target", "checked"}, true},
		{"scroll", "Scroll the viewport or target at a declared point", pointer, []string{"target", "direction", "steps", "delta_x", "delta_y"}, false},
		{"scrollto", "Scroll a persistent target into view", element, []string{"target"}, false},
		{"drag", "Drag between two points in one coordinate frame", pointer, []string{"target", "end"}, true},
		{"swipe", "Swipe between two touch points", pointer, []string{"target", "end"}, true},
		{"navigate", "Navigate the existing session to a URL", nil, []string{"url"}, false},
		{"reload", "Reload the existing document", nil, nil, false},
		{"back", "Traverse browser history backwards", nil, nil, false},
		{"forward", "Traverse browser history forwards", nil, nil, false},
		{"zoom", "Set page scale", nil, []string{"value"}, true},
		{"keyboard", "Show or hide the emulated mobile keyboard", nil, []string{"value"}, true},
	}
}

func LookupActionDefinition(kind string) (ActionDefinition, bool) {
	for _, d := range ActionDefinitions() {
		if d.Kind == kind {
			return d, true
		}
	}
	return ActionDefinition{}, false
}
