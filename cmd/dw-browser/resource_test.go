package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brightman-ai/deepwork-browser/browser"
)

func TestFetchDeclaredContractRejectsUnsupportedAndDuplicateInput(t *testing.T) {
	base := []string{"https://example.com/image", "--save", "body.bin"}
	for _, suffix := range [][]string{
		{"--save", "other"}, {"--timeout", "2s", "--timeout=3s"},
		{"--max-bytes", "1", "--max-bytes=2"}, {"--max-network-bytes=1", "--max-network-bytes", "2"},
		{"--network-route=direct", "--network-route", "system"},
		{"--mode", "headless"}, {"--id", "task"}, {"--header", "Accept: image/png"},
		{"--cookie", "a=b"}, {"--method", "POST"}, {"--allow-host", "example.com"},
		{"--network-route", "auto"}, {"--timeout", "0s"}, {"--max-bytes", "0"},
	} {
		args := append(append([]string{}, base...), suffix...)
		if _, _, err := parseResourceRequest(args); err == nil {
			t.Fatalf("accepted invalid fetch request: %q", args)
		}
	}
	for _, args := range [][]string{{"https://example.com/"}, {"https://example.com/", "--save="}} {
		if _, _, err := parseResourceRequest(args); err == nil {
			t.Fatalf("accepted missing body destination: %q", args)
		}
	}
	request, path, err := parseResourceRequest(append(base, "--network-route=direct", "--timeout=2s", "--max-bytes=123", "--max-network-bytes=456"))
	if err != nil || request.NetworkRoute != browser.NetworkRouteDirect || request.MaxBytes != 123 || request.MaxNetworkBytes != 456 || path != "body.bin" {
		t.Fatalf("explicit settings lost: %+v %s %v", request, path, err)
	}
}

func assertNoFetchTemps(t *testing.T, dir string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, ".dw-fetch-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("body temp leak: %v %v", files, err)
	}
}

func TestFetchRejectsBadSaveBeforeAcquisition(t *testing.T) {
	dir := t.TempDir()
	called := 0
	acquire := func(context.Context, browser.ResourceRequest) (*browser.Resource, error) {
		called++
		return nil, errors.New("unexpected network")
	}
	for _, path := range []string{dir, filepath.Join(dir, "missing", "body")} {
		if _, err := runFetchWith(context.Background(), []string{"https://example.com/", "--save", path}, acquire); err == nil {
			t.Fatalf("bad path accepted: %s", path)
		}
	}
	if called != 0 {
		t.Fatalf("bad path attempted network %d times", called)
	}
	assertNoFetchTemps(t, dir)
}

func TestFetchOutputCollisionRejectsPathAliases(t *testing.T) {
	dir := t.TempDir()
	body := filepath.Join(dir, "body")
	if err := os.WriteFile(body, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "link")
	hardlink := filepath.Join(dir, "hard")
	aliasDir := filepath.Join(dir, "directory-link")
	if err := os.Symlink(body, symlink); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(body, hardlink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, aliasDir); err != nil {
		t.Fatal(err)
	}
	for _, metadata := range []string{body, filepath.Join(dir, ".", "body"), symlink, hardlink, filepath.Join(aliasDir, "body")} {
		if err := validateFetchOutputs([]string{"https://example.com/", "--save", body}, metadata); err == nil {
			t.Fatalf("accepted collision: %s", metadata)
		}
	}
	absent := filepath.Join(dir, "new")
	if err := validateFetchOutputs([]string{"https://example.com/", "--save", absent}, filepath.Join(aliasDir, "new")); err == nil {
		t.Fatal("missing files with aliased parents accepted")
	}
	if err := validateFetchOutputs([]string{"https://example.com/", "--save", body}, filepath.Join(dir, "meta")); err != nil {
		t.Fatal(err)
	}
	assertNoFetchTemps(t, dir)
}

func settledTestResource(body []byte) *browser.Resource {
	hash := sha256.Sum256(body)
	return &browser.Resource{Schema: browser.ResourceSchema, RequestedURL: "https://example.com/", URL: "https://example.com/", StatusCode: 200, ContentType: "application/octet-stream", Representation: "http-body", Bytes: int64(len(body)), SHA256: hex.EncodeToString(hash[:]), Body: body, Cleanup: browser.ResourceCleanup{NetworkClosed: true}}
}

