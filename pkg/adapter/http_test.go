package adapter

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAdapter serves every capability. Its "key" step is a member step: each
// member's readiness is its own.
type fakeAdapter struct {
	mu      sync.Mutex
	keys    map[string]string
	changed Notifier
	failAt  int // Play fails after this many frames when positive
	played  []string
}

func newFake() *fakeAdapter { return &fakeAdapter{keys: map[string]string{}} }

func (f *fakeAdapter) Status(member string) (Status, <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	done := f.keys[member] != ""
	return Status{Name: "Fake", Ready: done, Setup: []Step{{ID: "key", Kind: StepSecret, Audience: AudienceMember, Title: "Key", Done: done}}}, f.changed.Wait()
}

func (f *fakeAdapter) Setup(_ context.Context, member, step, value string) error {
	if step != "key" {
		return errors.New("unknown step")
	}
	if member == "" {
		return errors.New("a member sets the key")
	}
	f.mu.Lock()
	f.keys[member] = value
	f.changed.Notify()
	f.mu.Unlock()
	return nil
}

func (f *fakeAdapter) Reset(_ context.Context, member, step string) error {
	if step != "key" {
		return errors.New("unknown step")
	}
	return f.Forget(context.Background(), member)
}

func (f *fakeAdapter) Forget(_ context.Context, member string) error {
	f.mu.Lock()
	delete(f.keys, member)
	f.changed.Notify()
	f.mu.Unlock()
	return nil
}

func (f *fakeAdapter) ready(member string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keys[member] == "" {
		return errors.New("set the key first")
	}
	return nil
}

func (f *fakeAdapter) Search(_ context.Context, member, q string) ([]Item, error) {
	if err := f.ready(member); err != nil {
		return nil, err
	}
	return []Item{{Kind: KindTrack, ID: "1", Title: q, DurationMs: 1000}}, nil
}

func (f *fakeAdapter) Browse(_ context.Context, member, id, cursor string) (Page, error) {
	switch {
	case id == "" && cursor == "":
		return Page{Items: []Item{{Kind: KindAlbum, ID: "a", Title: "Album"}}, Next: "2"}, nil
	case id == "" && cursor == "2":
		return Page{Items: []Item{{Kind: KindPlaylist, ID: "p", Title: "Mix"}}}, nil
	case id == "a":
		return Page{Items: []Item{{Kind: KindTrack, ID: "1", Title: "Song"}}}, nil
	}
	return Page{}, ErrNotFound
}

func (f *fakeAdapter) Lyrics(_ context.Context, member, id string) (Lyrics, error) {
	if id != "1" {
		return Lyrics{}, ErrNoLyrics
	}
	return Lyrics{Synced: true, Lines: []Line{{StartMs: 500, Words: "la"}}}, nil
}

func (f *fakeAdapter) Continue(_ context.Context, member string, played []string) ([]Item, error) {
	f.played = played
	return []Item{{Kind: KindTrack, ID: "2", Title: "Next"}}, nil
}

// Play sends four frames whose left sample counts up from startMs.
func (f *fakeAdapter) Play(_ context.Context, member, id string, startMs int64, out PCM) error {
	if id == "missing" {
		return ErrNotFound
	}
	if err := f.ready(member); err != nil {
		return err
	}
	out.SetRate(48000)
	frame := make([]byte, 8)
	for i := 0; i < 4; i++ {
		if f.failAt > 0 && i == f.failAt {
			return errors.New("connection lost")
		}
		binary.LittleEndian.PutUint32(frame, math.Float32bits(float32(startMs)+float32(i)))
		binary.LittleEndian.PutUint32(frame[4:], math.Float32bits(float32(-i)))
		if _, err := out.Write(frame); err != nil {
			return err
		}
	}
	return nil
}

type capture struct {
	bytes.Buffer
	rate float64
}

func (c *capture) SetRate(hz float64) { c.rate = hz }

