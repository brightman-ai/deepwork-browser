package browser

// Package browser — fill/type 的 `with` 误用拦截测试 [BUG-FILL-WITH]
//
// 真因: DSL 合法支持不带引号的多词值(fill @r1 hello world → "hello world"),
// 于是 `fill @r1 with 'hello'` 被静默当成字面值 "with 'hello'" 填进输入框,
// act 返回 success:true / 退出码 0 → agent 无从察觉 (实测截图已确认输入框里真是 with 'hello')。

import (
	"errors"
	"strings"
	"testing"
)

func TestFillWithKeyword_Rejected(t *testing.T) {
	cases := []struct {
		action string
		desc   string
	}{
		{"fill @r1 with 'hello'", "单引号"},
		{`fill @r1 with "hello"`, "双引号"},
		{"fill @r1 WITH 'hello'", "大写 WITH"},
		{"type @r1 with 'hello'", "type 同样拦截"},
		{"fillsecret @r1 with 'secret'", "fillsecret 同样拦截"},
		{"fill css=.x with 'v'", "CSS 选择器同样拦截"},
	}
	for _, tc := range cases {
		_, err := ParseAction(tc.action)
		if err == nil {
			t.Errorf("fill-with %s: %q 必须报错(否则静默填错值), got nil", tc.desc, tc.action)
			continue
		}
		if !errors.Is(err, ErrActFailed) {
			t.Errorf("fill-with %s: 应为 ErrActFailed, got %v", tc.desc, err)
		}
		// 错误信息必须给出正确写法，否则 agent 不知道怎么改
		if !strings.Contains(err.Error(), "正确写法") {
			t.Errorf("fill-with %s: 错误信息缺少正确写法指引: %v", tc.desc, err)
		}
	}
}

func TestFillWithKeyword_EscapeHatchAndNoRegression(t *testing.T) {
	cases := []struct {
		action string
		want   string
		desc   string
	}{
		// 逃生舱: 真要填 "with 'hello'" 这个字面量 → 整体加引号即可
		{`fill @r1 "with 'hello'"`, "with 'hello'", "整体加引号 = 逃生舱"},
		// 正常用法不受影响
		{"fill @r1 hello", "hello", "单词值"},
		{"fill @r1 hello world", "hello world", "多词裸值"},
		{"fill @r1 'hello world'", "hello world", "引号多词值"},
		{"fill @r1 withdraw", "withdraw", "以 with 开头的词不误伤"},
		{"fill @r1 with hello", "with hello", "with 后无引号 → 不拦截(可能是真值)"},
		{"fill @r1 deal with it", "deal with it", "with 在中间不误伤"},
	}
	for _, tc := range cases {
		got, err := ParseAction(tc.action)
		if err != nil {
			t.Errorf("fill-ok %s: %q 不该报错, got %v", tc.desc, tc.action, err)
			continue
		}
		if got.Value != tc.want {
			t.Errorf("fill-ok %s: %q → Value=%q, want %q", tc.desc, tc.action, got.Value, tc.want)
		}
	}
}
