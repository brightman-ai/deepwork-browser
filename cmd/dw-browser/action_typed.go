package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/brightman-ai/deepwork-browser/browser"
)

func parseTypedActionArgs(args []string) (browser.Action, []string, error) {
	a := browser.Action{}
	var rest, pos []string
	textSeen, fileSeen := false, false
	var optionLabel, optionValue *string
	observationID := ""
	provided := map[string]bool{}
	fail := func(err error) (browser.Action, []string, error) { return a, nil, err }
	for i := 0; i < len(args); i++ {
		item := args[i]
		if item == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		key, value, inline := strings.Cut(item, "=")
		take := func() (string, error) {
			if inline {
				return value, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", key)
			}
			i++
			return args[i], nil
		}
		if (key == "--label" || key == "--value") && provided[key] {
			return fail(fmt.Errorf("duplicate %s", key))
		}
		provided[key] = true
		switch key {
		case "--focused":
			a.Focused = true
		case "--sensitive":
			a.Sensitive = true
		case "--text", "--text-file", "--url", "--key", "--label", "--value", "--button", "--count", "--checked", "--direction", "--steps", "--x", "--y", "--end-x", "--end-y", "--unit", "--frame", "--delta-x", "--delta-y", "--observation-id":
			v, err := take()
			if err != nil {
				return fail(err)
			}
			switch key {
			case "--observation-id":
				observationID = v
			case "--text":
				if textSeen {
					return fail(fmt.Errorf("duplicate --text"))
				}
				textSeen = true
				a.Text = v
			case "--text-file":
				if fileSeen {
					return fail(fmt.Errorf("duplicate --text-file"))
				}
				fileSeen = true
				b, e := os.ReadFile(v)
				if e != nil {
					return fail(e)
				}
				if !utf8.Valid(b) {
					return fail(fmt.Errorf("text file must be UTF-8"))
				}
				a.Text = string(b)
			case "--url":
				a.URL = v
			case "--key":
				a.Key = v
			case "--label":
				optionLabel = &v
			case "--value":
				optionValue = &v
			case "--button":
				a.Button = v
			case "--direction":
				a.Direction = v
			case "--checked":
				b, e := strconv.ParseBool(v)
				if e != nil {
					return fail(e)
				}
				a.Checked = &b
			case "--count", "--steps":
				n, e := strconv.Atoi(v)
				if e != nil {
					return fail(e)
				}
				if key == "--count" {
					a.Count = n
				} else {
					a.Steps = n
				}
			case "--unit", "--frame":
				if a.Target.Point == nil {
					a.Target.Point = &browser.Point{Unit: "css-px", Frame: "viewport"}
				}
				if key == "--unit" {
					a.Target.Point.Unit = v
				} else {
					a.Target.Point.Frame = v
				}
			default:
				n, e := strconv.ParseFloat(v, 64)
				if e != nil {
					return fail(e)
				}
				switch key {
				case "--delta-x":
					a.DeltaX = n
				case "--delta-y":
					a.DeltaY = n
				case "--end-x", "--end-y":
					if a.End == nil {
						a.End = &browser.Point{}
					}
					if key == "--end-x" {
						a.End.X = n
					} else {
						a.End.Y = n
					}
				default:
					if a.Target.Point == nil {
						a.Target.Point = &browser.Point{Unit: "css-px", Frame: "viewport"}
					}
					if key == "--x" {
						a.Target.Point.X = n
					} else {
						a.Target.Point.Y = n
					}
				}
			}
		case "--id":
			rest = append(rest, item)
			if !inline {
				v, e := take()
				if e != nil {
					return fail(e)
				}
				rest = append(rest, v)
			}
		default:
			if strings.HasPrefix(item, "-") {
				return fail(fmt.Errorf("unknown act flag %s", key))
			}
			pos = append(pos, item)
		}
	}
	if len(pos) < 1 || len(pos) > 2 {
		return fail(fmt.Errorf("act requires verb and optional target"))
	}
	a.Verb = pos[0]
	if a.Verb == "select" {
		a.Option = &browser.SelectOptionSelector{Label: optionLabel, Value: optionValue}
	} else {
		if optionLabel != nil {
			return fail(fmt.Errorf("--label is only available for select"))
		}
		if optionValue != nil {
			a.Value = *optionValue
		}
	}
	if len(pos) == 2 {
		if strings.HasPrefix(pos[1], "@") {
			a.Target.Kind = "ref"
			a.Target.Ref = pos[1]
		} else {
			a.Target.Kind = "locator"
			a.Target.Locator = pos[1]
		}
	}
	if a.Target.Point != nil && a.Target.Kind == "" {
		a.Target.Kind = "point"
	}
	if observationID != "" && a.Target.Point == nil {
		return fail(fmt.Errorf("--observation-id requires a point"))
	}
	if a.Target.Point != nil {
		a.Target.Point.ObservationID = observationID
	}
	if a.End != nil && a.Target.Point != nil {
		a.End.ObservationID = observationID
		a.End.Unit = a.Target.Point.Unit
		a.End.Frame = a.Target.Point.Frame
	}
	if a.Target.Point != nil && (!provided["--x"] || !provided["--y"]) {
		return fail(fmt.Errorf("point requires explicit --x and --y"))
	}
	if a.End != nil && (!provided["--end-x"] || !provided["--end-y"]) {
		return fail(fmt.Errorf("gesture endpoint requires --end-x and --end-y"))
	}
	if textSeen && fileSeen {
		return fail(fmt.Errorf("--text and --text-file are mutually exclusive"))
	}
	if (a.Verb == "fill" || a.Verb == "type") && !textSeen && !fileSeen {
		return fail(fmt.Errorf("text action requires --text or --text-file"))
	}
	if !utf8.ValidString(a.Text) {
		return fail(fmt.Errorf("text must be UTF-8"))
	}
	if e := a.Validate(); e != nil {
		return fail(e)
	}
	return a, rest, nil
}

func actionErrorPayload(out browser.Outcome, err error) map[string]any {
	return map[string]any{"success": false, "outcome": out, "error": err.Error()}
}
