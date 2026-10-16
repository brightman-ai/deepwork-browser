package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func writeResultFile(path string, data []byte) error {
	tmp, e := os.CreateTemp(filepath.Dir(path), ".dw-result-*.json")
	if e != nil {
		return e
	}
	defer os.Remove(tmp.Name())
	if _, e = tmp.Write(data); e != nil {
		tmp.Close()
		return e
	}
	if e = tmp.Close(); e != nil {
		return e
	}
	return os.Rename(tmp.Name(), path)
}

// emitCLIResult is the only stdout/result-file boundary. A command verdict's
// classified Code is preserved; delivery/parameter/runtime errors cannot be 0.
func emitCLIResult(result CommandResult, err error, path string) int {
	code := result.Code
	payload := result.Payload
	if err != nil {
		var ce *CommandError
		if !errors.As(err, &ce) {
			ce = &CommandError{Phase: "runtime", DispatchState: "unknown", Recovery: "Inspect the session before retrying", Code: exitRunErr, Cause: err}
		}
		code = ce.Code
		if code == exitOK {
			code = exitRunErr
		}
		response := map[string]any{"success": false, "phase": ce.Phase, "dispatch_state": ce.DispatchState, "error": ce.Error(), "recovery": ce.Recovery}
		if payload != nil {
			response["partial_result"] = payload
		}
		payload = response
	}
	if payload == nil {
		payload = map[string]any{"success": code == exitOK, "delivery": "complete"}
	}
	deliveryFailure := func(cause error) []byte {
		response := map[string]any{"success": false, "phase": "delivery", "dispatch_state": "unknown", "error": cause.Error(), "recovery": "Inspect the runtime before repeating a mutating command"}
		if result.DeliveryFailurePayload != nil {
			response["partial_result"] = result.DeliveryFailurePayload(cause)
			response["recovery"] = "Inspect the diagnostic; body publication may already have committed. No body is advertised as usable by this failed delivery"
		}
		data, err := json.Marshal(response)
		if err != nil {
			data, _ = json.Marshal(map[string]any{"success": false, "phase": "delivery", "dispatch_state": "unknown", "error": cause.Error()})
		}
		return data
	}
	data, e := json.MarshalIndent(payload, "", "  ")
	if e != nil {
		code = exitRunErr
		data = deliveryFailure(e)
	}
	if path != "" {
		if e = writeResultFile(path, append(data, '\n')); e != nil {
			code = exitRunErr
			data = deliveryFailure(e)
		}
	}
	if _, e = fmt.Fprintln(os.Stdout, string(data)); e != nil {
		fmt.Fprintf(os.Stderr, "result delivery failed: %v\n", e)
		return exitRunErr
	}
	return code
}
func runCLI(argv []string) (code int) {
	configureCLIInternalLogging(argv)
	defer func() {
		if p := recover(); p != nil {
			code = emitCLIResult(CommandResult{}, dispatchCommandError(fmt.Errorf("unexpected runtime failure: %v", p)), "")
		}
	}()
	if len(argv) == 0 {
		return emitCLIResult(commandResult(generatedHelp(""), exitRunErr), nil, "")
	}
	if argv[0] == "--help" || argv[0] == "-h" {
		argv = append([]string{"help"}, argv[1:]...)
	}
	if argv[0] == "--version" {
		argv = append([]string{"version"}, argv[1:]...)
	}
	if argv[0] != "help" && wantsHelp(argv[1:]) {
		d, args, e := lookupCommand(argv)
		path := argv[0]
		infra := []string{}
		if e == nil {
			path = d.Path
			_, infra, _ = projectCommandArguments(d, args)
		}
		argv = append(append([]string{"help"}, strings.Fields(path)...), infra...)
	}

	def, args, e := lookupCommand(argv)
	if e == nil {
		e = validateCommandArgs(def, args)
	}
	if e != nil {
		return emitCLIResult(CommandResult{}, commandError("validation", exitRunErr, e), "")
	}
	clean, _, path := projectCommandArguments(def, args)
	if def.ValidateOutputs != nil {
		if e = def.ValidateOutputs(clean, path); e != nil {
			return emitCLIResult(CommandResult{}, commandError("validation", exitRunErr, e), "")
		}
	}
	if path != "" {
		probe, e := os.CreateTemp(filepath.Dir(path), ".dw-output-probe-*")
		if e != nil {
			return emitCLIResult(CommandResult{}, commandError("validation", exitRunErr, e), "")
		}
		probe.Close()
		os.Remove(probe.Name())
	}
	if e = acquireSessionOperatorLockE(argv); e != nil {
		return emitCLIResult(CommandResult{}, commandError("validation", exitRunErr, e), "")
	}
	defer func() {
		if heldSessionLock != nil {
			heldSessionLock.Release()
			heldSessionLock = nil
		}
	}()
	result, e := def.Handler(clean)
	return emitCLIResult(result, e, path)
}
