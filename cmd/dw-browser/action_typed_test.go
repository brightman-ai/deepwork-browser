package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTypedCLITextFilePreservesUTF8AndTrailingNewline(t *testing.T) {
	value := "with 'quoted' 中文\nlast\n"
	path := filepath.Join(t.TempDir(), "text")
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	a, rest, err := parseTypedActionArgs([]string{"fill", "@obs-r1", "--id", "x", "--text-file", path})
	if err != nil || a.Text != value || len(rest) != 2 {
		t.Fatalf("%+v %v %v", a, rest, err)
	}
	for _, args := range [][]string{{"fill", "@r1", "--text", "x", "--text-file", path}, {"fill @r1 'old'", "--id", "x"}, {"fill", "@r1", "--text", "x", "--await"}, {"type", "--focused"}} {
		if _, _, err := parseTypedActionArgs(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
}

func TestTypedCLIPointRequiresExactCaptureIdentity(t *testing.T) {
	args := []string{"click", "--x", "0", "--y", "0", "--unit", "css-px", "--frame", "viewport"}
	if _, _, err := parseTypedActionArgs(args); err == nil {
		t.Fatal("absolute screenshot point lacks capture identity")
	}
	action, _, err := parseTypedActionArgs(append(args, "--observation-id", "capture-one"))
	if err != nil {
		t.Fatal(err)
	}
	if action.Target.Point.ObservationID != "capture-one" {
		t.Fatal("capture identity not transported")
	}
	action, _, err = parseTypedActionArgs([]string{"drag", "--x", "0", "--y", "0", "--end-x", "10", "--end-y", "20", "--observation-id", "capture-two"})
	if err != nil {
		t.Fatal(err)
	}
	if action.End.ObservationID != action.Target.Point.ObservationID {
		t.Fatal("gesture endpoints do not share capture identity")
	}
}
