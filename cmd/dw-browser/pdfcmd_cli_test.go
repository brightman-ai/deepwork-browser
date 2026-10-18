package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePdfPages(t *testing.T) {
	for _, c := range []struct {
		raw   string
		total int
		want  []int
	}{
		{"1", 5, []int{1}},
		{"1-3", 5, []int{1, 2, 3}},
		{"2,4-5", 5, []int{2, 4, 5}},
		{"3-9", 5, []int{3, 4, 5}}, // clamp
		{"1,1-2", 5, []int{1, 2}},  // dedupe
	} {
		got, err := parsePdfPages(c.raw, c.total)
		if err != nil {
			t.Fatalf("parse %q: %v", c.raw, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("parse %q = %v, want %v", c.raw, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("parse %q = %v, want %v", c.raw, got, c.want)
			}
		}
	}
	for _, bad := range []string{"0", "3-1", "a", "1-x"} {
		if _, err := parsePdfPages(bad, 5); err == nil {
			t.Fatalf("parse %q should fail", bad)
		}
	}
}

func TestLoadPdfExcerptSpec(t *testing.T) {
	specs, err := loadPdfExcerptSpec(`[{"name":"a","page":2,"phrases":["x"]}]`)
	if err != nil || len(specs) != 1 || specs[0].Page != 2 {
		t.Fatalf("inline: %v %v", specs, err)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "s.json")
	os.WriteFile(f, []byte(`[{"name":"b","page":1,"boxes":[[10,10,200,80]]}]`), 0o644)
	if _, err := loadPdfExcerptSpec("@" + f); err != nil {
		t.Fatalf("file: %v", err)
	}
	for _, bad := range []string{
		`{}`,
		`[]`,
		`[{"page":1,"phrases":["x"]}]`, // no name
		`[{"name":"a","page":0,"phrases":["x"]}]`, // page<1
		`[{"name":"a","page":1}]`,                 // no phrases/boxes
	} {
		if _, err := loadPdfExcerptSpec(bad); err == nil {
			t.Fatalf("spec %q should fail", bad)
		}
	}
}

func TestLoadPdfSpecArrayErrorHasNoGoTypeName(t *testing.T) {
	_, err := loadPdfExcerptSpec(`{"name":"a","page":1}`)
	if err == nil || strings.Contains(err.Error(), "PdfExcerptSpec") {
		t.Fatalf("non-array error should not leak Go type names: %v", err)
	}
}
