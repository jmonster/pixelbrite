package check

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pixelbrite.site/adapter"
	"pixelbrite.site/adapter/adaptertest"
)

func runCheck(t *testing.T, path string, setup map[string]string) (int, []Finding) {
	t.Helper()
	var findings []Finding
	failures := Run(context.Background(), path, Options{Setup: setup, Audio: 50 * time.Millisecond, Stderr: os.Stderr},
		func(f Finding) { findings = append(findings, f) })
	return failures, findings
}

func outcomes(findings []Finding) map[string]string {
	got := map[string]string{}
	for _, f := range findings {
		if got[f.Check] != Fail {
			got[f.Check] = f.Outcome
		}
	}
	return got
}

func TestCheckPassesTheFixtureThroughEveryCapability(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fixture")
	adaptertest.Write(t, dir, adaptertest.Manifest("fixture"))
	failures, findings := runCheck(t, dir, map[string]string{"server_key": "s", "member_key": "k"})
	if failures != 0 {
		t.Errorf("%d failures: %+v", failures, findings)
	}
	got := outcomes(findings)
	for _, check := range []string{"manifest", "handshake", "status", "member status", "setup", "search", "browse", "browse album", "lyrics", "continuation", "live", "reset", "forget"} {
		if got[check] != OK {
			t.Errorf("%s = %q, want ok; findings %+v", check, got[check], findings)
		}
	}
}

func TestCheckSkipsCapabilitiesUntilSetupIsDone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fixture")
	adaptertest.Write(t, dir, adaptertest.Manifest("fixture"))
	failures, findings := runCheck(t, dir, nil)
	got := outcomes(findings)
	if failures != 0 || got["capabilities"] != Skip || got["search"] != "" {
		t.Errorf("without setup: %d failures, %+v", failures, findings)
	}
}

func TestCheckReportsEveryFailureInOnePass(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fixture")
	m := adaptertest.Manifest("fixture")
	m.Programs["plan9-mips"] = "bin/plan9/fixture" // another platform's program may be absent
	adaptertest.Write(t, dir, m)
	t.Setenv(adaptertest.Env, "broken")
	failures, findings := runCheck(t, dir, map[string]string{"server_key": "s", "member_key": "k"})
	var failed []string
	for _, f := range findings {
		if f.Outcome == Fail {
			failed = append(failed, f.Check+": "+f.Message)
		}
	}
	all := strings.Join(failed, "\n")
	for _, want := range []string{"audience", "has no title", "before the line above it"} {
		if !strings.Contains(all, want) {
			t.Errorf("failures do not mention %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "plan9") || failures != len(failed) {
		t.Errorf("Check counted %d failures for %d reported:\n%s", failures, len(failed), all)
	}
}

func TestCheckReportsAMissingProgramForThisPlatform(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fixture")
	m := adaptertest.Manifest("fixture")
	m.Programs[adapter.Platform()] = "bin/missing"
	adaptertest.Write(t, dir, m)
	if failures, findings := runCheck(t, dir, nil); failures != 2 { // the program, then the handshake
		t.Errorf("%d failures, %+v", failures, findings)
	}
}

func TestCheckReportsAnUnreadableManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "adapter.json"), []byte(`{"id": "Bad"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	failures, findings := runCheck(t, dir, nil)
	if failures < 4 { // id, name, version, status capability, programs
		t.Errorf("%d failures for a bare manifest: %+v", failures, findings)
	}
}

func TestMain(m *testing.M) {
	adaptertest.RunFixture()
	os.Exit(m.Run())
}
