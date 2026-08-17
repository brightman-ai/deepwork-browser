package main

import (
	"testing"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
)

// 单操作者门的判据是"这条命令要不要争这个会话",全部由 argv 决定。
// 它跑在任何 flag 解析之前,所以必须自己认得 --id / --lock-wait / --force。

func TestSessionIDIsScannedFromArgvInBothSpellings(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"act", "--id", "ws1", "click @r1"}, "ws1"},
		{[]string{"observe", "--id=ws2"}, "ws2"},
		{[]string{"journey", "--file", "spec.yaml", "--id", "ws3"}, "ws3"},
		{[]string{"once", "--url", "http://x"}, ""},
		{[]string{"gc"}, ""},
	}
	for _, tc := range cases {
		if got := scanSessionIDArg(tc.argv); got != tc.want {
			t.Errorf("scanSessionIDArg(%v)=%q, want %q", tc.argv, got, tc.want)
		}
	}
}

// muxhost 是被持锁的会话命令 fork 出来的宿主进程,而且带的就是同一个 --id。
// 它要是也去取同一把锁,必然自锁死 —— 这条豁免是正确性要求,不是优化。
func TestMuxHostIsExemptFromTheSessionLock(t *testing.T) {
	if !operatorLockExemptCommands["muxhost"] {
		t.Fatal("muxhost 不在豁免名单里 —— 它由持锁进程 fork 且带同一个 --id, 会自锁死")
	}
	for _, cmd := range []string{"gc", "--version", "help"} {
		if !operatorLockExemptCommands[cmd] {
			t.Errorf("%q 不碰会话却要取会话锁", cmd)
		}
	}
	for _, cmd := range []string{"act", "observe", "open", "close", "eval", "journey"} {
		if operatorLockExemptCommands[cmd] {
			t.Errorf("会话作用域命令 %q 被豁免了会话锁", cmd)
		}
	}
}

func TestLockWaitParsesDurationsAndBareSeconds(t *testing.T) {
	cases := []struct {
		argv []string
		want time.Duration
	}{
		{[]string{"act", "--id", "s"}, browser.DefaultOperatorLockWait},
		{[]string{"act", "--id", "s", "--lock-wait", "30s"}, 30 * time.Second},
		{[]string{"act", "--id", "s", "--lock-wait=500ms"}, 500 * time.Millisecond},
		{[]string{"act", "--id", "s", "--lock-wait", "45"}, 45 * time.Second},
		{[]string{"act", "--id", "s", "--lock-wait", "0"}, 0},
		// 打错的等待时长不该让整条命令失败 —— 回退默认并说一声。
		{[]string{"act", "--id", "s", "--lock-wait", "soon"}, browser.DefaultOperatorLockWait},
	}
	for _, tc := range cases {
		if got := scanLockWait(tc.argv); got != tc.want {
			t.Errorf("scanLockWait(%v)=%s, want %s", tc.argv, got, tc.want)
		}
	}
}

// --lock-wait / --force 必须被 parseCommonFlags 吃掉。未知 flag 在这里会掉进
// positional,而 act 的 positional 就是动作串 —— 那会变成"click @r1 被顶掉,
// --lock-wait 被当成动作"这类离奇失败。
func TestLockFlagsDoNotLeakIntoPositionalArgs(t *testing.T) {
	positional, flags := parseCommonFlags([]string{"--id", "s1", "click @r1", "--lock-wait", "30s", "--force"}, "act")
	if len(positional) != 1 || positional[0] != "click @r1" {
		t.Fatalf("positional=%v, want 只剩动作串 —— 锁相关 flag 漏进了动作参数", positional)
	}
	if flags.lockWait != "30s" {
		t.Fatalf("flags.lockWait=%q, want 30s", flags.lockWait)
	}
	if !flags.force {
		t.Fatal("flags.force 没有被解析")
	}
	if flags.sessionID != "s1" {
		t.Fatalf("flags.sessionID=%q", flags.sessionID)
	}
}

func TestForceIsDetectedForTheCloseEscapeHatch(t *testing.T) {
	if !scanBoolFlag([]string{"close", "--id", "s1", "--force"}, "--force") {
		t.Fatal("--force 没被识别 —— 占有者卡死时就没有逃生口了")
	}
	if scanBoolFlag([]string{"close", "--id", "s1"}, "--force") {
		t.Fatal("没给 --force 却认为给了 —— 会静默绕过会话锁")
	}
}
