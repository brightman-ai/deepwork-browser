package main

import (
	"strings"
	"testing"
)

// 输入窗口的 .owner 会原样出现在另一个进程的报错里:只许命令路径与 --id,
// 任何位置参数 / flag 值(act fill --text、eval 表达式、fillsecret)都不得进入摘要。
func TestInputWindowCommandSummaryCarriesOnlyVerbAndSessionID(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"act", "fill", "@r3", "--id", "s1", "--text", "hunter2-secret"}, "act fill --id s1"},
		{[]string{"act", "fill", "@r3", "--id=s1", "--text=hunter2-secret"}, "act fill --id s1"},
		{[]string{"act", "click", "css=#submit", "--id", "chat1"}, "act click --id chat1"},
		{[]string{"eval", "document.cookie", "--id", "s2"}, "eval --id s2"},
		{[]string{"tabs", "select", "--target", "T1", "--id", "s3"}, "tabs select --id s3"},
	}
	for _, c := range cases {
		def, _, err := lookupCommand(c.argv)
		if err != nil {
			t.Fatalf("lookupCommand(%v): %v", c.argv, err)
		}
		got := inputWindowCommandSummary(def, c.argv)
		if got != c.want {
			t.Errorf("summary(%v) = %q, want %q", c.argv, got, c.want)
		}
		for _, leak := range []string{"hunter2", "document.cookie", "css=#submit", "@r3"} {
			if strings.Contains(got, leak) {
				t.Errorf("summary(%v) leaked %q: %q", c.argv, leak, got)
			}
		}
	}
}
