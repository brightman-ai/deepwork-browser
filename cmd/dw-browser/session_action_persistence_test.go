package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/brightman-ai/deepwork-browser/browser"
	btest "github.com/brightman-ai/deepwork-browser/browser/testing"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func persistedActionFixture(t *testing.T) *browser.SessionInfo {
	t.Helper()
	info := &browser.SessionInfo{SessionID: fmt.Sprintf("journey-fence-%d", time.Now().UnixNano()), AuthorityState: browser.AuthorityAgentic, PageURL: "https://example.test", BrowserRunID: "run", TargetID: "target", ViewportW: 1280, ViewportH: 800, ObservationID: "capture", ObservationValid: true, ObservationPageURL: "https://example.test", ObservationRunID: "run", ObservationTargetID: "target", ObservationViewportW: 1280, ObservationViewportH: 800, LastActionOutcome: browser.SessionActionOutcomeReconciled, Refs: []browser.SessionRef{{Ref: "@ocapture-r1", Observed: true}}}
	info.WSURL = "ws://127.0.0.1/devtools/browser/" + info.SessionID
	if err := browser.SaveSession(info); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := browser.DeleteSession(info.SessionID); err != nil {
			t.Error(err)
		}
	})
	return info
}
func TestJourneyActionPersistsFenceAndCanonicalOutcome(t *testing.T) {
	for _, state := range []string{"confirmed", "not-dispatched", "unknown"} {
		t.Run(state, func(t *testing.T) {
			info := persistedActionFixture(t)
			operation := ""
			request := browser.Action{Verb: "fill", Target: browser.Target{Kind: "locator", Locator: "css=#editor"}, Text: "secret must never persist", Sensitive: true}
			out, err := executePersistedSessionAction(info, request, func(id string) { operation = id }, func() (*browser.Snapshot, browser.Outcome, browser.ActionFidelityReport, error) {
				disk, e := browser.LoadSession(info.SessionID)
				if e != nil {
					t.Fatal(e)
				}
				if operation == "" || disk.LastActionOperationID != operation || disk.LastActionOutcome != browser.SessionActionOutcomeInProgress || len(disk.Refs) != 0 || !disk.RedactFormValues {
					t.Fatalf("dispatch preceded durable fence: %+v", disk)
				}
				raw, e := os.ReadFile(filepath.Join("/tmp/dw-browser-sessions", info.SessionID+".json"))
				if e != nil {
					t.Fatal(e)
				}
				if strings.Contains(string(raw), request.Text) {
					t.Fatal("plaintext sensitive text persisted")
				}
				var failure error
				if state != "confirmed" {
					failure = errors.New("canonical core outcome")
				}
				return nil, browser.Outcome{Phase: "dispatch", DispatchState: state}, browser.ActionFidelityReport{}, failure
			})
			if out.DispatchState != state || (state == "confirmed" && err != nil) || (state != "confirmed" && err == nil) {
				t.Fatalf("outcome mismatch: %+v %v", out, err)
			}
			disk, e := browser.LoadSession(info.SessionID)
			if e != nil {
				t.Fatal(e)
			}
			if !disk.RedactFormValues {
				t.Fatal("sensitive posture lost")
			}
			if state == "not-dispatched" {
				if !disk.ObservationValid || len(disk.Refs) != 1 || disk.LastActionOutcome != browser.SessionActionOutcomeReconciled {
					t.Fatalf("validation failure revoked unchanged observation: %+v", disk)
				}
			} else {
				if disk.ObservationValid || len(disk.Refs) != 0 {
					t.Fatalf("terminal input left stale geometry: %+v", disk)
				}
			}
			if state == "unknown" && disk.LastActionOutcome != browser.SessionActionOutcomeUnknown {
				t.Fatal("unknown delivery became confirmed")
			}
		})
	}
}

