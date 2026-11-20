package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"pixelbrite.site/adapter"
)

type note struct {
	id   string
	name string
	hz   float64
}

// notes are the tracks, in album order. A track's ID is its note's id, which
// never changes.
var notes = []note{
	{"c4", "C4", 261.63},
	{"e4", "E4", 329.63},
	{"g4", "G4", 392.00},
	{"a4", "A4", 440.00},
	{"c5", "C5", 523.25},
}

const (
	albumID    = "tuning-notes"
	albumTitle = "Tuning notes"
	artist     = "Test tones"
	enableStep = "enable"
	sampleRate = 48000
	trackMs    = 20000
	// pageSize keeps the album to several browse pages, to show the cursor.
	pageSize = 2
)

// tones remembers which members turned tones on, in members/HANDLE/on in the
// data folder, so it survives restarts. Forget deletes the member's folder.
type tones struct {
	dir     string
	mu      sync.Mutex
	on      map[string]bool
	changed adapter.Notifier
}

func newTones(dir string) *tones {
	t := &tones{dir: dir, on: map[string]bool{}}
	members, _ := os.ReadDir(filepath.Join(dir, "members"))
	for _, m := range members {
		if _, err := os.Stat(t.flag(m.Name())); err == nil {
			t.on[m.Name()] = true
		}
	}
	return t
}

func (t *tones) flag(member string) string { return filepath.Join(t.dir, "members", member, "on") }

// Status shows the one member step. Without a member it's the server's view,
// which is ready: the adapter serves any member who finished the step.
func (t *tones) Status(member string) (adapter.Status, <-chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	done := t.on[member]
	return adapter.Status{
		Name:  "Tones",
		Ready: member == "" || done,
		Setup: []adapter.Step{{
			ID: enableStep, Kind: adapter.StepAction, Audience: adapter.AudienceMember,
			Title: "Turn on test tones for your account", Button: "Turn on", Done: done,
		}},
	}, t.changed.Wait()
}

func (t *tones) Setup(_ context.Context, member, step, _ string) error {
	if step != enableStep || member == "" {
		return fmt.Errorf("there is no step %q for this account", step)
	}
	if err := os.MkdirAll(filepath.Dir(t.flag(member)), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(t.flag(member), nil, 0o600); err != nil {
		return err
	}
	t.set(member, true)
	return nil
}

func (t *tones) Reset(_ context.Context, member, step string) error {
	if step != enableStep || member == "" {
		return fmt.Errorf("there is no step %q for this account", step)
	}
	if err := os.Remove(t.flag(member)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	t.set(member, false)
	return nil
}

func (t *tones) Forget(_ context.Context, member string) error {
	t.set(member, false)
	return os.RemoveAll(filepath.Join(t.dir, "members", member))
}

// set records a member's state and raises the revision, which tells the
// server to ask for status again.
func (t *tones) set(member string, on bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if on {
		t.on[member] = true
	} else {
		delete(t.on, member)
	}
	t.changed.Notify()
}

func (t *tones) ready(member string) error {
	if st, _ := t.Status(member); !st.Ready {
		return errors.New("turn on test tones in this adapter's setup first")
	}
	return nil
}

func track(n note) adapter.Item {
	return adapter.Item{Kind: adapter.KindTrack, ID: n.id, Title: fmt.Sprintf("%s (%g Hz)", n.name, n.hz),
		Artist: artist, Album: albumTitle, DurationMs: trackMs}
}

func album() adapter.Item {
	return adapter.Item{Kind: adapter.KindAlbum, ID: albumID, Title: albumTitle, Artist: artist}
}

func find(id string) (note, bool) {
	for _, n := range notes {
		if n.id == id {
			return n, true
		}
	}
	return note{}, false
}

// Search returns tracks whose title contains the query, plus the album if
// its title does.
func (t *tones) Search(_ context.Context, member, query string) ([]adapter.Item, error) {
	if err := t.ready(member); err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	items := []adapter.Item{}
	if q == "" {
		return items, nil
	}
	for _, n := range notes {
		if it := track(n); strings.Contains(strings.ToLower(it.Title), q) {
			items = append(items, it)
		}
	}
	if strings.Contains(strings.ToLower(albumTitle), q) {
		items = append(items, album())
	}
	return items, nil
}

// Browse shows the album at the top level and its tracks inside it, pageSize
// at a time. The cursor is the index of the page's first track.
func (t *tones) Browse(_ context.Context, member, id, cursor string) (adapter.Page, error) {
	if err := t.ready(member); err != nil {
		return adapter.Page{}, err
	}
	switch id {
	case "":
		return adapter.Page{Items: []adapter.Item{album()}}, nil
	case albumID:
		from, err := strconv.Atoi(cursor)
		if cursor == "" {
			from, err = 0, nil
		}
		if err != nil || from < 0 || from > len(notes) {
			return adapter.Page{}, fmt.Errorf("the cursor %q is not one this adapter gave", cursor)
		}
		to := min(from+pageSize, len(notes))
		page := adapter.Page{}
		for _, n := range notes[from:to] {
			page.Items = append(page.Items, track(n))
		}
		if to < len(notes) {
			page.Next = strconv.Itoa(to)
		}
		return page, nil
	}
	return adapter.Page{}, adapter.ErrNotFound
}

// Lyrics are synced: each line starts at its start_ms.
func (t *tones) Lyrics(_ context.Context, member, id string) (adapter.Lyrics, error) {
	if err := t.ready(member); err != nil {
		return adapter.Lyrics{}, err
	}
	n, ok := find(id)
	if !ok {
		return adapter.Lyrics{}, adapter.ErrNotFound
	}
	return adapter.Lyrics{Synced: true, Lines: []adapter.Line{
		{StartMs: 0, Words: n.name},
		{StartMs: trackMs / 2, Words: fmt.Sprintf("%g hertz", n.hz)},
	}}, nil
}

// Continue picks the note after the last one played, wrapping around.
func (t *tones) Continue(_ context.Context, member string, played []string) ([]adapter.Item, error) {
	if err := t.ready(member); err != nil {
		return nil, err
	}
	next := 0
	if len(played) > 0 {
		for i, n := range notes {
			if n.id == played[len(played)-1] {
				next = (i + 1) % len(notes)
			}
		}
	}
	return []adapter.Item{track(notes[next])}, nil
}

// Play writes the note's sine wave from startMs to the end. With pace
// "reader" in adapter.json, it writes as fast as the server reads. It stops
// when ctx ends, which is how the server stops a track early.
func (t *tones) Play(ctx context.Context, member, id string, startMs int64, out adapter.PCM) error {
	n, ok := find(id)
	if !ok {
		return adapter.ErrNotFound
	}
	if err := t.ready(member); err != nil {
		return err
	}
	out.SetRate(sampleRate)
	const chunk = sampleRate / 10 // frames per write: a tenth of a second
	buf := make([]byte, chunk*8)  // two float32 samples per frame
	end := int64(trackMs) * sampleRate / 1000
	for frame := max(startMs, 0) * sampleRate / 1000; frame < end; {
		if err := ctx.Err(); err != nil {
			return err
		}
		frames := min(chunk, end-frame)
		for i := range frames {
			v := float32(0.2 * math.Sin(2*math.Pi*n.hz*float64(frame+i)/sampleRate))
			binary.LittleEndian.PutUint32(buf[i*8:], math.Float32bits(v))
			binary.LittleEndian.PutUint32(buf[i*8+4:], math.Float32bits(v))
		}
		if _, err := out.Write(buf[:frames*8]); err != nil {
			return err
		}
		frame += frames
	}
	return nil
}
