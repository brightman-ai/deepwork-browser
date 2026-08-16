package testing

import (
	"fmt"

	"github.com/brightman-ai/deepwork-browser/browser"
	"gopkg.in/yaml.v3"
)

// MutationStep is a mutation directive parsed from the spec. It tolerates both
// scalar form ("- back_forward_recovery") and mapping form ("- viewport: '1280x800'").
// NOTE: mutations are parsed but NOT yet executed by the journey runner.
type MutationStep struct {
	Name   string            `json:"name"`
	Params map[string]string `json:"params,omitempty"`
}

func (m *MutationStep) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		m.Name = node.Value
		return nil
	case yaml.MappingNode:
		raw := map[string]string{}
		if err := node.Decode(&raw); err != nil {
			return err
		}
		m.Params = raw
		for k := range raw { // first key becomes the mutation name
			m.Name = k
			break
		}
		return nil
	default:
		return fmt.Errorf("mutation: unsupported YAML node kind %d", node.Kind)
	}
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
	BaseURL      string `yaml:"base_url" json:"base_url"`
	EntryURL     string `yaml:"entry_url,omitempty" json:"entry_url,omitempty"`
	Mode         string `yaml:"mode" json:"mode"`                             // "headless" | "headed"
	Viewport     string `yaml:"viewport,omitempty" json:"viewport,omitempty"` // "1440x900"
	Client       string `yaml:"client,omitempty" json:"client,omitempty"`     // "chrome" | "safari"
	CleanSession bool   `yaml:"clean_session,omitempty" json:"clean_session,omitempty"`
	WorkspaceDir string `yaml:"workspace_dir,omitempty" json:"workspace_dir,omitempty"` // base dir for file_glob assertions (default: cwd)
}

// BaselineRef — 基线引用（快照文件 + 不变量断言）
type BaselineRef struct {
	Files      []string        `yaml:"files,omitempty" json:"files,omitempty"`
	Invariants []AssertionSpec `yaml:"invariants,omitempty" json:"invariants,omitempty"`
}

// StepSpec — 单步旅程规格
type StepSpec struct {
	ID    string                  `yaml:"id" json:"id"`
	Do    string                  `yaml:"do" json:"do"`
	Mode  browser.InteractionMode `yaml:"mode,omitempty" json:"mode,omitempty"`
	Wait  *WaitSpec               `yaml:"wait,omitempty" json:"wait,omitempty"`
	Check []AssertionSpec         `yaml:"check,omitempty" json:"check,omitempty"`
}

// WaitSpec — 等待条件
type WaitSpec struct {
	Until     string `yaml:"until" json:"until"`
	TimeoutMs int    `yaml:"timeout_ms" json:"timeout_ms"`
}

// AssertionSpec — 断言规格
type AssertionSpec struct {
	ID       string   `yaml:"id,omitempty" json:"id,omitempty"`
	Assert   string   `yaml:"assert" json:"assert"`
	Using    []string `yaml:"using,omitempty" json:"using,omitempty"`
	Required bool     `yaml:"required,omitempty" json:"required,omitempty"`
	// Spec governance fields
	SourceSpec string `yaml:"source_spec,omitempty" json:"source_spec,omitempty"`
	CoversBug  string `yaml:"covers_bug,omitempty" json:"covers_bug,omitempty"`
	Runtime    string `yaml:"runtime,omitempty" json:"runtime,omitempty"` // "chrome" | "wails" | "all"
}

// EvidenceSpec — 证据持久化配置
type EvidenceSpec struct {
	Save []string `yaml:"save,omitempty" json:"save,omitempty"`
}