func serve(t *testing.T, a Adapter, chosen map[string]int) *Remote {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = Serve(ctx, l, a, chosen) }()
	return Dial("tcp", l.Addr().String())
}

func allCapabilities() map[string]int {
	chosen := map[string]int{}
	for name, v := range Supported() {
		chosen[name] = v.Max
	}
	return chosen
}

func TestRemoteCarriesTheWholeContractForEachMember(t *testing.T) {
	src := newFake()
	remote := serve(t, src, allCapabilities())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	followed := make(chan error, 1)
	go func() { followed <- remote.Follow(ctx) }()

	// The server's own view follows every member's change.
	for st, next := remote.Status(); st.Name != "Fake"; st, next = remote.Status() {
		select {
		case <-next:
		case <-ctx.Done():
			t.Fatal("status never arrived")
		}
	}
	_, changed := remote.Status()
	if st, err := remote.MemberStatus(ctx, "ana"); err != nil || st.Ready || st.Setup[0].Audience != AudienceMember {
		t.Fatalf("ana before setup = %+v, %v", st, err)
	}
	if err := remote.Setup(ctx, "ana", "key", "secret"); err != nil {
		t.Fatal(err)
	}
	// Setup answers before the long poll sees the change; ana sees it at once.
	if st, err := remote.MemberStatus(ctx, "ana"); err != nil || !st.Ready || !st.Setup[0].Done {
		t.Errorf("ana right after setup = %+v, %v", st, err)
	}
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("ana's setup raised no revision")
	}
	if st, err := remote.MemberStatus(ctx, "ben"); err != nil || st.Ready {
		t.Errorf("ben shares ana's key: %+v, %v", st, err)
	}
	if err := remote.Setup(ctx, "ana", "nope", ""); err == nil || err.Error() != "unknown step" {
		t.Errorf("Setup(nope) = %v", err)
	}

	items, err := remote.Search(ctx, "ana", "song")
	if err != nil || len(items) != 1 || items[0].Title != "song" || items[0].Kind != KindTrack {
		t.Errorf("Search = %+v, %v", items, err)
	}
	if _, err := remote.Search(ctx, "ben", "song"); err == nil || err.Error() != "set the key first" {
		t.Errorf("ben's search = %v", err)
	}
	page, err := remote.Browse(ctx, "ana", "", "")
	if err != nil || page.Next != "2" || page.Items[0].Kind != KindAlbum {
		t.Errorf("Browse top = %+v, %v", page, err)
	}
	if page, err = remote.Browse(ctx, "ana", "", "2"); err != nil || page.Next != "" || page.Items[0].ID != "p" {
		t.Errorf("Browse page 2 = %+v, %v", page, err)
	}
	if _, err := remote.Browse(ctx, "ana", "gone", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("Browse(gone) = %v, want ErrNotFound", err)
	}
	if l, err := remote.Lyrics(ctx, "ana", "1"); err != nil || !l.Synced || l.Lines[0].Words != "la" {
		t.Errorf("Lyrics(1) = %+v, %v", l, err)
	}
	if _, err := remote.Lyrics(ctx, "ana", "2"); !errors.Is(err, ErrNoLyrics) {
		t.Errorf("Lyrics(2) = %v, want ErrNoLyrics", err)
	}
	if next, err := remote.Continue(ctx, "ana", []string{"1", "3"}); err != nil || next[0].ID != "2" || !reflect.DeepEqual(src.played, []string{"1", "3"}) {
		t.Errorf("Continue = %+v, %v after %v", next, err, src.played)
	}

	var out capture
	if err := remote.Play(ctx, "ana", "1", 0, &out); err != nil || out.rate != 48000 || out.Len() != 32 {
		t.Errorf("Play = rate %v, %d bytes, %v", out.rate, out.Len(), err)
	}
	out.Reset()
	if err := remote.Play(ctx, "ana", "1", 7000, &out); err != nil || math.Float32frombits(binary.LittleEndian.Uint32(out.Bytes())) != 7000 {
		t.Errorf("Play from 7 s = %v, first sample %v", err, out.Bytes()[:4])
	}
	if err := remote.Play(ctx, "ana", "missing", 0, &capture{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Play(missing) = %v", err)
	}
	if err := remote.Play(ctx, "ben", "1", 0, &capture{}); err == nil || err.Error() != "set the key first" {
		t.Errorf("ben's Play = %v", err)
	}
	src.failAt = 2
	out.Reset()
	if err := remote.Play(ctx, "ana", "1", 0, &out); err == nil || err.Error() != "connection lost" || out.Len() != 16 {
		t.Errorf("Play after a mid-song failure = %d bytes, %v", out.Len(), err)
	}
	if err := remote.Reset(ctx, "ana", "key"); err != nil {
		t.Fatal(err)
	}
	if st, err := remote.MemberStatus(ctx, "ana"); err != nil || st.Ready || st.Setup[0].Done {
		t.Errorf("ana after reset = %+v, %v", st, err)
	}
	if err := remote.Setup(ctx, "ana", "key", "again"); err != nil {
		t.Fatal(err)
	}
	if err := remote.Forget(ctx, "ana"); err != nil {
		t.Fatal(err)
	}
	if st, err := remote.MemberStatus(ctx, "ana"); err != nil || st.Ready {
		t.Errorf("ana after forget = %+v, %v", st, err)
	}
	if err := remote.Forget(ctx, ""); err == nil {
		t.Error("Forget without a member succeeded")
	}
	cancel()
	<-followed
}

