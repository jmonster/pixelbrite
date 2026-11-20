// Package adaptertest is a fake adapter for testing code that runs adapters.
// The test binary itself acts as the adapter's program. It has an owner step
// ("server_key") and a member step ("member_key"), and a member is ready once
// both are done. It serves every capability, and its live track "endless"
// plays until stopped. Setting Env to "broken" makes it break the spec in
// several ways at once.
package adaptertest

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"pixelbrite.site/adapter"
)

// Env makes a test binary that calls RunFixture act as the fixture adapter.
const Env = "PIXELBRITE_ADAPTER_FIXTURE"

// RunFixture runs this process as the fixture adapter when Env is set, and
// exits when the adapter stops. Call it first in TestMain.
func RunFixture() {
	if os.Getenv(Env) == "" {
		return
	}
	adapter.Main(func(dir string) adapter.Adapter {
		return &fixture{dir: dir, keys: map[string]string{}, broken: os.Getenv(Env) == "broken"}
	})
	os.Exit(0)
}

// fixture is an adapter with an owner step and a member step. A member is
// ready once both are done. Its live track "endless" plays until stopped.
// A broken fixture breaks the spec in several ways at once.
type fixture struct {
	dir     string
	broken  bool
	mu      sync.Mutex
	server  string
	keys    map[string]string
	changed adapter.Notifier
}

func (f *fixture) Status(member string) (adapter.Status, <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	serverDone, memberDone := f.server != "", f.keys[member] != ""
	memberAudience := adapter.AudienceMember
	if f.broken {
		memberAudience = ""
	}
	return adapter.Status{Name: "Fixture", Ready: serverDone && (member == "" || memberDone), Setup: []adapter.Step{
		{ID: "server_key", Kind: adapter.StepSecret, Audience: adapter.AudienceOwner, Title: "Server key", Button: "Save", Done: serverDone},
		{ID: "member_key", Kind: adapter.StepSecret, Audience: memberAudience, Title: "Your key", Button: "Save", Done: memberDone},
	}}, f.changed.Wait()
}

func (f *fixture) Setup(_ context.Context, member, step, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case step == "server_key" && member == "":
		f.server = value
	case step == "member_key" && member != "":
		f.keys[member] = value
		// Each member's state lives apart in the data folder.
		_ = os.MkdirAll(filepath.Join(f.dir, "members", member), 0o700)
	default:
		return fmt.Errorf("step %s cannot be done for %q", step, member)
	}
	f.changed.Notify()
	return nil
}

func (f *fixture) Reset(_ context.Context, member, step string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case step == "server_key" && member == "":
		f.server = ""
	case step == "member_key" && member != "":
		delete(f.keys, member)
	default:
		return fmt.Errorf("step %s cannot be undone for %q", step, member)
	}
	f.changed.Notify()
	return nil
}

func (f *fixture) Forget(_ context.Context, member string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.keys, member)
	f.changed.Notify()
	return os.RemoveAll(filepath.Join(f.dir, "members", member))
}

func (f *fixture) ready(member string) error {
	st, _ := f.Status(member)
	if !st.Ready {
		return errors.New("finish setting up first")
	}
	return nil
}

func (f *fixture) Search(_ context.Context, member, q string) ([]adapter.Item, error) {
	if err := f.ready(member); err != nil {
		return nil, err
	}
	if f.broken {
		return []adapter.Item{{Kind: adapter.KindTrack, ID: "t1", Artist: "Band", DurationMs: 1000}}, nil
	}
	return []adapter.Item{
		{Kind: adapter.KindTrack, ID: "t1", Title: q, Artist: "Band", Album: "Record", DurationMs: 1000},
		{Kind: adapter.KindAlbum, ID: "a1", Title: "Record", Artist: "Band"},
		{Kind: adapter.KindTrack, ID: "t2", Title: q + " again", Artist: "Band", DurationMs: 2000},
	}, nil
}

func (f *fixture) Browse(_ context.Context, member, id, cursor string) (adapter.Page, error) {
	switch {
	case id == "" && cursor == "":
		return adapter.Page{Items: []adapter.Item{{Kind: adapter.KindAlbum, ID: "a1", Title: "Record"}}, Next: "2"}, nil
	case id == "" && cursor == "2":
		return adapter.Page{Items: []adapter.Item{{Kind: adapter.KindPlaylist, ID: "p1", Title: "Mix"}}}, nil
	case id == "a1":
		return adapter.Page{Items: []adapter.Item{{Kind: adapter.KindTrack, ID: "t1", Title: "Song", DurationMs: 1000}}}, nil
	}
	return adapter.Page{}, adapter.ErrNotFound
}

func (f *fixture) Lyrics(_ context.Context, member, id string) (adapter.Lyrics, error) {
	if id != "t1" {
		return adapter.Lyrics{}, adapter.ErrNoLyrics
	}
	if f.broken {
		return adapter.Lyrics{Synced: true, Lines: []adapter.Line{{StartMs: 500, Words: "la la"}, {StartMs: 0, Words: "la"}}}, nil
	}
	return adapter.Lyrics{Synced: true, Lines: []adapter.Line{{StartMs: 0, Words: "la"}, {StartMs: 500, Words: "la la"}}}, nil
}

func (f *fixture) Continue(_ context.Context, member string, played []string) ([]adapter.Item, error) {
	return []adapter.Item{{Kind: adapter.KindTrack, ID: "t2", Title: "Next", DurationMs: 2000}}, nil
}

// Frames is how many frames a finite fixture track plays.
const Frames = 4800

// Play produces as fast as it can, as a realtime adapter does whatever the
// reader does: each frame's left sample is its number.
func (f *fixture) Play(ctx context.Context, member, id string, startMs int64, out adapter.PCM) error {
	if id != "t1" && id != "t2" && id != "endless" {
		return adapter.ErrNotFound
	}
	if err := f.ready(member); err != nil {
		return err
	}
	out.SetRate(48000)
	frame := make([]byte, 8)
	for i := 0; id == "endless" || i < Frames; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		binary.LittleEndian.PutUint32(frame, math.Float32bits(float32(i)))
		if _, err := out.Write(frame); err != nil {
			return err
		}
	}
	return nil
}

// Manifest describes the fixture as adapter id: every capability, with
// realtime live audio and one stream per member.
func Manifest(id string) adapter.Manifest {
	return adapter.Manifest{ID: id, Name: "Fixture", Version: "1.0",
		Capabilities: map[string]adapter.Versions{adapter.CapStatus: {Min: 1, Max: 1}, adapter.CapSearch: {Min: 1, Max: 1},
			adapter.CapBrowse: {Min: 1, Max: 1}, adapter.CapLyrics: {Min: 1, Max: 1}, adapter.CapContinuation: {Min: 1, Max: 1},
			adapter.CapLive: {Min: 1, Max: 1}},
		Live:     &adapter.Live{Pace: adapter.PaceRealtime, Streams: 1},
		Programs: map[string]string{adapter.Platform(): "bin/fixture"}}
}

// Write makes dir an adapter folder for m whose program is this test binary,
// and has the binary act as the fixture when run. The test binary's TestMain
// calls RunFixture first.
func Write(t *testing.T, dir string, m adapter.Manifest) {
	t.Helper()
	t.Setenv(Env, "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(dir, "bin", "fixture")); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, adapter.ManifestName), b, 0o644); err != nil {
		t.Fatal(err)
	}
}
