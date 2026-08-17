package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
)

// runGC implements `dw-browser gc` — reclaim Chrome processes and profile
// directories that no live owner accounts for.
//
// Separate from `profile prune`, which reasons about *directories* under the
// CLI profile roots. gc reasons about *processes*: the registry notes this
// build writes, plus the ownerless Chromes that no note covers because the
// process that forked them was killed before it could write one. Those are
// exactly the ones that used to accumulate silently — ~60 per `go test
// ./browser/`, five waves in one day, 13G of profile dirs at the peak.
func runGC(args []string) {
	dryRun := false
	jsonOut := false
	minAge := browser.DefaultChromeGCMinAge

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--dry-run":
			dryRun = true
		case arg == "--json":
			jsonOut = true
		case arg == "--all":
			// No age floor. For "clean this machine now", when the caller knows
			// no launch is in flight.
			minAge = -1
		case arg == "--older-than" && i+1 < len(args):
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "dw-browser gc: invalid --older-than %q: %v\n", args[i+1], err)
				os.Exit(exitRunErr)
			}
			minAge = d
			i++
		case arg == "--help" || arg == "-h":
			printGCUsage()
			os.Exit(exitOK)
		default:
			fmt.Fprintf(os.Stderr, "dw-browser gc: unknown flag %q\n", arg)
			printGCUsage()
			os.Exit(exitRunErr)
		}
	}

	result := browser.RunChromeGC(browser.ChromeGCOptions{DryRun: dryRun, MinAge: minAge})

	if jsonOut {
		enc, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser gc: encode result: %v\n", err)
			os.Exit(exitRunErr)
		}
		fmt.Println(string(enc))
		os.Exit(exitOK)
	}

	printGCResult(result)
	os.Exit(exitOK)
}

func printGCResult(result browser.ChromeGCResult) {
	verb := "cleaned"
	if result.DryRun {
		verb = "would clean"
	}

	for _, act := range result.RegistryActions {
		if act.Action == "protected" {
			continue
		}
		fmt.Printf("registry  %-13s pid=%-7d kind=%-16s age=%ds  %s\n",
			act.Action, act.ChromePID, act.Kind, act.AgeSec, act.ProfileDir)
	}
	for _, proc := range result.Processes {
		if proc.Action == "protected" {
			continue
		}
		fmt.Printf("process   %-13s pid=%-7d pgid=%-7d age=%ds  %s\n",
			proc.Action, proc.PID, proc.PGID, proc.AgeSec, proc.ProfileDir)
	}
	for _, dir := range result.ProfileDirs {
		if dir.Action == "protected" {
			continue
		}
		fmt.Printf("profile   %-13s %s (%s, age=%ds)\n",
			dir.Action, dir.Path, humanBytes(dir.SizeBytes), dir.AgeSec)
	}

	protected := 0
	for _, act := range result.RegistryActions {
		if act.Action == "protected" {
			protected++
		}
	}
	for _, proc := range result.Processes {
		if proc.Action == "protected" {
			protected++
		}
	}
	for _, dir := range result.ProfileDirs {
		if dir.Action == "protected" {
			protected++
		}
	}

	fmt.Printf("\n%s: %d chrome process tree(s), %d profile dir(s), %s reclaimed; %d protected (live or younger than %ds)\n",
		verb, result.KilledProcesses, result.RemovedDirs, humanBytes(result.FreedBytes), protected, result.MinAgeSec)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func printGCUsage() {
	fmt.Println(`dw-browser gc — reclaim ownerless Chrome processes and stale profile dirs

Usage:
  dw-browser gc [--dry-run] [--older-than <dur>] [--all] [--json]

What it reclaims:
  1. Registered Chromes whose owning process is gone (the kill that a
     SIGKILLed owner never got to run).
  2. Chrome processes reparented to init whose --user-data-dir is a
     chromedp temp dir or a dw-browser profile root.
  3. /tmp/chromedp-runner* directories with no process still using them.

Never touched: anything a live session file references, anything younger
than the age floor, and any profile outside dw-browser/chromedp roots —
the Human's own Chrome is not gc's business.

Flags:
  --dry-run            report without killing or deleting
  --older-than <dur>   age floor (default 10m), e.g. 30s, 1h
  --all                no age floor; use when no launch is in flight
  --json               machine-readable report`)
}
