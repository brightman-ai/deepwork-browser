package testing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
)

// LoadObservation accepts a canonical artifact or its explicit stdout reference.
// Arbitrary flat JSON cannot silently become a zero-valued Observation.
func LoadObservation(path string) (*Observation, error) { return loadObservation(path, false) }
func loadObservation(path string, referenced bool) (*Observation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var head struct {
		Schema string `json:"schema"`
		Path   string `json:"observation_path"`
		ID     string `json:"observation_id"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, err
	}
	if head.Schema == "dw.observe.ref.v2" {
		if referenced || head.Path == "" || head.ID == "" {
			return nil, fmt.Errorf("invalid observation reference")
		}
		target := head.Path
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		obs, err := loadObservation(target, true)
		if err != nil {
			return nil, err
		}
		if obs.ID != head.ID {
			return nil, fmt.Errorf("observation reference identity mismatch")
		}
		return obs, nil
	}
	if head.Schema != "dw.observe.v2" {
		return nil, fmt.Errorf("unsupported observation schema %q", head.Schema)
	}
	var obs Observation
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&obs); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("trailing observation data")
	}
	if obs.SessionID == "" || obs.Timestamp.IsZero() || obs.Page.URL == "" {
		return nil, fmt.Errorf("observation requires session_id, timestamp and page.url")
	}
	if obs.Schema == "dw.observe.v2" {
		if obs.ID == "" || len(obs.Channels) == 0 {
			return nil, fmt.Errorf("observation requires identity and channel states")
		}
		for name, c := range obs.Channels {
			if c.Status != "captured" && c.Status != "unavailable" && c.Status != "not-requested" {
				return nil, fmt.Errorf("invalid channel %s", name)
			}
		}
		for _, name := range []string{"structural", "visual", "behavior", "console", "network"} {
			c, ok := obs.Channels[name]
			if !ok || (c.Status != "captured" && c.Status != "unavailable" && c.Status != "not-requested") {
				return nil, fmt.Errorf("invalid channel %s", name)
			}
		}
		if obs.Channels["structural"].Status == "captured" && obs.Structural == nil {
			return nil, fmt.Errorf("captured structural evidence missing")
		}
		if obs.Structural != nil && obs.Structural.RefsCount != len(obs.Structural.Refs) {
			return nil, fmt.Errorf("structural refs_count does not match refs")
		}
		if obs.Channels["behavior"].Status == "captured" && obs.Behavior == nil {
			return nil, fmt.Errorf("captured behavior evidence missing")
		}
		if obs.Channels["console"].Status == "captured" && obs.Telemetry != nil && obs.Telemetry.ConsoleErrors == nil {
			return nil, fmt.Errorf("captured console events missing")
		}
		if obs.Channels["network"].Status == "captured" && obs.Telemetry != nil && obs.Telemetry.NetworkFailures == nil {
			return nil, fmt.Errorf("captured network events missing")
		}
		for _, name := range []string{"console", "network"} {
			if obs.Channels[name].Status == "captured" && obs.Telemetry == nil {
				return nil, fmt.Errorf("captured %s evidence missing", name)
			}
		}
	}
	if obs.Channels["transcript"].Status == "captured" && (obs.Transcript == nil || obs.Transcript.FilePath == "" || len(obs.Transcript.Events) == 0) {
		return nil, fmt.Errorf("captured transcript evidence missing")
	}
	for pattern, fact := range obs.Files {
		if pattern == "" || (fact.Status != "captured" && fact.Status != "unavailable" && fact.Status != "not-requested") || (fact.Status == "captured" && (fact.Count == nil || *fact.Count < 0)) {
			return nil, fmt.Errorf("invalid file evidence %q", pattern)
		}
	}

	if obs.Channels["content"].Status == "captured" {
		if obs.Content == nil {
			return nil, fmt.Errorf("captured content missing")
		}
		switch obs.Content.Format {
		case "text":
			if obs.Content.Text == nil {
				return nil, fmt.Errorf("captured text missing")
			}
		case "list":
			if obs.Content.Items == nil {
				return nil, fmt.Errorf("captured list missing")
			}
		case "table":
			if obs.Content.Tables == nil {
				return nil, fmt.Errorf("captured tables missing")
			}
		default:
			return nil, fmt.Errorf("unsupported content format")
		}
	}
	if obs.Visual != nil && obs.Visual.ScreenshotPath != "" {
		shot := obs.Visual.ScreenshotPath
		if !filepath.IsAbs(shot) {
			shot = filepath.Join(filepath.Dir(path), shot)
		}
		imageData, err := os.ReadFile(shot)
		if err != nil {
			return nil, fmt.Errorf("screenshot: %w", err)
		}
		if _, err = png.Decode(bytes.NewReader(imageData)); err != nil {
			return nil, fmt.Errorf("screenshot: %w", err)
		}
		obs.Visual.ScreenshotPath = shot
	} else if obs.Schema == "dw.observe.v2" && obs.Channels["visual"].Status == "captured" {
		return nil, fmt.Errorf("captured screenshot evidence missing")
	}
	return &obs, nil
}

func WriteObservation(path string, obs *Observation) error {
	data, err := json.MarshalIndent(obs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
