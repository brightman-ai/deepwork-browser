package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// CommandResult separates delivered command data from product verdicts. Code
// is the classified exit status; errors retain phase/dispatch/recovery facts.
type CommandResult struct {
	DeliveryFailurePayload func(error) any `json:"-"`
	Payload                any
	Code                   int
	cause                  error
}

func commandResult(payload any, code int) CommandResult {
	return CommandResult{Payload: payload, Code: code}
}
func commandSuccess(payload any) CommandResult { return commandResult(payload, exitOK) }

type CommandError struct {
	Phase         string
	DispatchState string
	Recovery      string
	Code          int
	Cause         error
}

func (e *CommandError) Error() string { return e.Cause.Error() }
func (e *CommandError) Unwrap() error { return e.Cause }
func commandError(phase string, code int, cause error) error {
	if cause == nil {
		cause = fmt.Errorf("command could not complete")
	}
	return &CommandError{Phase: phase, DispatchState: "not-dispatched", Code: code, Cause: cause}
}

func finishCommand(result CommandResult, code int) (CommandResult, error) {
	result.Code = code
	if code != exitOK && result.Payload == nil {
		return result, &CommandError{Phase: "runtime", DispatchState: "unknown", Recovery: "Inspect the existing session before retrying", Code: code, Cause: commandCause(result)}
	}
	return result, nil
}

func dispatchCommandError(e error) error {
	return &CommandError{Phase: "dispatch", DispatchState: "unknown", Recovery: "Observe the existing session before deciding whether to retry", Code: exitRunErr, Cause: e}
}
func deliveryCommandError(e error) error {
	return &CommandError{Phase: "delivery", DispatchState: "unknown", Recovery: "Inspect the existing runtime and recover its persisted metadata", Code: exitRunErr, Cause: e}
}

func printJSON(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }

func (r *CommandResult) appendContent(text string) {
	content := ""
	if old, ok := r.Payload.(map[string]any); ok {
		content, _ = old["content"].(string)
	}
	r.Payload = map[string]any{"content": content + text}
}

func (r *CommandResult) note(format string, args ...any) {
	e := fmt.Errorf(format, args...)
	if r.cause == nil {
		r.cause = e
	}
	fmt.Fprint(os.Stderr, e.Error())
}
func commandCause(r CommandResult) error {
	if r.cause != nil {
		return r.cause
	}
	return fmt.Errorf("command did not complete")
}