func TestServeAnswersOnlyTheChosenCapabilities(t *testing.T) {
	remote := serve(t, newFake(), map[string]int{CapStatus: 1, CapSearch: 1})
	ctx := context.Background()
	if _, err := remote.Lyrics(ctx, "", "1"); !errors.Is(err, ErrNoLyrics) {
		t.Errorf("Lyrics without the capability = %v, want a 404", err)
	}
	var status *StatusError
	if err := remote.Play(ctx, "", "1", 0, &capture{}); !errors.As(err, &status) || status.Code != 404 {
		t.Errorf("Play without the capability = %v, want a 404", err)
	}
}

// TestPlayReadsRecordsInAnyFraming plays bodies an adapter in any language
// might send: with a length, with a record type the server does not know,
// without its end record, and ending in a failure.
func TestPlayReadsRecordsInAnyFraming(t *testing.T) {
	record := func(kind byte, payload string) string {
		var head [5]byte
		head[0] = kind
		binary.LittleEndian.PutUint32(head[1:], uint32(len(payload)))
		return string(head[:]) + payload
	}
	frame := string(make([]byte, 8))
	for _, tc := range []struct {
		name, body string
		withLength bool
		wantErr    string
		wantBytes  int
	}{
		{"a length and an unknown record", record('a', frame) + record('x', "later") + record('a', frame) + record('e', "{}"), true, "", 16},
		{"no end record", record('a', frame), false, "stopped before its end record", 8},
		{"a failure at the end", record('a', frame) + record('e', `{"message":"lost the stream"}`), false, "lost the stream", 8},
	} {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(rateHeader, "48000")
			if tc.withLength {
				w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)))
			}
			_, _ = w.Write([]byte(tc.body))
		})}
		go func() { _ = srv.Serve(l) }()
		out := &capture{}
		err = Dial("tcp", l.Addr().String()).Play(context.Background(), "", "1", 0, out)
		_ = srv.Close()
		switch {
		case tc.wantErr == "" && err != nil, tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: Play = %v, want %q", tc.name, err, tc.wantErr)
		case out.Len() != tc.wantBytes:
			t.Errorf("%s: %d bytes of audio, want %d", tc.name, out.Len(), tc.wantBytes)
		}
	}
}