func TestFetchPublishesExactBinaryAndSeparatesMetadata(t *testing.T) {
	dir := t.TempDir()
	bodyPath, metadata := filepath.Join(dir, "body"), filepath.Join(dir, "meta.json")
	binary := []byte{0, 255, 10, 0, 128}
	resource := settledTestResource(binary)
	result, err := runFetchWith(context.Background(), []string{"https://example.com/", "--save", bodyPath}, func(context.Context, browser.ResourceRequest) (*browser.Resource, error) { return resource, nil })
	if err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(bodyPath)
	if err != nil || string(saved) != string(binary) {
		t.Fatalf("body changed: %x %v", saved, err)
	}
	// Exercise the real JSON delivery boundary separately from kernel acquisition.
	stdout := os.Stdout
	sink, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = sink
	defer func() { os.Stdout = stdout; sink.Close() }()
	if code := emitCLIResult(result, nil, metadata); code != 0 {
		t.Fatalf("metadata delivery exit %d", code)
	}
	os.Stdout = stdout
	encoded, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["body_file"] != bodyPath || fields["bytes"] != float64(len(binary)) || fields["sha256"] != resource.SHA256 || fields["schema"] != browser.ResourceSchema {
		t.Fatalf("metadata lost body identity: %s", encoded)
	}
	if _, present := fields["body"]; present {
		t.Fatalf("binary leaked into metadata: %s", encoded)
	}
	saved, err = os.ReadFile(bodyPath)
	if err != nil || string(saved) != string(binary) {
		t.Fatal("metadata delivery overwrote body")
	}
	assertNoFetchTemps(t, dir)
}

func TestFetchFailurePreservesExistingBodyAndReportsOnlyDiagnostic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "body")
	if err := os.WriteFile(path, []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	failure := &browser.ResourceAcquisitionError{Diagnostic: browser.ResourceFailureDiagnostic{Schema: browser.ResourceFailureSchema, RequestedURL: "https://example.com/", Stage: "read", Reason: "body_budget_exceeded", BodyBytesRead: 123, Cleanup: browser.ResourceCleanup{NetworkClosed: true}}, Cause: errors.New("body budget exceeded")}
	result, err := runFetchWith(context.Background(), []string{"https://example.com/", "--save", path}, func(context.Context, browser.ResourceRequest) (*browser.Resource, error) { return nil, failure })
	if err == nil {
		t.Fatal("acquisition failure became success")
	}
	encoded, marshalErr := json.Marshal(result.Payload)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if !strings.Contains(string(encoded), `"body_bytes_read":123`) || !strings.Contains(string(encoded), `"usable":false`) || strings.Contains(string(encoded), "body_file") || strings.Contains(string(encoded), `"body":`) {
		t.Fatalf("failure advertises body: %s", encoded)
	}
	existing, readErr := os.ReadFile(path)
	if readErr != nil || string(existing) != "previous" {
		t.Fatalf("old body changed: %q %v", existing, readErr)
	}
	assertNoFetchTemps(t, dir)
}

func TestFetchPersistFailureKeepsAcquisitionFacts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "body")
	resource := settledTestResource([]byte("complete"))
	resource.Policy.NetworkBytes = 99
	result, err := runFetchWith(context.Background(), []string{"https://example.com/", "--save", path}, func(context.Context, browser.ResourceRequest) (*browser.Resource, error) {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		return resource, nil
	})
	if err == nil {
		t.Fatal("rename to directory should fail")
	}
	diagnostic, ok := result.Payload.(browser.ResourceFailureDiagnostic)
	if !ok || diagnostic.Stage != "persist" || diagnostic.Usable || diagnostic.BodyBytesRead != resource.Bytes || diagnostic.Policy.NetworkBytes != 99 || !diagnostic.Cleanup.NetworkClosed {
		t.Fatalf("persist lost settled facts: %+v", result.Payload)
	}
	encoded, _ := json.Marshal(result.Payload)
	if strings.Contains(string(encoded), "body_file") {
		t.Fatalf("failed persist advertises file: %s", encoded)
	}
	assertNoFetchTemps(t, dir)
}

