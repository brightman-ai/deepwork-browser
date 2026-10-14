package testing

import (
	"fmt"
	"github.com/brightman-ai/deepwork-browser/browser"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func DecodeYAML(data []byte, target any) error {
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing YAML documents or malformed content")
	}
	return nil
}

var journeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validID(id string) bool { return journeyIDPattern.MatchString(id) }

func ValidateAssertion(expr string) error { return validateAssertionSyntax(strings.TrimSpace(expr)) }
func (w WaitSpec) Validate() error {
	if w.DurationMs > 0 && w.Assert == "" && w.TimeoutMs == 0 {
		return nil
	}
	if w.DurationMs != 0 || w.Assert == "" || w.TimeoutMs <= 0 {
		return fmt.Errorf("wait requires assert+positive timeout_ms or positive duration_ms")
	}
	return ValidateAssertion(w.Assert)
}
func (p ProbeSpec) Validate() error {
	if !validID(p.ID) {
		return fmt.Errorf("probe requires safe unique id")
	}
	switch p.Kind {
	case "css":
		if p.Selector == "" || p.URL != "" {
			return fmt.Errorf("css probe requires selector and no URL")
		}
	case "text", "mutation-start", "mutation-collect":
		if p.Selector != "" || p.URL != "" {
			return fmt.Errorf("%s probe accepts no selector/URL", p.Kind)
		}
	case "http-json":
		if !(strings.HasPrefix(p.URL, "http://") || strings.HasPrefix(p.URL, "https://")) || p.TimeoutMs <= 0 || p.Selector != "" {
			return fmt.Errorf("http-json requires explicit HTTP URL and positive timeout_ms")
		}
	default:
		return fmt.Errorf("unsupported probe kind %q", p.Kind)
	}
	if p.Kind != "http-json" && p.TimeoutMs != 0 {
		return fmt.Errorf("timeout_ms unavailable for %s probe", p.Kind)
	}
	return nil
}
func (s *JourneySpec) Validate() error {
	if s == nil || s.Version != 2 || !validID(s.ID) || len(s.Journey) == 0 {
		return fmt.Errorf("journey requires version:2, safe id and nonempty journey")
	}
	if s.Environment.Mode != "" && s.Environment.Mode != "headless" && s.Environment.Mode != "headed" && s.Environment.Mode != "visible" {
		return fmt.Errorf("unsupported environment mode %q", s.Environment.Mode)
	}
	if s.Environment.Client != "" && s.Environment.Client != "chrome" && s.Environment.Client != "safari" {
		return fmt.Errorf("unsupported environment client %q", s.Environment.Client)
	}
	if s.Environment.Viewport != "" {
		if _, _, err := parseViewportSize(s.Environment.Viewport); err != nil {
			return err
		}
	}
	if s.Environment.TranscriptPath != "" && s.Environment.TranscriptDir != "" {
		return fmt.Errorf("transcript_path and transcript_dir are exclusive")
	}
	if s.Environment.TranscriptPattern != "" && s.Environment.TranscriptDir == "" {
		return fmt.Errorf("transcript_pattern requires explicit directory")
	}
	seen := map[string]bool{}
	for _, steps := range [][]StepSpec{s.Journey, s.Recovery} {
		for _, step := range steps {
			if !validID(step.ID) || seen[step.ID] {
				return fmt.Errorf("step id missing, unsafe or duplicate: %q", step.ID)
			}
			seen[step.ID] = true
			if _, err := browser.NormalizeInteractionMode(string(step.Mode)); err != nil {
				return err
			}

			switch step.Kind {
			case "action":
				if step.Action == nil {
					return fmt.Errorf("action step %s requires structured action", step.ID)
				}
			case "observe":
				if step.Action != nil {
					return fmt.Errorf("observe cannot dispatch action")
				}
			case "wait":
				if step.Wait == nil || step.Action != nil {
					return fmt.Errorf("wait step requires wait only")
				}
			case "check":
				if len(step.Check) == 0 || step.Action != nil {
					return fmt.Errorf("check step requires checks and no action")
				}
			case "probe":
				if len(step.Probes) == 0 || step.Action != nil {
					return fmt.Errorf("probe step requires probes and no action")
				}
			default:
				return fmt.Errorf("unsupported step kind %q", step.Kind)
			}
			if step.Kind != "action" && step.Mode != "" {
				return fmt.Errorf("interaction mode requires action step")
			}
			if step.TextFile != "" && (step.Action == nil || (step.Action.Verb != "fill" && step.Action.Verb != "type") || step.Action.TextWasProvided()) {
				return fmt.Errorf("text_file requires fill/type without inline text")
			}
			if step.Action != nil {
				if err := validatePersistentPoints(step.Action.Target.Point, step.Action.End); err != nil {
					return fmt.Errorf("step %s: %w", step.ID, err)
				}
				if step.Action.Target.Kind == "ref" {
					return fmt.Errorf("persistent journeys require locator or point targets, not observation refs")
				}
				request := *step.Action
				if step.TextFile != "" {
					request = request.WithText("")
				}
				if err := request.ValidateTemplate(); err != nil {
					return fmt.Errorf("step %s: %w", step.ID, err)
				}
				if step.Action.Sensitive && step.Action.Text != "" {
					return fmt.Errorf("sensitive journey text must use text_file")
				}
			}
			if step.Wait != nil {
				if err := step.Wait.Validate(); err != nil {
					return fmt.Errorf("step %s: %w", step.ID, err)
				}
			}
			probeIDs := map[string]bool{}
			for _, p := range step.Probes {
				if err := p.Validate(); err != nil {
					return err
				}
				if probeIDs[p.ID] {
					return fmt.Errorf("duplicate probe %s", p.ID)
				}
				probeIDs[p.ID] = true
			}
			for _, check := range step.Check {
				if err := ValidateAssertion(check.Assert); err != nil {
					return err
				}
			}
		}
	}
	if s.Baseline != nil {
		for _, check := range s.Baseline.Invariants {
			if err := ValidateAssertion(check.Assert); err != nil {
				return err
			}
		}
	}
	for _, m := range s.Mutations {
		switch m.Kind {
		case "back-forward", "reload":
			if m.Width != 0 || m.Height != 0 || m.Target.Kind != "" {
				return fmt.Errorf("unexpected fields for mutation %s", m.Kind)
			}
		case "viewport":
			if m.Width <= 0 || m.Height <= 0 || m.Target.Kind != "" {
				return fmt.Errorf("viewport mutation requires positive width/height")
			}
		case "double-click":
			if err := validatePersistentPoints(m.Target.Point); err != nil {
				return err
			}
			if err := (browser.Action{Verb: "click", Target: m.Target, Count: 2}).ValidateTemplate(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported mutation kind %q", m.Kind)
		}
	}
	for _, name := range s.Evidence.Save {
		if name != "observation" && name != "screenshot" && name != "trace" && name != "report" && name != "replay" {
			return fmt.Errorf("unsupported evidence field %q", name)
		}
	}
	return nil
}

func validatePersistentPoints(points ...*browser.Point) error {
	for _, point := range points {
		if point != nil && point.ObservationID != "" {
			return fmt.Errorf("persistent coordinates cannot contain a runtime observation_id")
		}
	}
	return nil
}
func LoadSpec(path string) (*JourneySpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var spec JourneySpec
	if err := DecodeYAML(data, &spec); err != nil {
		return nil, fmt.Errorf("parse spec %s: %w", path, err)
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	for _, value := range []*string{&spec.Environment.WorkspaceDir, &spec.Environment.TranscriptPath, &spec.Environment.TranscriptDir} {
		if *value != "" && !filepath.IsAbs(*value) {
			*value = filepath.Join(base, *value)
		}
	}
	for _, steps := range [][]StepSpec{spec.Journey, spec.Recovery} {
		for i := range steps {
			if steps[i].TextFile != "" && !filepath.IsAbs(steps[i].TextFile) {
				steps[i].TextFile = filepath.Join(base, steps[i].TextFile)
			}
		}
	}
	if spec.Baseline != nil {
		for i, path := range spec.Baseline.Files {
			if !filepath.IsAbs(path) {
				path = filepath.Join(base, path)
			}
			spec.Baseline.Files[i] = path
			if _, err := LoadObservation(path); err != nil {
				return nil, fmt.Errorf("baseline reference %s: %w", path, err)
			}
		}
	}
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("spec %s: %w", path, err)
	}
	return &spec, nil
}
