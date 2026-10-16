package browser

import (
	"errors"
	"testing"
)

func TestDocumentEquivalentRepeatedHTMLContentType(t *testing.T) {
	for _, raw := range []string{
		"text/html; charset=UTF-8\ntext/html; charset=UTF-8",
		"TEXT/HTML; CHARSET=\"UTF-8\"\r\ntext/html; charset=utf-8",
		"text/html\ntext/html; charset=UTF-8",
		"APPLICATION/XHTML+XML; charset=UTF-8\napplication/xhtml+xml",
	} {
		if !documentIsHTML(raw) {
			t.Errorf("equivalent repeated HTML media type rejected: %q", raw)
		}
	}
}

func TestDocumentContentTypeNormalization(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"text/html; charset=UTF-8\ntext/html; charset=UTF-8", "text/html; charset=utf-8"},
		{"TEXT/HTML; CHARSET=\"UTF-8\"\r\ntext/html", "text/html; charset=utf-8"},
		{"text/html; x-extra=KeepCase\ntext/html; charset=UTF-8", "text/html; charset=utf-8; x-extra=KeepCase"},
		{"text/html; x-extra=\"a,b\"\ntext/html; x-extra=\"a,b\"", "text/html; x-extra=\"a,b\""},
		{"application/rss+xml; charset=UTF-8\nAPPLICATION/RSS+XML", "application/rss+xml; charset=utf-8"},
	} {
		got, err := normalizeDocumentContentType(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("normalize(%q)=%q,%v; want %q", tc.raw, got, err, tc.want)
		}
	}
}

func TestDocumentContentTypeConflictsAreNotReroutable(t *testing.T) {
	for _, raw := range []string{
		"text/html\napplication/rss+xml",
		"text/html; charset=UTF-8\ntext/html; charset=ISO-8859-1",
		"text/html; boundary=CaseSensitive\ntext/html; boundary=casesensitive",
		"text/html; x-extra=one\ntext/html; x-extra=two",
		`text/html; charset=UTF-8\ntext/html; charset=UTF-8`,
		"text/html\n", "text/html, application/xml", "not-a-media-type",
	} {
		got, err := normalizeDocumentContentType(raw)
		var invalid *DocumentContentTypeError
		var unsupported *UnsupportedDocumentContentError
		if got != "" || !errors.As(err, &invalid) || invalid.Value != raw || errors.As(err, &unsupported) {
			t.Errorf("invalid header lost fail-closed/raw classification: %q -> %q,%v", raw, got, err)
		}
		if documentIsHTML(raw) {
			t.Errorf("conflicting/invalid header treated as HTML: %q", raw)
		}
	}
}
