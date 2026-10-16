package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDocumentLaunchCanceledBeforeFork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handle, err := startChromeProcess(ChromeLaunchSpec{ChromePath: "/must-not-execute", DebugPort: 0, documentRead: true, LaunchContext: ctx})
	if handle != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled launch crossed fork boundary: %v %v", handle, err)
	}
}

func TestDocumentOwnedDevToolsIdentity(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprint(mismatch), func(t *testing.T) {
			var endpoint string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				value := endpoint
				if mismatch {
					value = "ws://127.0.0.1:1/devtools/browser/other-owner"
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"webSocketDebuggerUrl": value})
			}))
			defer server.Close()
			_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
			endpoint = "ws://127.0.0.1:" + port + "/devtools/browser/owned"
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte(port+"\n/devtools/browser/owned\n"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			got, err := waitDocumentChromeReady(ctx, dir, make(chan struct{}))
			if mismatch {
				if err == nil {
					t.Fatalf("accepted another browser's endpoint: %s", got)
				}
			} else if err != nil || got != endpoint {
				t.Fatalf("owned endpoint rejected: %s %v", got, err)
			}
		})
	}
}

func TestDocumentLaunchRejectsUnownedProxy(t *testing.T) {
	for _, proxy := range []string{"https://127.0.0.1:1234", "http://localhost:1234", "http://127.0.0.1:1234/path", "http://user@127.0.0.1:1234", "http://127.0.0.1:0"} {
		if _, err := prepareDocumentLaunch(t.TempDir(), proxy, 0, 100, 100); err == nil {
			t.Errorf("accepted non-dedicated proxy %q", proxy)
		}
	}
}

func TestDocumentTargetLifecycleErrorClassification(t *testing.T) {
	for _, message := range []string{"No target with given id found", "No session with given id", "Target closed.", "Invalid InterceptionId."} {
		if !documentTargetGoneError(errors.New(message)) {
			t.Errorf("normal destruction treated as fatal: %s", message)
		}
	}
	for _, err := range []error{context.DeadlineExceeded, errors.New("Access denied"), errors.New("Method not found"), errors.New("unable to configure network guard")} {
		if documentTargetGoneError(err) {
			t.Errorf("real control error suppressed: %v", err)
		}
	}
}

func TestDocumentOwnedDevToolsPartialFile(t *testing.T) {
	var endpoint string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"webSocketDebuggerUrl": endpoint})
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	endpoint = "ws://127.0.0.1:" + port + "/devtools/browser/owned"
	dir := t.TempDir()
	path := filepath.Join(dir, "DevToolsActivePort")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type outcome struct {
		url string
		err error
	}
	result := make(chan outcome, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		url, err := waitDocumentChromeReady(ctx, dir, make(chan struct{}))
		result <- outcome{url, err}
	}()
	<-started
	for _, partial := range []string{"", port + "\n"} {
		if err := os.WriteFile(path, []byte(partial), 0600); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-result:
			t.Fatalf("reader returned for partial file %q: %+v", partial, got)
		case <-time.After(40 * time.Millisecond):
		}
	}
	if err := os.WriteFile(path, []byte(port+"\n/devtools/browser/owned\n"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.url != endpoint {
			t.Fatalf("completed file rejected: %+v", got)
		}
	case <-ctx.Done():
		t.Fatal("completed file was not observed within deadline")
	}
}

func TestDocumentOwnedDevToolsPartialFileDeadline(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte("1234\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err := waitDocumentChromeReady(ctx, dir, make(chan struct{}))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("incomplete file must wait for bounded deadline, got %v", err)
	}
}
