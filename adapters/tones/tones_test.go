package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pixelbrite.site/adapter"
	"pixelbrite.site/adapter/check"
)

// runEnv makes the test binary act as the adapter's program.
const runEnv = "TONES_TEST_RUN"

func TestMain(m *testing.M) {
	if os.Getenv(runEnv) != "" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestTonesPassesTheCheck runs this folder's adapter.json, with this test
// binary as its program, through pixelbrite-adapter check.
func TestTonesPassesTheCheck(t *testing.T) {
	m, err := adapter.ReadManifest(".")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	b, err := os.ReadFile(adapter.ManifestName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, adapter.ManifestName), b, 0o644); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(dir, filepath.FromSlash(m.Programs[adapter.Platform()]))); err != nil {
		t.Fatal(err)
	}
	t.Setenv(runEnv, "1")
	passed := map[string]bool{}
	check.Run(context.Background(), dir, check.Options{Query: "a4", Setup: map[string]string{enableStep: ""}, Audio: 200 * time.Millisecond, Stderr: os.Stderr},
		func(f check.Finding) {
			switch f.Outcome {
			case check.Fail:
				t.Errorf("%s: %s", f.Check, f.Message)
			case check.OK:
				passed[f.Check] = true
			}
		})
	for _, want := range []string{"setup", "search", "browse", "browse album", "lyrics", "continuation", "live", "live seek", "live end", "reset", "forget"} {
		if !passed[want] {
			t.Errorf("the %s check did not pass", want)
		}
	}
}
