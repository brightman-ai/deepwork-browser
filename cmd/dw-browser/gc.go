package main

import (
	"fmt"
	"github.com/brightman-ai/deepwork-browser/browser"
	"time"
)

func runGCResult(args []string) (CommandResult, error) {
	options, e := parseDeclaredOptions(args, "gc")
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	minAge := browser.DefaultChromeGCMinAge
	if options["--all"] != "" {
		minAge = -1
	}
	if raw := options["--older-than"]; raw != "" {
		minAge, e = time.ParseDuration(raw)
		if e != nil || minAge < 0 {
			return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("invalid --older-than"))
		}
	}
	result := browser.RunChromeGC(browser.ChromeGCOptions{DryRun: options["--dry-run"] != "", MinAge: minAge})
	return commandSuccess(result), nil
}
