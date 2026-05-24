package testing

import "strings"

// Getter 从 Observation 中提取类型化数据
type Getter struct{}

// GetResult 是提取结果
type GetResult struct {
	Query string      `json:"query"`
	Value interface{} `json:"value"`
	Type  string      `json:"type"` // "string", "int", "[]string"
}

// Get 从 observation 中提取数据（内置映射，不调 LLM）
func (g *Getter) Get(obs *Observation, query string) *GetResult {
	q := strings.ToLower(strings.TrimSpace(query))

	switch q {
	case "active tab url":
		if obs.Behavior != nil {
			for _, t := range obs.Behavior.Tabs {
				if t.Active {
					return &GetResult{Query: query, Value: t.URL, Type: "string"}
				}
			}
			// No active tab found — return empty string
			return &GetResult{Query: query, Value: "", Type: "string"}
		}
		return &GetResult{Query: query, Value: nil, Type: "string"}

	case "page title":
		return &GetResult{Query: query, Value: obs.Page.Title, Type: "string"}

	case "page url":
		return &GetResult{Query: query, Value: obs.Page.URL, Type: "string"}

	case "tab count":
		if obs.Behavior != nil {
			return &GetResult{Query: query, Value: obs.Behavior.TabCount, Type: "int"}
		}
		return &GetResult{Query: query, Value: 0, Type: "int"}

	case "console errors":
		if obs.Telemetry != nil {
			msgs := make([]string, 0, len(obs.Telemetry.ConsoleErrors))
			for _, e := range obs.Telemetry.ConsoleErrors {
				msgs = append(msgs, e.Text)
			}
			return &GetResult{Query: query, Value: msgs, Type: "[]string"}
		}
		return &GetResult{Query: query, Value: []string{}, Type: "[]string"}

	case "text":
		if obs.Structural != nil {
			return &GetResult{Query: query, Value: obs.Structural.Text, Type: "string"}
		}
		return &GetResult{Query: query, Value: "", Type: "string"}

	case "refs count":
		if obs.Structural != nil {
			return &GetResult{Query: query, Value: obs.Structural.RefsCount, Type: "int"}
		}
		return &GetResult{Query: query, Value: 0, Type: "int"}

	default:
		return &GetResult{Query: query, Value: nil, Type: "unknown"}
	}
}
