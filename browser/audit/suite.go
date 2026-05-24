package audit

// Suite 是命名的 check 筛选器。
type Suite struct {
	Name   string
	Filter func(Check) bool
}

// byTag 返回按 tag 过滤的 Suite Filter。
func byTag(tag string) func(Check) bool {
	return func(c Check) bool {
		return hasTag(c.Tags, tag)
	}
}

// Suites 预定义 suite 集合。
var Suites = map[string]Suite{
	"compat": {
		Name:   "compat",
		Filter: func(c Check) bool { return c.Category == "compat" },
	},
	"a11y": {
		Name:   "a11y",
		Filter: func(c Check) bool { return c.Category == "a11y" },
	},
	"layout": {
		Name:   "layout",
		Filter: func(c Check) bool { return c.Category == "layout" },
	},
	"perf": {
		Name:   "perf",
		Filter: func(c Check) bool { return c.Category == "perf" },
	},
	"touch": {
		Name:   "touch",
		Filter: byTag("touch"),
	},
	"ios": {
		Name:   "ios",
		Filter: byTag("ios"),
	},
	"full": {
		Name:   "full",
		Filter: func(c Check) bool { return true },
	},
}
