package main

import (
	"encoding/json"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
)

func runExportContractResult(args []string) (CommandResult, error) {
	options, e := parseDeclaredOptions(args, "export-contract")
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	root := options["--directory"]
	if e = os.MkdirAll(filepath.Join(root, "dw-browser", "references"), 0755); e != nil {
		return CommandResult{}, deliveryCommandError(e)
	}
	files, e := generatedContractArtifacts()
	if e != nil {
		return CommandResult{}, deliveryCommandError(e)
	}
	for name, data := range files {
		if e = writeResultFile(filepath.Join(root, name), data); e != nil {
			return CommandResult{}, deliveryCommandError(e)
		}
	}
	return commandSuccess(map[string]any{"version": version, "directory": root, "files": []string{"capabilities.json", "dw-browser/SKILL.md", "dw-browser/references/commands.md", "dw-browser/references/journey.yaml", "completion.zsh", "completion.bash"}}), nil
}

func generatedContractArtifacts() (map[string][]byte, error) {
	capJSON, e := json.MarshalIndent(capabilityDocument(), "", "  ")
	if e != nil {
		return nil, e
	}
	sample, e := yaml.Marshal(exampleJourneySpec())
	if e != nil {
		return nil, e
	}
	zsh, e := generatedCompletion("zsh")
	if e != nil {
		return nil, e
	}
	bash, e := generatedCompletion("bash")
	if e != nil {
		return nil, e
	}
	files := map[string][]byte{
		"capabilities.json":                  append(capJSON, '\n'),
		"dw-browser/SKILL.md":                []byte(generatedProtocol()),
		"dw-browser/references/commands.md":  []byte(generatedCommandReference()),
		"dw-browser/references/journey.yaml": sample,
		"completion.zsh":                     []byte(zsh),
		"completion.bash":                    []byte(bash),
	}
	return files, nil
}
