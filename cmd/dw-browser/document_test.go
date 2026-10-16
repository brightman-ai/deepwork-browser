package main

import (
	"testing"
	"time"
)

func TestDocumentCLIContract(t *testing.T) {
	for _, args := range [][]string{
		{"https://example.com/", "--timeout=2s", "--max-html-bytes", "1234", "--max-network-bytes=5678"},
		{"--timeout", "2s", "--max-html-bytes=1234", "--max-network-bytes", "5678", "--", "https://example.com/"},
	} {
		r, err := parseDocumentRequest(args)
		if err != nil || r.URL != "https://example.com/" || r.Timeout != 2*time.Second || r.MaxHTMLBytes != 1234 || r.MaxNetworkBytes != 5678 {
			t.Fatalf("request %+v err %v", r, err)
		}
	}
	for _, args := range [][]string{
		nil, {"https://example.com/", "second"},
		{"http://127.0.0.1/"}, {"file:///etc/passwd"},
		{"https://example.com/", "--id", "existing"},
		{"https://example.com/", "--allow-host", "example.com"},
		{"https://example.com/", "--mode", "visible"},
		{"https://example.com/", "--timeout", "0s"},
		{"https://example.com/", "--max-html-bytes", "-1"},
		{"https://example.com/", "--max-network-bytes", "0"},
		{"https://example.com/", "--max-html-bytes", "999999999999999999999"},
	} {
		if _, err := parseDocumentRequest(args); err == nil {
			t.Fatalf("unsafe/invalid document arguments accepted: %v", args)
		}
	}
}
