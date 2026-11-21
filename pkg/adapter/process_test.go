package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// adapterEnv makes the test binary act as an adapter speaking the status
// versions it names. With helperEnv set, it starts a helper that holds its
// stderr and that its Close leaves running, and Close records that it ran.
const (
	adapterEnv = "ADAPTER_TEST_PROGRAM"
	helperEnv  = "ADAPTER_TEST_HELPER"
)

// helped is a fake adapter with a helper process.
type helped struct {
	*fakeAdapter
	dir string
}

func (h helped) Close() error { return os.WriteFile(filepath.Join(h.dir, "closed"), nil, 0o600) }

func newHelped(dir string) Adapter {
	helper := exec.Command("sleep", "300")
	helper.Stderr = os.Stderr
	if err := helper.Start(); err != nil {
		panic(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "helper"), []byte(strconv.Itoa(helper.Process.Pid)), 0o600)
	return helped{newFake(), dir}
}

func TestMain(m *testing.M) {
	if speaks := os.Getenv(adapterEnv); speaks != "" {
		ours := Supported()
		v, err := ParseVersions(speaks)
		newAdapter := func(string) Adapter { return newFake() }
		if os.Getenv(helperEnv) != "" {
			newAdapter = newHelped
		}
		if err == nil {
			ours[CapStatus] = v
			err = run(newAdapter, ours, os.Args[1:], os.Stdin, os.Stdout)
		}
		var incompatible *IncompatibleError
		if err != nil && !errors.As(err, &incompatible) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestVersionsNegotiateTheNewestSharedVersion(t *testing.T) {
	for _, tc := range []struct {
		a, b Versions
		want int
		ok   bool
	}{
		{Versions{1, 1}, Versions{1, 1}, 1, true},
		{Versions{1, 3}, Versions{2, 5}, 3, true},
		{Versions{2, 5}, Versions{1, 3}, 3, true},
		{Versions{4, 4}, Versions{1, 9}, 4, true},
		{Versions{1, 2}, Versions{3, 4}, 2, false},
	} {
		if got, ok := tc.a.Negotiate(tc.b); ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("%s with %s = %d, %v; want %d, %v", tc.a, tc.b, got, ok, tc.want, tc.ok)
		}
	}
	for in, want := range map[string]Versions{"1-3": {1, 3}, "2": {2, 2}} {
		if got, err := ParseVersions(in); err != nil || got != want {
			t.Errorf("ParseVersions(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "3-1", "0-1", "a-b", "1-"} {
		if _, err := ParseVersions(in); err == nil {
			t.Errorf("ParseVersions(%q) accepted", in)
		}
	}
	chosen, skipped, err := Negotiate(map[string]Versions{CapStatus: {1, 2}, CapSearch: {1, 1}, "photos": {1, 1}, CapLive: {3, 4}},
		map[string]Versions{CapStatus: {1, 1}, CapSearch: {1, 2}, CapLive: {1, 2}})
	if err != nil || !reflect.DeepEqual(chosen, map[string]int{CapStatus: 1, CapSearch: 1}) || !reflect.DeepEqual(skipped, []string{CapLive, "photos"}) {
		t.Errorf("Negotiate = %v, %v, %v", chosen, skipped, err)
	}
	if _, _, err := Negotiate(map[string]Versions{CapStatus: {2, 2}}, Supported()); err == nil {
		t.Error("an adapter sharing no status version negotiated")
	}
	if got, err := ParseCapabilities(FormatCapabilities(map[string]int{CapStatus: 1, CapLive: 2})); err != nil || got[CapLive] != 2 || got[CapStatus] != 1 {
		t.Errorf("capabilities round trip = %v, %v", got, err)
	}
}

func TestManifestValidateReportsEveryProblem(t *testing.T) {
	var m Manifest
	if err := m.Validate(); err == nil {
		t.Fatal("empty manifest accepted")
	}
	m = Manifest{ID: "Bad ID", Capabilities: map[string]Versions{CapLive: {1, 1}},
		Live: &Live{Pace: "fast", Streams: 1}, Programs: map[string]string{"linux-amd64": "../escape", "darwin-arm64": "/abs"}}
	err := m.Validate()
	for _, want := range []string{"id", "name", "version", `lack "status"`, "pace", "linux-amd64", "darwin-arm64"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate = %v, want it to mention %s", err, want)
		}
	}
	good := Manifest{ID: "music-box", Name: "Music Box", Version: "1.0", Capabilities: map[string]Versions{CapStatus: {1, 1}},
		Programs: map[string]string{Platform(): "bin/music-box"}}
	if err := good.Validate(); err != nil {
		t.Errorf("good manifest: %v", err)
	}
	if _, err := good.Program(t.TempDir()); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("Program without the file = %v", err)
	}
	good.Programs = map[string]string{"plan9-mips": "x"}
	if _, err := good.Program(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no program for "+Platform()) {
		t.Errorf("Program for another platform = %v", err)
	}
}

// testPackage is an adapter folder whose program is this test binary,
// speaking status versions speaks.
func testPackage(t *testing.T, speaks string, declared map[string]Versions) (string, Manifest) {
	t.Helper()
	t.Setenv(adapterEnv, speaks)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(exe, filepath.Join(dir, "fake")); err != nil {
		t.Fatal(err)
	}
	return dir, Manifest{ID: "fake", Name: "Fake", Version: "1", Capabilities: declared, Programs: map[string]string{Platform(): "fake"}}
}

func TestStartServesTheNegotiatedCapabilities(t *testing.T) {
	dir, m := testPackage(t, "1", map[string]Versions{CapStatus: {1, 3}, CapSearch: {1, 1}, CapBrowse: {5, 6}})
	data := filepath.Join(t.TempDir(), "data")
	p, err := Start(context.Background(), dir, m, data, nil, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if !reflect.DeepEqual(p.Capabilities, map[string]int{CapStatus: 1, CapSearch: 1}) || !reflect.DeepEqual(p.Skipped, []string{CapBrowse}) {
		t.Errorf("capabilities %v, skipped %v", p.Capabilities, p.Skipped)
	}
	if _, err := os.Stat(data); err != nil {
		t.Errorf("data folder not made: %v", err)
	}
	ctx := context.Background()
	if err := p.Setup(ctx, "ana", "key", "k"); err != nil {
		t.Fatal(err)
	}
	if items, err := p.Search(ctx, "ana", "song"); err != nil || len(items) != 1 {
		t.Errorf("Search = %v, %v", items, err)
	}
	if _, err := p.Browse(ctx, "ana", "", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("Browse of a skipped capability = %v, want a 404", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	default:
		t.Error("Close returned before the adapter exited")
	}
}

func TestStartRunsAnAdapterNamedByARelativePath(t *testing.T) {
	dir, m := testPackage(t, "1", map[string]Versions{CapStatus: {1, 1}})
	t.Chdir(filepath.Dir(dir))
	p, err := Start(context.Background(), filepath.Base(dir), m, filepath.Join(t.TempDir(), "data"), nil, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	_ = p.Close()
}

func TestCloseLetsTheAdapterCleanUpThenStopsWhatItStarted(t *testing.T) {
	dir, m := testPackage(t, "1", map[string]Versions{CapStatus: {1, 1}})
	t.Setenv(helperEnv, "1")
	data := t.TempDir()
	p, err := Start(context.Background(), dir, m, data, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(data, "helper"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(b))
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	select {
	case <-closed:
	case <-time.After(StopGrace + 5*time.Second): // a hang guard: Close never returning
		t.Fatal("Close did not return while the helper held the adapter's stderr")
	}
	if _, err := os.Stat(filepath.Join(data, "closed")); err != nil {
		t.Error("the adapter was stopped before its Close ran")
	}
	// The helper was in the adapter's process group; killing a dead
	// process's group fails.
	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Error("the adapter's helper outlived it")
	}
}

func TestStartReportsAnAdapterThatCannotServeTheChosenVersions(t *testing.T) {
	// The manifest claims status 2, which the program does not speak.
	dir, m := testPackage(t, "1", map[string]Versions{CapStatus: {2, 2}})
	_, err := start(context.Background(), dir, m, t.TempDir(), nil, os.Stderr, map[string]Versions{CapStatus: {1, 2}})
	var incompatible *IncompatibleError
	if !errors.As(err, &incompatible) || !strings.Contains(incompatible.Reason, "status 2") {
		t.Errorf("Start = %v, want the program to refuse status 2", err)
	}
}

func TestRequirementsReachTheAdapterOrRefuseIt(t *testing.T) {
	m := Manifest{Requires: []string{RequireChrome}}
	env, err := requirementEnv(m, Provided{RequireChrome: "/opt/chrome"})
	if err != nil || len(env) != 1 || env[0] != "PIXELBRITE_CHROME=/opt/chrome" {
		t.Errorf("installed: %v %v", env, err)
	}
	if env, err := requirementEnv(m, Provided{RequireChrome: ""}); err != nil || len(env) != 0 {
		t.Errorf("not yet installed: %v %v", env, err)
	}
	var incompatible *IncompatibleError
	if _, err := requirementEnv(Manifest{Requires: []string{"teleporter"}}, Provided{RequireChrome: ""}); !errors.As(err, &incompatible) ||
		!strings.Contains(incompatible.Reason, "teleporter") {
		t.Errorf("unknown requirement: %v", err)
	}
	bad := Manifest{ID: "x", Name: "X", Version: "1", Capabilities: map[string]Versions{CapStatus: {1, 1}},
		Programs: map[string]string{"darwin-arm64": "x"}, Requires: []string{"chrome", "chrome", "Not Valid"}}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "twice") || !strings.Contains(err.Error(), "Not Valid") {
		t.Errorf("Validate = %v", err)
	}
}
