package testing

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var comparisonPattern = regexp.MustCompile(`^(==|!=|>=|<=|>|<)\s*\d+$`)
var quotedLiteralPattern = regexp.MustCompile(`^(?:'(?:\\.|[^'\\])*'|"(?:\\.|[^"\\])*")$`)

func validateAssertionSyntax(expression string) error {
	expr := strings.TrimSpace(expression)
	if m := simplePattern.FindStringSubmatch(expr); m != nil {
		switch m[1] {
		case "tab_count", "console_errors_count", "network_failures_count":
			return nil
		default:
			return fmt.Errorf("unsupported scalar assertion %q", m[1])
		}
	}
	m := exprPattern.FindStringSubmatch(expr)
	if m == nil {
		return fmt.Errorf("cannot parse assertion expression %q", expr)
	}
	name, args, comparison := m[1], strings.TrimSpace(m[2]), strings.TrimSpace(m[3])
	if _, ok := primitives[name]; !ok {
		return fmt.Errorf("unknown primitive %q", name)
	}
	count := name == "count" || name == "tab_count" || transcriptCountFuncs[name]
	if count {
		if !comparisonPattern.MatchString(comparison) {
			return fmt.Errorf("count assertion %s requires integer comparison", name)
		}
	} else if comparison != "" {
		return fmt.Errorf("unsupported comparison suffix for %s", name)
	}
	selectors := map[string]bool{"exists": true, "visible": true, "gone": true, "count": true}
	if selectors[name] {
		if args == "" && name == "count" {
			return nil
		}
		fields, err := strictKV(args)
		if err != nil {
			return err
		}
		if len(fields) == 0 {
			return fmt.Errorf("selector fields required")
		}
		for key := range fields {
			switch key {
			case "role", "name", "name_contains", "testid", "text":
			default:
				return fmt.Errorf("unsupported selector field %q", key)
			}
		}
		return nil
	}
	switch name {
	case "tab_count", "transcript_error_count", "transcript_done_count", "transcript_format_v1":
		if args != "" {
			return fmt.Errorf("%s accepts no arguments", name)
		}
	case "console_errors_count", "network_failures_count":
		return fmt.Errorf("%s requires scalar comparison form", name)
	case "latency_lt":
		value, err := strconv.ParseInt(args, 10, 64)
		if err != nil || value < 0 {
			return fmt.Errorf("latency_lt requires nonnegative milliseconds")
		}
	case "region_not_overlapped":
		parts := strings.Split(args, ",")
		if len(parts) != 2 || !quotedLiteralPattern.MatchString(strings.TrimSpace(parts[0])) || !quotedLiteralPattern.MatchString(strings.TrimSpace(parts[1])) {
			return fmt.Errorf("region_not_overlapped requires two quoted IDs")
		}
	case "probe_text_contains", "probe_json_has", "probe_json_count", "probe_value_equals_file":
		fields, err := strictKV(args)
		if err != nil {
			return err
		}
		if fields["id"] == "" {
			return fmt.Errorf("probe assertion requires id")
		}
		second := "path"
		if name == "probe_text_contains" {
			second = "text"
		}
		if name == "probe_value_equals_file" {
			second = "file"
			if !filepath.IsAbs(fields[second]) {
				return fmt.Errorf("expected value file requires an absolute path")
			}
		}
		if _, ok := fields[second]; !ok {
			return fmt.Errorf("probe assertion requires %s", second)
		}
		for key := range fields {
			if key != "id" && key != second {
				return fmt.Errorf("unsupported probe argument %s", key)
			}
		}
	default:
		if !quotedLiteralPattern.MatchString(args) {
			return fmt.Errorf("%s requires quoted literal argument", name)
		}
		if _, err := decodeAssertionLiteral(args); err != nil {
			return err
		}
		if name == "url_matches" {
			if _, err := regexp.Compile(stripQuotes(args)); err != nil {
				return fmt.Errorf("invalid URL regex: %w", err)
			}
		}
		if name == "file_glob_count" {
			if _, err := filepath.Match(stripQuotes(args), ""); err != nil {
				return fmt.Errorf("invalid glob: %w", err)
			}
		}
		if name == "transcript_has" && stripQuotes(args) != "done" {
			return fmt.Errorf("unsupported transcript state")
		}
	}
	return nil
}
func strictKV(args string) (map[string]string, error) {
	matches := argPattern.FindAllStringSubmatch(args, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("invalid selector arguments")
	}
	if strings.Trim(argPattern.ReplaceAllString(args, ""), " ,\t\r\n") != "" {
		return nil, fmt.Errorf("invalid argument syntax")
	}
	fields := map[string]string{}
	for _, match := range matches {
		if _, seen := fields[match[1]]; seen {
			return nil, fmt.Errorf("duplicate argument %s", match[1])
		}
		quoted := strings.SplitN(match[0], "=", 2)[1]
		value, err := decodeAssertionLiteral(quoted)
		if err != nil {
			return nil, err
		}
		fields[match[1]] = value
	}
	return fields, nil
}

func decodeAssertionLiteral(value string) (string, error) {
	if len(value) < 2 {
		return "", fmt.Errorf("quoted literal required")
	}
	if value[0] == '"' {
		return strconv.Unquote(value)
	}
	if value[0] != 39 || value[len(value)-1] != 39 {
		return "", fmt.Errorf("quoted literal required")
	}
	body := value[1 : len(value)-1]
	var out strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] != 92 {
			out.WriteByte(body[i])
			continue
		}
		i++
		if i >= len(body) {
			return "", fmt.Errorf("invalid literal escape")
		}
		switch body[i] {
		case 39, 34, 92:
			out.WriteByte(body[i])
		case 'n':
			out.WriteByte(10)
		case 'r':
			out.WriteByte(13)
		case 't':
			out.WriteByte(9)
		default:
			return "", fmt.Errorf("unsupported literal escape")
		}
	}
	return out.String(), nil
}