func TestJourneyActionRejectsPersistedHumanAndUncertainty(t *testing.T) {
	for _, state := range []string{browser.AuthorityHuman, browser.SessionActionOutcomeUnknown, browser.SessionActionOutcomeInProgress} {
		t.Run(state, func(t *testing.T) {
			info := persistedActionFixture(t)
			originalOp := "prior-operation"
			persisted := *info
			persisted.LastActionOperationID = originalOp
			if state == browser.AuthorityHuman {
				persisted.AuthorityState = state
			} else {
				persisted.LastActionOutcome = state
			}
			if err := browser.SaveSession(&persisted); err != nil {
				t.Fatal(err)
			}
			called := false
			out, err := executePersistedSessionAction(info, browser.Action{Verb: "reload"}, func(string) { called = true }, func() (*browser.Snapshot, browser.Outcome, browser.ActionFidelityReport, error) {
				called = true
				return nil, browser.Outcome{}, browser.ActionFidelityReport{}, nil
			})
			if err == nil || called || out.DispatchState != "not-dispatched" {
				t.Fatalf("prior authority bypassed: %+v %v", out, err)
			}
			disk, e := browser.LoadSession(info.SessionID)
			if e != nil {
				t.Fatal(e)
			}
			if disk.LastActionOperationID != originalOp {
				t.Fatal("prior uncertain operation overwritten")
			}
		})
	}
}
func TestJourneyActionProcessExitLeavesDurableFence(t *testing.T) {
	if id := os.Getenv("DW_TEST_JOURNEY_FENCE_EXIT"); id != "" {
		info := &browser.SessionInfo{SessionID: id}
		_, _ = executePersistedSessionAction(info, browser.Action{Verb: "fill", Target: browser.Target{Kind: "locator", Locator: "css=#editor"}, Text: "private", Sensitive: true}, func(string) {}, func() (*browser.Snapshot, browser.Outcome, browser.ActionFidelityReport, error) {
			os.Exit(31)
			return nil, browser.Outcome{}, browser.ActionFidelityReport{}, nil
		})
		os.Exit(32)
	}
	info := persistedActionFixture(t)
	command := exec.Command(os.Args[0], "-test.run=^TestJourneyActionProcessExitLeavesDurableFence$")
	command.Env = append(os.Environ(), "DW_TEST_JOURNEY_FENCE_EXIT="+info.SessionID)
	err := command.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 31 {
		t.Fatalf("child did not reach fenced dispatch: %v", err)
	}
	disk, err := browser.LoadSession(info.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if disk.LastActionOutcome != browser.SessionActionOutcomeInProgress || len(disk.Refs) != 0 || disk.LastActionOperationID == "" || !disk.RedactFormValues {
		t.Fatalf("process exit left unfenced authority: %+v", disk)
	}
	dispatched := false
	out, err := executePersistedSessionAction(disk, browser.Action{Verb: "reload"}, func(string) { dispatched = true }, func() (*browser.Snapshot, browser.Outcome, browser.ActionFidelityReport, error) {
		dispatched = true
		return nil, browser.Outcome{}, browser.ActionFidelityReport{}, nil
	})
	if err == nil || dispatched || out.DispatchState != "not-dispatched" {
		t.Fatal("crashed action was automatically replayable")
	}
}
func TestJourneyActionOutcomePersistenceFailureIsUnknown(t *testing.T) {
	info := persistedActionFixture(t)
	path := filepath.Join("/tmp/dw-browser-sessions", info.SessionID+".json")
	out, err := executePersistedSessionAction(info, browser.Action{Verb: "reload"}, func(string) {}, func() (*browser.Snapshot, browser.Outcome, browser.ActionFidelityReport, error) {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Remove(path); err != nil {
				t.Error(err)
			}
		})
		return nil, browser.Outcome{Phase: "complete", DispatchState: "confirmed"}, browser.ActionFidelityReport{}, nil
	})
	if err == nil || out.DispatchState != "unknown" || info.LastActionOutcome != browser.SessionActionOutcomeUnknown || info.ObservationValid {
		t.Fatalf("failed persistence falsely confirmed: %+v %+v %v", out, info, err)
	}
}

func TestJourneyActionTerminalCommitPreservesNewerControlAndFence(t *testing.T) {
	for _, change := range []string{"Human", "operation", "runtime"} {
		t.Run(change, func(t *testing.T) {
			info := persistedActionFixture(t)
			out, err := executePersistedSessionAction(info, browser.Action{Verb: "click", Target: browser.Target{Kind: "locator", Locator: "css=#button"}}, func(string) {}, func() (*browser.Snapshot, browser.Outcome, browser.ActionFidelityReport, error) {
				lock, err := acquireSessionRuntimeInputLock(info)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Release()
				current, err := browser.LoadSession(info.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				switch change {
				case "Human":
					if err := browser.SetSessionAuthority(current, browser.AuthorityHuman); err != nil {
						t.Fatal(err)
					}
					current.RedactFormValues = true
				case "operation":
					current.LastActionOperationID = "newer-actor"
				case "runtime":
					current.BrowserRunID = "newer-runtime"
				}
				if err := browser.SaveSession(current); err != nil {
					t.Fatal(err)
				}
				return nil, browser.Outcome{Phase: "complete", DispatchState: "confirmed"}, browser.ActionFidelityReport{FocusUpdated: true, FocusedBackend: 42}, nil
			})
			current, loadErr := browser.LoadSession(info.SessionID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if change == "Human" {
				if err != nil || out.DispatchState != "confirmed" || current.AuthorityState != browser.AuthorityHuman || current.Owner != browser.SessionOwnerHuman || current.AuthorityPreviousOwner == "" || !current.RedactFormValues || current.ObservationValid || current.HumanFocusBackendNodeID != 0 {
					t.Fatalf("terminal save overrode Human control or lost delivered result: %+v %+v %v", current, out, err)
				}
			} else {
				if err == nil || out.DispatchState != "unknown" || current.LastActionOutcome != browser.SessionActionOutcomeInProgress {
					t.Fatalf("new actor fence overwritten: %+v %+v %v", current, out, err)
				}
				if change == "operation" && current.LastActionOperationID != "newer-actor" {
					t.Fatal("newer operation overwritten")
				}
				if change == "runtime" && current.BrowserRunID != "newer-runtime" {
					t.Fatal("newer runtime overwritten")
				}
			}
		})
	}
}

func TestJourneyProbeRequiresLatestPrivacyProvenance(t *testing.T) {
	info := persistedActionFixture(t)
	if err := browser.DeleteSession(info.SessionID); err != nil {
		t.Fatal(err)
	}
	// No core is attached: unavailable latest privacy must stop before its read port.
	executor := &cliActionExecutor{sessionID: info.SessionID, sessionInfo: info}
	fact, err := executor.CollectProbe(context.Background(), btest.ProbeSpec{ID: "editor", Kind: "css", Selector: "#editor"})
	if err == nil || fact.Status != "unavailable" || fact.Value != nil {
		t.Fatalf("stale in-memory privacy posture authorized runtime read: %+v %v", fact, err)
	}
}
