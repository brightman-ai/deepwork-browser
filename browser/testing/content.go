package testing

import (
	"context"
	"fmt"
	"github.com/brightman-ai/deepwork-browser/browser"
)

// ContentState is document reading evidence; it never grants action refs.
type ContentState struct {
	Format string        `json:"format"`
	Scope  string        `json:"scope"`
	Text   *string       `json:"text,omitempty"`
	Items  *[]string     `json:"items,omitempty"`
	Tables *[][][]string `json:"tables,omitempty"`
}

func CollectContent(ctx context.Context, core browser.BrowserCore, format string) (*ContentState, error) {
	state := &ContentState{Format: format, Scope: "document"}
	switch format {
	case "text":
		text, err := core.Text(ctx, nil)
		if err != nil {
			return nil, err
		}
		state.Text = &text
	case "list":
		items := []string{}
		if err := core.EvalJS(ctx, `Array.from(document.querySelectorAll('li')).map(li=>li.innerText.trim()).filter(Boolean)`, &items); err != nil {
			return nil, err
		}
		state.Items = &items
	case "table":
		tables := [][][]string{}
		if err := core.EvalJS(ctx, `Array.from(document.querySelectorAll('table')).map(tbl=>Array.from(tbl.querySelectorAll('tr')).map(tr=>Array.from(tr.querySelectorAll('th,td')).map(td=>td.innerText.trim())))`, &tables); err != nil {
			return nil, err
		}
		state.Tables = &tables
	default:
		return nil, fmt.Errorf("unsupported content format %q (text/list/table)", format)
	}
	return state, nil
}
