//go:build linux

package browser

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain is the leak gate.
//
// A green suite that leaves 60 orphaned Chromes behind is not a green suite —
// it is a suite that cannot see the thing it broke. This gate makes the
// invariant mechanical: the browser package must end its run owning zero Chrome
// processes, and a violation fails the package rather than being discovered
// hours later by a Human running pgrep.
//
// Attribution is exact rather than heuristic, which matters on a dev box where
// the Human's own Chrome and another agent's test run may be alive at the same
// time. Two isolations do it:
//
//   - TMPDIR is repointed at a per-run directory before any test starts, so any
//     Chrome that chromedp implicitly allocates lands its --user-data-dir under
//     a path only this process could have produced. (chromedp's ExecAllocator
//     calls os.MkdirTemp("", "chromedp-runner"), which honours TMPDIR.)
//   - DW_BROWSER_PROCREG_DIR is repointed likewise, so the ownership notes
//     this run writes are this run's alone.
//
// Anything found is killed before failing: a gate that reports the leak and
// then leaves it running has only made the problem noisier.
func TestMain(m *testing.M) {
	runDir, err := os.MkdirTemp("", "dwb-leakgate-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "leak gate: cannot create run dir: %v\n", err)
		os.Exit(1)
	}
	tmpDir := filepath.Join(runDir, "tmp")
	regDir := filepath.Join(runDir, "procreg")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "leak gate: cannot create tmp dir: %v\n", err)
		os.Exit(1)
	}
	os.Setenv("TMPDIR", tmpDir)
	os.Setenv(EnvChromeProcRegistryDir, regDir)

	code := m.Run()

	leaks := findLeakedChromeProcesses(tmpDir, regDir)
	if len(leaks) > 0 {
		reportAndKillLeaks(leaks)
		if code == 0 {
			code = 1
		}
	}
	_ = os.RemoveAll(runDir)
	os.Exit(code)
}

type leakedChrome struct {
	pid        int
	pgid       int
	profileDir string
	evidence   string
	cmdline    string
}

// findLeakedChromeProcesses returns every Chrome this test binary is still
// responsible for. Both sources are proofs of ownership, not guesses:
// a --user-data-dir under this run's TMPDIR can only have been created by this
// process, and a registry note in this run's registry dir was written by it.
func findLeakedChromeProcesses(tmpDir, regDir string) []leakedChrome {
	byPID := map[int]leakedChrome{}

	for _, proc := range scanChromeProcesses() {
		if proc.profileDir == "" {
			continue
		}
		if !strings.HasPrefix(proc.profileDir, filepath.Clean(tmpDir)+string(filepath.Separator)) {
			continue
		}
		proc.evidence = "user_data_dir_under_test_tmpdir"
		byPID[proc.pid] = proc
	}

	for _, rec := range ListChromeProcessRecords() {
		if rec.OwnerPID != os.Getpid() || !isPIDAlive(rec.ChromePID) {
			continue
		}
		if existing, ok := byPID[rec.ChromePID]; ok {
			existing.evidence += "+registry_note"
			byPID[rec.ChromePID] = existing
			continue
		}
		byPID[rec.ChromePID] = leakedChrome{
			pid:        rec.ChromePID,
			pgid:       rec.PGID,
			profileDir: rec.ProfileDir,
			evidence:   "registry_note_owned_by_this_test_binary",
		}
	}

	leaks := make([]leakedChrome, 0, len(byPID))
	for _, leak := range byPID {
		leaks = append(leaks, leak)
	}
	sort.Slice(leaks, func(i, j int) bool { return leaks[i].pid < leaks[j].pid })
	return leaks
}

// scanChromeProcesses lists live Chrome *browser* processes (helpers carry
// --type= and die with the group anyway) with their profile dir and pgid.
func scanChromeProcesses() []leakedChrome {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var procs []leakedChrome
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		args := ParseProcCmdline(raw)
		if len(args) == 0 || !isChromeBrowserArgv(args) {
			continue
		}
		procs = append(procs, leakedChrome{
			pid:        pid,
			pgid:       ChromeProcessGroupID(pid),
			profileDir: filepath.Clean(ChromeProfileDirFromArgs(args)),
			cmdline:    strings.Join(args, " "),
		})
	}
	return procs
}

func reportAndKillLeaks(leaks []leakedChrome) {
	fmt.Fprintf(os.Stderr, "\n=== CHROME LEAK GATE: FAIL ===\n")
	fmt.Fprintf(os.Stderr, "%d chrome process tree(s) outlived this test binary.\n", len(leaks))
	fmt.Fprintf(os.Stderr, "Every Chrome a test starts must be killed by the test that started it —\n")
	fmt.Fprintf(os.Stderr, "Pdeathsig does not save you here (snap-confine clears it; see\n")
	fmt.Fprintf(os.Stderr, "ApplyOwnedChromeProcAttr), so an unowned Chrome is a permanent orphan.\n\n")
	for _, leak := range leaks {
		fmt.Fprintf(os.Stderr, "  pid=%d pgid=%d evidence=%s\n    profile: %s\n",
			leak.pid, leak.pgid, leak.evidence, leak.profileDir)
		if leak.cmdline != "" {
			cmdline := leak.cmdline
			if len(cmdline) > 220 {
				cmdline = cmdline[:220] + "…"
			}
			fmt.Fprintf(os.Stderr, "    argv:    %s\n", cmdline)
		}
	}
	fmt.Fprintf(os.Stderr, "\nkilling them now so the machine is left clean...\n")
	for _, leak := range leaks {
		// Tree, not group: a leaked Chrome may be sitting in *our* process
		// group (a chromedp allocator that never called Setpgid), and
		// group-killing that number would SIGKILL the test binary and the
		// shell running it. KillChromeProcessTree group-kills only when the
		// pid actually leads its own group.
		KillChromeProcessTree(leak.pid)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		alive := 0
		for _, leak := range leaks {
			if isPIDAlive(leak.pid) {
				alive++
			}
		}
		if alive == 0 {
			break
		}
		time.Sleep(ProcessExitPollInterval)
	}
	fmt.Fprintf(os.Stderr, "=== CHROME LEAK GATE: END ===\n\n")
}
