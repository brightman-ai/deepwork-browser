package main

import (
	"strings"
	"testing"
)

// REQ-SELFDESC-01: 自说话契约——每条命令必须带 guidance 与至少一个可复制
// example；usage 对非诊断命令必须存在。新增命令漏填时此测试红。
func TestAllCommandsCarryGuidanceAndExamples(t *testing.T) {
	defs := commandDefinitions()
	if len(defs) < 60 {
		t.Fatalf("command surface unexpectedly small: %d commands", len(defs))
	}
	for _, d := range defs {
		if strings.TrimSpace(d.Guidance) == "" {
			t.Errorf("%s: missing guidance (self-describing contract)", d.Path)
		}
		if len(d.Examples) == 0 {
			t.Errorf("%s: missing examples (self-describing contract)", d.Path)
		}
		for i, ex := range d.Examples {
			if !strings.HasPrefix(ex, "dw-browser ") {
				t.Errorf("%s: example[%d] must be copy-runnable starting with 'dw-browser ': %q", d.Path, i, ex)
			}
		}
		if !d.Diagnostic && strings.TrimSpace(d.Usage) == "" {
			t.Errorf("%s: non-diagnostic command missing usage", d.Path)
		}
	}
}
