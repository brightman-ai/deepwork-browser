package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExcerptSpec(t *testing.T) {
	specs, err := loadExcerptSpec(`[{"name":"a","targets":["id:X"],"highlight":["p"]}]`)
	if err != nil || len(specs) != 1 || specs[0].Name != "a" {
		t.Fatalf("inline spec: %v %v", specs, err)
	}

	dir := t.TempDir()
	f := filepath.Join(dir, "spec.json")
	if err := os.WriteFile(f, []byte(`[{"name":"b","targets":["#sel"]}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadExcerptSpec("@" + f); err != nil {
		t.Fatalf("file spec: %v", err)
	}
	if _, err := loadExcerptSpec("@" + filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing file should fail")
	}

	for _, bad := range []string{
		`{}`,              // not an array
		`[]`,              // empty array
		`[{"targets":["a"]}]`,     // no name
		`[{"name":"a"}]`, // no targets
	} {
		if _, err := loadExcerptSpec(bad); err == nil {
			t.Fatalf("spec %q should fail", bad)
		}
	}
}

func TestSanitizeExcerptName(t *testing.T) {
	cases := map[string]string{
		"a/b:c":     "a_b_c",
		"图-02-标题": "图-02-标题",
		"":          "excerpt-1",
		".":         "excerpt-1",
		"..":        "excerpt-1",
	}
	for in, want := range cases {
		if got := sanitizeExcerptName(in, 0); got != want {
			t.Fatalf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
