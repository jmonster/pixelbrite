// Package adapter implements the Pixel Brite adapter spec in README.md.
// An adapter adds a source, such as a streaming service, to a Pixel Brite
// server, which runs it as a separate program and asks it for search
// results, lists, lyrics, what to play next and audio.
//
// To write an adapter, implement Adapter and the interface for each
// capability you serve, then call Main. The server runs adapters with Start.
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Capabilities an adapter may serve. Each carries its own version.
const (
	// CapStatus is the adapter's status and setup steps. Every adapter serves it.
	CapStatus = "status"
	// CapSearch answers a query with typed items.
	CapSearch = "search"
	// CapBrowse lists the items in a folder, artist, album or playlist.
	CapBrowse = "browse"
	// CapLyrics supplies a track's lyrics.
	CapLyrics = "lyrics"
	// CapContinuation picks what to play after the items played.
	CapContinuation = "continuation"
	// CapLive plays a track as live PCM audio.
	CapLive = "live"
)

// Item kinds. An adapter may answer with tracks only.
const (
	KindArtist   = "artist"
	KindAlbum    = "album"
	KindTrack    = "track"
	KindPlaylist = "playlist"
	KindFolder   = "folder"
	KindVideo    = "video"
	KindFile     = "file"
)

// Item is one entry in a list. ID is unique within its adapter and stable:
// the server passes it back to browse, lyrics and play, possibly long after
// the list was read. It is any non-empty text except "." and "..".
type Item struct {
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist,omitempty"`
	Album      string `json:"album,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	// Artwork is an absolute URL the server fetches and serves.
	Artwork string `json:"artwork,omitempty"`
}

// Page is a browse answer. Next is passed back to read the following page;
// it is empty on the last one.
type Page struct {
	Items []Item `json:"items"`
	Next  string `json:"next,omitempty"`
}

// Line is one lyric line; StartMs is zero throughout untimed lyrics.
type Line struct {
	StartMs int64  `json:"start_ms"`
	Words   string `json:"words"`
}

// Lyrics are a track's lines; Synced is false for untimed text.
type Lyrics struct {
	Synced bool   `json:"synced"`
	Lines  []Line `json:"lines"`
}

// Setup step kinds. Clients render each kind with their own controls.
const (
	// StepWebSignIn opens URL in a private browser session. When the person
	// finishes, the client sends every cookie the session holds as the
	// step's value, which ParseCookies reads.
	StepWebSignIn = "web_sign_in"
	// StepSecret asks for a typed value; URL explains where to get it.
	StepSecret = "secret"
	// StepAction is a button. Busy says it is running; Detail says what to do meanwhile.
	StepAction = "action"
)

// Who completes a setup step.
const (
	// AudienceOwner steps are completed once for the server, by an owner.
	AudienceOwner = "owner"
	// AudienceMember steps are completed by each member for their own account.
	AudienceMember = "member"
)

// Step is one thing a person does once to make an adapter ready.
type Step struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Audience string `json:"audience"`
	Title    string `json:"title"`
	Button   string `json:"button"` // the label of the step's control
	Detail   string `json:"detail,omitempty"`
	URL      string `json:"url,omitempty"`
	Done     bool   `json:"done"`
	Busy     bool   `json:"busy,omitempty"`
	// Error says why the step's last attempt failed, until it is tried again.
	Error string `json:"error,omitempty"`
}

// Status is what an adapter shows to one member, or to the server when no
// member is named: its name, whether it serves requests now, and the steps
// that make it ready, in order. Unavailable is one sentence saying why the
// adapter cannot run on this computer; it then lists no steps.
type Status struct {
	Name        string `json:"name"`
	Ready       bool   `json:"ready"`
	Unavailable string `json:"unavailable,omitempty"`
	Setup       []Step `json:"setup,omitempty"`
}

// Cookie is one cookie from a web sign-in, as the browser held it. Domain
// starts with a dot when the cookie also applies to subdomains. Expires is
// zero for a cookie that lasts only for the session.
type Cookie struct {
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Domain   string    `json:"domain"`
	Path     string    `json:"path"`
	Expires  time.Time `json:"expires,omitzero"`
	Secure   bool      `json:"secure"`
	HTTPOnly bool      `json:"http_only"`
}

// ParseCookies reads a web sign-in step's value: every cookie the person's
// browser session held when they finished.
func ParseCookies(value string) ([]Cookie, error) {
	var cookies []Cookie
	if err := json.Unmarshal([]byte(value), &cookies); err != nil {
		return nil, fmt.Errorf("the sign-in sent no cookie jar: %w", err)
	}
	return cookies, nil
}

// PCM receives live audio: SetRate once, before the first Write, then
// interleaved float32 little-endian stereo frames.
type PCM interface {
	io.Writer
	SetRate(hz float64)
}

var (
	// ErrNoLyrics means the adapter has no lyrics for a track.
	ErrNoLyrics = errors.New("no lyrics")
	// ErrNotFound means the adapter has no item with that ID.
	ErrNotFound = errors.New("not found")
)

// Adapter is the part every adapter serves. A member is the random handle
// the server issued for one profile, or empty for the server itself; an
// adapter with member steps keeps each member's state apart by it.
type Adapter interface {
	// Status returns what member sees and a channel closed at the next
	// change to any member's status.
	Status(member string) (Status, <-chan struct{})
	// Setup completes step for member with value.
	Setup(ctx context.Context, member, step, value string) error
	// Reset undoes step for member, so it can be completed again; for a
	// sign-in that signs out.
	Reset(ctx context.Context, member, step string) error
	// Forget deletes everything the adapter keeps for member, whose profile
	// is gone. Member is never empty.
	Forget(ctx context.Context, member string) error
}

// Searcher serves CapSearch: items matching query, best match first.
type Searcher interface {
	Search(ctx context.Context, member, query string) ([]Item, error)
}

// Browser serves CapBrowse: the items in the item id, or the adapter's top
// level when id is empty, from the page cursor names.
type Browser interface {
	Browse(ctx context.Context, member, id, cursor string) (Page, error)
}

// LyricsSource serves CapLyrics. It returns ErrNoLyrics when it has none.
type LyricsSource interface {
	Lyrics(ctx context.Context, member, id string) (Lyrics, error)
}

// Continuer serves CapContinuation: what to play after played, most recent last.
type Continuer interface {
	Continue(ctx context.Context, member string, played []string) ([]Item, error)
}

// LivePlayer serves CapLive. Play writes id's audio to out from startMs
// (zero unless the manifest declares seek) and returns when it ends.
type LivePlayer interface {
	Play(ctx context.Context, member, id string, startMs int64, out PCM) error
}

// Notifier hands out channels closed on the next change. The zero value is ready.
type Notifier struct {
	ch chan struct{}
}

// Wait returns a channel closed by the next Notify. Callers serialize Wait and
// Notify with the lock that guards the state they announce.
func (n *Notifier) Wait() <-chan struct{} {
	if n.ch == nil {
		n.ch = make(chan struct{})
	}
	return n.ch
}

// Notify wakes every waiter.
func (n *Notifier) Notify() {
	if n.ch != nil {
		close(n.ch)
		n.ch = nil
	}
}