func TestFetchReplacesSaveSymlinkWithoutFollowingIt(t *testing.T) {
	dir := t.TempDir()
	target, path := filepath.Join(dir, "unrelated"), filepath.Join(dir, "body")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	_, err := runFetchWith(context.Background(), []string{"https://example.com/", "--save", path}, func(context.Context, browser.ResourceRequest) (*browser.Resource, error) {
		return settledTestResource([]byte("new")), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	existing, err := os.ReadFile(target)
	if err != nil || string(existing) != "keep" {
		t.Fatal("save followed unrelated symlink")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatal("save did not atomically replace symlink")
	}
	assertNoFetchTemps(t, dir)
}

func TestFetchHelpExplainsRepresentationAndBudgets(t *testing.T) {
	data, err := json.Marshal(generatedHelp("fetch"))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"--save", "--output", "--max-bytes", "8388608", "33554432", "decoded", "network-accounting", "--network-route", "MIME/signature", "body_file"} {
		if !strings.Contains(string(data), text) {
			t.Fatalf("fetch help omits %s: %s", text, data)
		}
	}
	if !strings.Contains(generatedProtocol(), "fetch URL --save FILE") {
		t.Fatal("protocol omits fetch choice")
	}
}

func TestFetchCLIBoundaryRejectsCollisionBeforeWritingMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "body")
	if err := os.WriteFile(path, []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	sink, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = sink
	defer func() { os.Stdout = stdout; sink.Close() }()
	code := runCLI([]string{"fetch", "https://example.com/", "--save", path, "--output", path, "--timeout", "1ms"})
	os.Stdout = stdout
	if code != exitRunErr {
		t.Fatalf("collision exit=%d", code)
	}
	encoded, err := os.ReadFile(sink.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"phase": "validation"`) || !strings.Contains(string(encoded), "must not alias") {
		t.Fatalf("collision bypassed declaration output validation: %s", encoded)
	}
	previous, err := os.ReadFile(path)
	if err != nil || string(previous) != "previous" {
		t.Fatalf("metadata overwrote body destination: %q %v", previous, err)
	}
	assertNoFetchTemps(t, dir)
}

func TestFetchOutputCollisionUsesFilesystemNameEquivalence(t *testing.T) {
	for _, names := range [][2]string{{"asset.bin", "ASSET.BIN"}, {"caf\u00e9.bin", "cafe\u0301.bin"}} {
		dir := t.TempDir()
		a, b := filepath.Join(dir, names[0]), filepath.Join(dir, names[1])
		// Discover local volume semantics instead of assuming APFS or Linux rules.
		if err := os.WriteFile(a, []byte("probe"), 0600); err != nil {
			t.Fatal(err)
		}
		one, err := os.Stat(a)
		if err != nil {
			t.Fatal(err)
		}
		other, statErr := os.Stat(b)
		equivalent := statErr == nil && os.SameFile(one, other)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal(statErr)
		}
		if err := os.Remove(a); err != nil {
			t.Fatal(err)
		}
		err = validateFetchOutputs([]string{"https://example.com/", "--save", a}, b)
		if equivalent && err == nil {
			t.Fatalf("filesystem-equivalent names accepted: %q", names)
		}
		if !equivalent && err != nil {
			t.Fatalf("distinct filesystem names rejected: %q: %v", names, err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("alias preflight published output or leaked probe: %v %v", entries, err)
		}
	}
}

func TestFetchCLIBoundaryRejectsMetadataDirectoryBeforePublishingBody(t *testing.T) {
	dir := t.TempDir()
	body := filepath.Join(dir, "body")
	stdout := os.Stdout
	sink, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = sink
	defer func() { os.Stdout = stdout; sink.Close() }()
	code := runCLI([]string{"fetch", "https://example.com/", "--save", body, "--output", dir, "--timeout", "1ms"})
	os.Stdout = stdout
	encoded, err := os.ReadFile(sink.Name())
	if err != nil {
		t.Fatal(err)
	}
	if code != exitRunErr || !strings.Contains(string(encoded), "--output destination is a directory") {
		t.Fatalf("metadata directory bypassed preflight: code=%d %s", code, encoded)
	}
	if _, err := os.Stat(body); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bad metadata path published body: %v", err)
	}
	assertNoFetchTemps(t, dir)
}

func TestFetchLateDeliveryFailurePreservesCountsWithoutAdvertisingBody(t *testing.T) {
	for _, failure := range []string{"metadata-rename", "json-encoding"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			bodyPath := filepath.Join(dir, "body")
			metadata := filepath.Join(dir, "meta.json")
			args := []string{"https://example.com/", "--save", bodyPath}
			if err := validateFetchOutputs(args, metadata); err != nil {
				t.Fatal(err)
			}
			resource := settledTestResource([]byte("committed bytes"))
			resource.Policy.NetworkBytes = 71
			result, err := runFetchWith(context.Background(), args, func(context.Context, browser.ResourceRequest) (*browser.Resource, error) { return resource, nil })
			if err != nil {
				t.Fatal(err)
			}
			if failure == "metadata-rename" {
				// A real post-preflight filesystem change makes JSON publication fail.
				if err := os.Mkdir(metadata, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				result.Payload = make(chan int)
			}
			stdout := os.Stdout
			sink, err := os.Create(filepath.Join(dir, "stdout"))
			if err != nil {
				t.Fatal(err)
			}
			os.Stdout = sink
			defer func() { os.Stdout = stdout; sink.Close() }()
			code := emitCLIResult(result, nil, metadata)
			os.Stdout = stdout
			encoded, err := os.ReadFile(sink.Name())
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Phase      string                            `json:"phase"`
				Diagnostic browser.ResourceFailureDiagnostic `json:"partial_result"`
			}
			if err := json.Unmarshal(encoded, &envelope); err != nil {
				t.Fatal(err)
			}
			d := envelope.Diagnostic
			if code != exitRunErr || envelope.Phase != "delivery" || d.Schema != browser.ResourceFailureSchema || d.Stage != "delivery" || d.Reason != "delivery_failed" || d.Usable || d.BodyBytesRead != resource.Bytes || d.Policy.NetworkBytes != 71 || !d.Cleanup.NetworkClosed {
				t.Fatalf("late failure lost accounting: code=%d %s", code, encoded)
			}
			if strings.Contains(string(encoded), "body_file") || strings.Contains(string(encoded), `"body":`) {
				t.Fatalf("failed delivery advertises body: %s", encoded)
			}
			saved, err := os.ReadFile(bodyPath)
			if err != nil || string(saved) != "committed bytes" {
				t.Fatalf("body commit falsely rolled back: %q %v", saved, err)
			}
			assertNoFetchTemps(t, dir)
		})
	}
}

func TestFetchStdoutFailurePreservesCommittedMetadataAndBody(t *testing.T) {
	dir := t.TempDir()
	bodyPath := filepath.Join(dir, "body")
	metadata := filepath.Join(dir, "meta.json")
	resource := settledTestResource([]byte("complete body"))
	resource.Policy.NetworkBytes = 93
	result, err := runFetchWith(context.Background(), []string{"https://example.com/", "--save", bodyPath}, func(context.Context, browser.ResourceRequest) (*browser.Resource, error) { return resource, nil })
	if err != nil {
		t.Fatal(err)
	}
	closed, err := os.Create(filepath.Join(dir, "closed-stdout"))
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	previousOut, previousErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = closed, stderr
	defer func() { os.Stdout, os.Stderr = previousOut, previousErr; stderr.Close() }()
	code := emitCLIResult(result, nil, metadata)
	os.Stdout, os.Stderr = previousOut, previousErr
	if code != exitRunErr {
		t.Fatalf("broken stdout exit %d", code)
	}
	encoded, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	var committed struct {
		Schema   string                 `json:"schema"`
		BodyFile string                 `json:"body_file"`
		Bytes    int64                  `json:"bytes"`
		Policy   browser.ResourcePolicy `json:"policy"`
	}
	if err := json.Unmarshal(encoded, &committed); err != nil {
		t.Fatal(err)
	}
	if committed.Schema != browser.ResourceSchema || committed.BodyFile != bodyPath || committed.Bytes != resource.Bytes || committed.Policy.NetworkBytes != 93 {
		t.Fatalf("stdout failure altered committed metadata: %s", encoded)
	}
	body, err := os.ReadFile(bodyPath)
	if err != nil || string(body) != "complete body" {
		t.Fatalf("stdout failure altered committed body: %q %v", body, err)
	}
	diagnostic, err := os.ReadFile(stderr.Name())
	if err != nil || !strings.Contains(string(diagnostic), "result delivery failed") || strings.Contains(string(diagnostic), "complete body") {
		t.Fatalf("stdout failure stderr invalid: %q %v", diagnostic, err)
	}
	assertNoFetchTemps(t, dir)
}
