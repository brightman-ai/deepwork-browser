package testing

import (
	"encoding/json"
	"github.com/brightman-ai/deepwork-browser/browser"
)

// MutationStep is an explicit deterministic perturbation of an accepted journey.
type MutationStep struct {
	Kind   string         `yaml:"kind" json:"kind"`
	Width  int            `yaml:"width,omitempty" json:"width,omitempty"`
	Height int            `yaml:"height,omitempty" json:"height,omitempty"`
	Target browser.Target `yaml:"target,omitempty" json:"target,omitempty"`
}

// JourneySpec — BDD YAML 解析目标
type JourneySpec struct {
	Version     int             `yaml:"version" json:"version"`
	ID          string          `yaml:"id" json:"id"`
	Name        string          `yaml:"name" json:"name"`
	App         string          `yaml:"app,omitempty" json:"app,omitempty"`
	Portal      string          `yaml:"portal,omitempty" json:"portal,omitempty"`
	RiskTags    []string        `yaml:"risk_tags,omitempty" json:"risk_tags,omitempty"`
	Environment EnvironmentSpec `yaml:"environment" json:"environment"`
	Baseline    *BaselineRef    `yaml:"baseline,omitempty" json:"baseline,omitempty"`
	Journey     []StepSpec      `yaml:"journey" json:"journey"`
	Recovery    []StepSpec      `yaml:"recovery,omitempty" json:"recovery,omitempty"`
	Mutations   []MutationStep  `yaml:"mutations,omitempty" json:"mutations,omitempty"`
	Evidence    EvidenceSpec    `yaml:"evidence" json:"evidence"`
}

// EnvironmentSpec — 运行环境配置
type EnvironmentSpec struct {
	TranscriptPath    string `yaml:"transcript_path,omitempty" json:"transcript_path,omitempty"`
	TranscriptDir     string `yaml:"transcript_dir,omitempty" json:"transcript_dir,omitempty"`
	TranscriptPattern string `yaml:"transcript_pattern,omitempty" json:"transcript_pattern,omitempty"`
	BaseURL           string `yaml:"base_url" json:"base_url"`
	EntryURL          string `yaml:"entry_url,omitempty" json:"entry_url,omitempty"`
	Mode              string `yaml:"mode" json:"mode"`                             // "headless" | "headed"
	Viewport          string `yaml:"viewport,omitempty" json:"viewport,omitempty"` // "1440x900"
	Client            string `yaml:"client,omitempty" json:"client,omitempty"`     // "chrome" | "safari"
	CleanSession      bool   `yaml:"clean_session,omitempty" json:"clean_session,omitempty"`
	WorkspaceDir      string `yaml:"workspace_dir,omitempty" json:"workspace_dir,omitempty"` // base dir for file_glob assertions (default: cwd)
}

// BaselineRef — 基线引用（快照文件 + 不变量断言）
type BaselineRef struct {
	Files      []string        `yaml:"files,omitempty" json:"files,omitempty"`
	Invariants []AssertionSpec `yaml:"invariants,omitempty" json:"invariants,omitempty"`
}

// StepSpec — 单步旅程规格
type StepSpec struct {
	ID       string                  `yaml:"id" json:"id"`
	Kind     string                  `yaml:"kind" json:"kind"` // action | observe | wait | check | probe
	Action   *browser.Action         `yaml:"action,omitempty" json:"action,omitempty"`
	TextFile string                  `yaml:"text_file,omitempty" json:"text_file,omitempty"`
	Mode     browser.InteractionMode `yaml:"mode,omitempty" json:"mode,omitempty"`
	Wait     *WaitSpec               `yaml:"wait,omitempty" json:"wait,omitempty"`
	Check    []AssertionSpec         `yaml:"check,omitempty" json:"check,omitempty"`
	Probes   []ProbeSpec             `yaml:"probes,omitempty" json:"probes,omitempty"`
}

// WaitSpec has one condition kernel and an explicit cancelable duration alternative.
type WaitSpec struct {
	Assert     string `yaml:"assert,omitempty" json:"assert,omitempty"`
	TimeoutMs  int    `yaml:"timeout_ms,omitempty" json:"timeout_ms,omitempty"`
	DurationMs int    `yaml:"duration_ms,omitempty" json:"duration_ms,omitempty"`
}

// ProbeSpec declares the SUT fact to collect; endpoints are never inferred.
type ProbeSpec struct {
	ID        string `yaml:"id" json:"id"`
	Kind      string `yaml:"kind" json:"kind"` // css | text | http-json | mutation-start | mutation-collect
	Selector  string `yaml:"selector,omitempty" json:"selector,omitempty"`
	URL       string `yaml:"url,omitempty" json:"url,omitempty"`
	TimeoutMs int    `yaml:"timeout_ms,omitempty" json:"timeout_ms,omitempty"`
}

// AssertionSpec — 断言规格
type AssertionSpec struct {
	ID       string   `yaml:"id,omitempty" json:"id,omitempty"`
	Assert   string   `yaml:"assert" json:"assert"`
	Using    []string `yaml:"using,omitempty" json:"using,omitempty"`
	Required *bool    `yaml:"required,omitempty" json:"required,omitempty"`
	// Spec governance fields
	SourceSpec string `yaml:"source_spec,omitempty" json:"source_spec,omitempty"`
	CoversBug  string `yaml:"covers_bug,omitempty" json:"covers_bug,omitempty"`
	Runtime    string `yaml:"runtime,omitempty" json:"runtime,omitempty"` // "chrome" | "wails" | "all"
}

// EvidenceSpec — 证据持久化配置
type EvidenceSpec struct {
	Save []string `yaml:"save,omitempty" json:"save,omitempty"`
}

// MarshalYAML keeps a file binding separate from the typed request text field.
func (s StepSpec) MarshalYAML() (any, error) {
	type wire StepSpec
	if s.TextFile == "" {
		return wire(s), nil
	}
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if action, ok := fields["action"].(map[string]any); ok {
		delete(action, "text")
	}
	return fields, nil
}
