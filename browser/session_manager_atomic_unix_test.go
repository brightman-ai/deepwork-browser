//go:build linux || darwin

package browser

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	atomicWriteHelperEnv  = "DW_BROWSER_ATOMIC_WRITE_HELPER"
	atomicWritePathEnv    = "DW_BROWSER_ATOMIC_WRITE_PATH"
	atomicWriteSessionEnv = "DW_BROWSER_ATOMIC_WRITE_SESSION"
	atomicWriteSignalEnv  = "DW_BROWSER_ATOMIC_WRITE_SIGNAL"
)

// TestSessionAtomicWriteProcessHelper is entered only by
// TestSessionFileSurvivesWriterDeathAndObservationSelfHeals. It pauses after
// half of a replacement has reached the temp file while the canonical path
// still names the prior complete generation.
func TestSessionAtomicWriteProcessHelper(t *testing.T) {
	if os.Getenv(atomicWriteHelperEnv) != "1" {
		return
	}

	replacement := SessionInfo{
		SessionID:         os.Getenv(atomicWriteSessionEnv),
		PageURL:           "https://replacement.invalid/",
		LastActionOutcome: SessionActionOutcomeConfirmed,
	}
	for i := 0; i < 128; i++ {
		replacement.Refs = append(replacement.Refs, SessionRef{
			Ref:           fmt.Sprintf("@r%d", i+1),
			BackendNodeID: int64(i + 1),
			Name:          strings.Repeat("replacement-generation-", 32),
			Visible:       true,
			Observed:      true,
		})
	}
	data, err := json.MarshalIndent(&replacement, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(80)
	}

	err = writeFileAtomicWithHook(os.Getenv(atomicWritePathEnv), data, 0o644, func(tmpPath string, written, total int) error {
		fmt.Fprintf(os.Stdout, "READY\t%s\t%d\t%d\n", tmpPath, written, total)
		if _, readErr := bufio.NewReader(os.Stdin).ReadString('\n'); readErr != nil {
			return readErr
		}
		if os.Getenv(atomicWriteSignalEnv) == "sigpipe" {
			// fd 1 has no reader. Go preserves the conventional SIGPIPE process
			// termination behavior for stdout, which models a CLI output pipe
			// disappearing while session persistence is in flight.
			_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), 1<<20))
			os.Exit(81)
		}
		select {}
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(82)
	}
	os.Exit(83)
}

func TestSessionFileSurvivesWriterDeathAndObservationSelfHeals(t *testing.T) {
	cases := []struct {
		name       string
		wantSignal syscall.Signal
	}{
		{name: "sigkill", wantSignal: syscall.SIGKILL},
		{name: "sigpipe", wantSignal: syscall.SIGPIPE},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessionID := fmt.Sprintf("atomic-writer-death-%d-%s", os.Getpid(), tc.name)
			path := sessionFilePath(sessionID)
			var tempPath string
			t.Cleanup(func() {
				_ = DeleteSession(sessionID)
				if tempPath != "" {
					_ = os.Remove(tempPath)
				}
			})

			poisoned := &SessionInfo{
				SessionID:         sessionID,
				PageURL:           "https://before.invalid/",
				SnapEpoch:         3,
				LastActionOutcome: SessionActionOutcomeUnknown,
			}
			if err := SaveSession(poisoned); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSessionAtomicWriteProcessHelper$")
			cmd.Env = append(os.Environ(),
				atomicWriteHelperEnv+"=1",
				atomicWritePathEnv+"="+path,
				atomicWriteSessionEnv+"="+sessionID,
				atomicWriteSignalEnv+"="+tc.name,
			)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}

			ready, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil {
				t.Fatalf("helper readiness: %v; stderr=%s", err, stderr.String())
			}
			parts := strings.Split(strings.TrimSpace(ready), "\t")
			if len(parts) != 4 || parts[0] != "READY" {
				t.Fatalf("malformed helper readiness %q; stderr=%s", ready, stderr.String())
			}
			tempPath = parts[1]
			written, err := strconv.Atoi(parts[2])
			if err != nil {
				t.Fatal(err)
			}
			total, err := strconv.Atoi(parts[3])
			if err != nil {
				t.Fatal(err)
			}
			if written <= 0 || written >= total {
				t.Fatalf("helper did not stop mid-write: written=%d total=%d", written, total)
			}

			switch tc.wantSignal {
			case syscall.SIGKILL:
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
			case syscall.SIGPIPE:
				if err := stdout.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(stdin, "continue\n"); err != nil {
					t.Fatal(err)
				}
				_ = stdin.Close()
			}

			waitErr := cmd.Wait()
			if ctx.Err() != nil {
				t.Fatalf("helper timed out: %v; stderr=%s", ctx.Err(), stderr.String())
			}
			exitErr, ok := waitErr.(*exec.ExitError)
			if !ok {
				t.Fatalf("helper exit=%v, want signal %s; stderr=%s", waitErr, tc.wantSignal, stderr.String())
			}
			status, ok := exitErr.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != tc.wantSignal {
				t.Fatalf("helper status=%v, want signal %s; stderr=%s", exitErr.Sys(), tc.wantSignal, stderr.String())
			}

			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("interrupted temp write changed the canonical session generation")
			}
			onDisk, err := LoadSession(sessionID)
			if err != nil {
				t.Fatalf("canonical session became unreadable: %v", err)
			}
			if onDisk.LastActionOutcome != SessionActionOutcomeUnknown || len(onDisk.Refs) != 0 {
				t.Fatalf("unexpected pre-heal session: %+v", onDisk)
			}

			onDisk.SnapEpoch++
			fresh := []SessionRef{{Ref: "@r1", BackendNodeID: 42, Visible: true, Observed: true}}
			if err := SaveObservedSession(onDisk, &Snapshot{URL: "https://after.invalid/"}, fresh); err != nil {
				t.Fatal(err)
			}
			healed, err := LoadSession(sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if healed.LastActionOutcome != SessionActionOutcomeReconciled || healed.SnapEpoch != 4 ||
				healed.PageURL != "https://after.invalid/" || len(healed.Refs) != 1 || healed.Refs[0].Ref != "@r1" {
				t.Fatalf("successful observe did not self-heal authority: %+v", healed)
			}
			t.Logf("writer=%s temp=%d/%d canonical=%dB observe_outcome=%s refs=%d",
				tc.wantSignal, written, total, len(after), healed.LastActionOutcome, len(healed.Refs))
		})
	}
}
