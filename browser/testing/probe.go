package testing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
)

// CollectProbe uses fixed fact readers. Instrumentation has an explicit policy
// callback and is always marked intervention; no arbitrary JS is accepted.
func CollectProbe(ctx context.Context, spec ProbeSpec, eval func(string, any) error, authorizeIntervention func() error, redaction ...func() (bool, error)) (ProbeFact, error) {
	fact := ProbeFact{ChannelState: ChannelState{Status: "captured"}}
	if err := spec.Validate(); err != nil {
		return fact, err
	}
	switch spec.Kind {
	case "http-json":
		requestCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutMs)*time.Millisecond)
		defer cancel()
		request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, spec.URL, nil)
		if err != nil {
			return fact, err
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return fact, err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return fact, err
		}
		fact.HTTPStatus = response.StatusCode
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return fact, fmt.Errorf("SUT probe HTTP status %d", response.StatusCode)
		}
		if !json.Valid(body) {
			return fact, fmt.Errorf("SUT probe body is not JSON")
		}
		fact.JSON = body
	case "css", "text":
		// An unbound host cannot prove its form-value privacy posture. Keep
		// values redacted unless an explicit latest-state provider says otherwise.
		redactValues := true
		if len(redaction) > 0 && redaction[0] != nil {
			var err error
			redactValues, err = redaction[0]()
			if err != nil {
				fact.ChannelState = ChannelState{Status: "unavailable", Reason: "probe privacy provenance unavailable"}
				return fact, fmt.Errorf("probe privacy provenance: %w", err)
			}
		}
		selector := spec.Selector
		if spec.Kind == "text" {
			selector = "body"
		}
		script := browser.ControlProbeReadScript(selector, redactValues)
		var result struct {
			Count int     `json:"count"`
			Text  *string `json:"text"`
			Value *string `json:"value"`
		}
		if err := eval(script, &result); err != nil {
			return fact, err
		}
		fact.Text = result.Text
		if spec.Kind == "css" {
			fact.Count = &result.Count
			fact.Value = result.Value
		}
	case "mutation-start", "mutation-collect":
		if authorizeIntervention == nil {
			return fact, fmt.Errorf("instrumentation policy unavailable")
		}
		if err := authorizeIntervention(); err != nil {
			return fact, err
		}
		script := mutationStartJS
		if spec.Kind == "mutation-collect" {
			script = mutationCollectJS
		}
		var count int
		if err := eval(script, &count); err != nil {
			return fact, err
		}
		fact.Count = &count
		fact.Intervention = true
	}
	return fact, nil
}

const mutationStartJS = `(()=>{if(globalThis.__dw_observer)throw new Error('mutation probe already active');globalThis.__dw_mutation_count=0;globalThis.__dw_observer=new MutationObserver(records=>{globalThis.__dw_mutation_count+=records.length});globalThis.__dw_observer.observe(document.documentElement,{subtree:true,childList:true,attributes:true,characterData:true});return 0;})()`
const mutationCollectJS = `(()=>{const observer=globalThis.__dw_observer;if(!observer)throw new Error('mutation probe was not started');const count=globalThis.__dw_mutation_count+observer.takeRecords().length;observer.disconnect();delete globalThis.__dw_observer;delete globalThis.__dw_mutation_count;return count;})()`

func probeFact(obs *Observation, args string) (ProbeFact, error) {
	id := stripQuotes(strings.TrimSpace(args))
	if parsed := parseKV(args); parsed["id"] != "" {
		id = parsed["id"]
	}
	fact, ok := obs.Probes[id]
	if !ok || fact.Status != "captured" {
		return fact, fmt.Errorf("BLOCKED: probe %q evidence unavailable", id)
	}
	return fact, nil
}
func evalProbeCount(obs *Observation, args, comparison string) (bool, string) {
	fact, err := probeFact(obs, args)
	if err != nil {
		return false, err.Error()
	}
	if fact.Count == nil {
		return false, "BLOCKED: probe count not captured"
	}
	return compareInt(int64(*fact.Count), comparison, "probe_count")
}
func evalProbeStatus(obs *Observation, args, comparison string) (bool, string) {
	fact, err := probeFact(obs, args)
	if err != nil {
		return false, err.Error()
	}
	if fact.HTTPStatus == 0 {
		return false, "BLOCKED: HTTP status not captured"
	}
	return compareInt(int64(fact.HTTPStatus), comparison, "probe_http_status")
}
func evalProbeTextContains(obs *Observation, args string) (bool, string) {
	fact, err := probeFact(obs, args)
	if err != nil {
		return false, err.Error()
	}
	text, ok := parseKV(args)["text"]
	if !ok {
		return false, "BLOCKED: probe_text_contains requires id/text"
	}
	if fact.Text == nil {
		return false, "BLOCKED: probe text not captured or protected"
	}
	return strings.Contains(*fact.Text, text), fmt.Sprintf("probe text contains %q: %t", text, strings.Contains(*fact.Text, text))
}
func probeJSONValue(obs *Observation, args string) (any, error) {
	fact, err := probeFact(obs, args)
	if err != nil {
		return nil, err
	}
	if len(fact.JSON) == 0 {
		return nil, fmt.Errorf("BLOCKED: JSON probe evidence unavailable")
	}
	var value any
	if err := json.Unmarshal(fact.JSON, &value); err != nil {
		return nil, fmt.Errorf("BLOCKED: invalid JSON probe")
	}
	path, ok := parseKV(args)["path"]
	if !ok || path == "" {
		return nil, fmt.Errorf("BLOCKED: JSON probe requires path")
	}
	for _, segment := range strings.Split(path, ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("BLOCKED: JSON path %q parent is not object", path)
		}
		value, ok = object[segment]
		if !ok {
			return nil, fmt.Errorf("BLOCKED: JSON path %q missing", path)
		}
	}
	return value, nil
}
func evalProbeJSONHas(obs *Observation, args string) (bool, string) {
	_, err := probeJSONValue(obs, args)
	if err != nil {
		return false, err.Error()
	}
	return true, "declared JSON path captured"
}
func evalProbeJSONCount(obs *Observation, args, comparison string) (bool, string) {
	value, err := probeJSONValue(obs, args)
	if err != nil {
		return false, err.Error()
	}
	array, ok := value.([]any)
	if !ok {
		return false, "BLOCKED: declared JSON path is not array"
	}
	return compareInt(int64(len(array)), comparison, "probe_json_count")
}

func evalProbeValueEqualsFile(obs *Observation, args string) (bool, string) {
	fact, err := probeFact(obs, args)
	if err != nil {
		return false, err.Error()
	}
	if fact.Value == nil {
		return false, "BLOCKED: control value not captured (single nonsecret control required)"
	}
	file := parseKV(args)["file"]
	data, err := os.ReadFile(file)
	if err != nil {
		return false, "BLOCKED: expected value file unavailable"
	}
	return *fact.Value == string(data), "control value compared with explicit expected file"
}
