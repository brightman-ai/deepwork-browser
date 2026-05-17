package safari

import "strings"

// axToARIA 将 macOS AXUIElement role 映射到 ARIA role。
var axToARIA = map[string]string{
	"AXButton": "button"
	"AXTextField": "textbox"
	"AXTextArea": "textbox"
	"AXStaticText": "text"
	"AXLink": "link"
	"AXImage": "img"
	"AXCheckBox": "checkbox"
	"AXRadioButton": "radio"
	"AXComboBox": "combobox"
	"AXHeading": "heading"
	"AXList": "list"
	"AXListItem": "listitem"
	"AXGroup": "group"
	"AXWebArea": "document"
	"AXTable": "table"
	"AXRow": "row"
	"AXCell": "cell"
	"AXMenuItem": "menuitem"
	"AXMenu": "menu"
	"AXMenuBar": "menubar"
	"AXTabGroup": "tablist"
	"AXTab": "tab"
	"AXScrollArea": "region"
	"AXSlider": "slider"
	"AXProgressIndicator": "progressbar"
	"AXPopUpButton": "combobox"
	"AXDisclosureTriangle": "button"
	"AXToolbar": "toolbar"
	"AXOutline": "tree"
	"AXOutlineRow": "treeitem"
	"AXValueIndicator": "slider"
	"AXSplitGroup": "group"
	"AXSplitter": "separator"
	"AXColorWell": "button"
	"AXGrowArea": "generic"
	"AXSheet": "dialog"
	"AXDrawer": "complementary"
	"AXIncrementor": "spinbutton"
	"AXBusyIndicator": "progressbar"
}

// interactableARIARoles 是可交互的 ARIA role 集合（与 Chrome snapshot_engine 对齐）。
var interactableARIARoles = map[string]bool{
	"button": true
	"link": true
	"textbox": true
	"searchbox": true
	"combobox": true
	"listbox": true
	"option": true
	"menuitem": true
	"checkbox": true
	"radio": true
	"switch": true
	"slider": true
	"spinbutton": true
	"tab": true
	"treeitem": true
}

// genericARIARoles 是噪声 role，应过滤掉。
var genericARIARoles = map[string]bool{
	"generic": true
	"none": true
	"presentation": true
}

// mapAXRoleToARIA 转换 macOS AX role 到 ARIA role。
func mapAXRoleToARIA(axRole string) string {
	if aria, ok := axToARIA[axRole]; ok {
		return aria
	}
	// 未知 role 保留原始值（去掉 AX 前缀，小写化）
	if len(axRole) > 2 && axRole[:2] == "AX" {
		lower := strings.ToLower(axRole[2:])
		return lower
	}
	return strings.ToLower(axRole)
}
