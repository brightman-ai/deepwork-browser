package audit

// AuditContext 运行时上下文，用于参数化 checks。
type AuditContext struct {
	Engine string // "chrome" | "safari"
	IsTouch bool
	HasNotch bool
	DeviceName string
	ViewportW int
	ViewportH int
}

// ApplyContext 用上下文覆盖 check 的默认参数，返回覆盖后的参数副本。
// check 本身不被修改。
func ApplyContext(check *Check, actx *AuditContext) map[string]any {
	if check == nil {
		return nil
	}

	// 复制默认参数
	params := make(map[string]any, len(check.Params))
	for k, v := range check.Params {
		params[k] = v
	}

	if actx == nil {
		return params
	}

	// touch-target check: minSize 随设备类型调整
	if check.ID == "touch-target-size" || hasTag(check.Tags, "touch") {
		if actx.IsTouch {
			params["minSize"] = 44
		} else {
			params["minSize"] = 24
		}
	}

	// 注入通用上下文参数供 JS 脚本使用
	params["_engine"] = actx.Engine
	params["_isTouch"] = actx.IsTouch
	params["_hasNotch"] = actx.HasNotch
	params["_viewportW"] = actx.ViewportW
	params["_viewportH"] = actx.ViewportH

	return params
}

func hasTag(tags string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}
