package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// The wire contract served on the adapter's socket; README.md specifies it.
const (
	// MemberHeader carries the member handle on a request made for a member.
	MemberHeader = "Member"
	rateHeader   = "Sample-Rate"
	// Live audio records: a type byte, the payload's length as a uint32
	// little-endian, then the payload.
	recordAudio = 'a'
	recordEnd   = 'e'
	// maxEndRecord bounds an end record's JSON.
	maxEndRecord = 64 << 10
	pollWait     = 25 * time.Second
	// pollMargin is how much longer than pollWait a long poll may take
	// before the adapter counts as not answering.
	pollMargin = 10 * time.Second
	// StatusTimeout bounds a status request that does not wait for a change.
	StatusTimeout = 3 * time.Second
)

// ErrNotAnswering is an adapter that did not answer a status request in time.
var ErrNotAnswering = errors.New("the adapter is not answering")

// ValidItemID reports whether id can name an item: any non-empty text
// except "." and "..", which do not survive as a path segment.
func ValidItemID(id string) bool { return id != "" && id != "." && id != ".." }

type revised struct {
	Revision uint64 `json:"revision"`
	Status   Status `json:"status"`
}

type itemList struct {
	Items []Item `json:"items"`
}

type failure struct {
	Message string `json:"message"`
}

// Serve answers the requests of the chosen capabilities for a on l until
// ctx ends, and returns once every request it was answering has returned.
func Serve(ctx context.Context, l net.Listener, a Adapter, chosen map[string]int) error {
	h := &handler{a: a}
	go h.track(ctx)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", h.status)
	mux.HandleFunc("POST /setup/{step}", h.setup)
	mux.HandleFunc("DELETE /setup/{step}", func(w http.ResponseWriter, r *http.Request) {
		writeDone(w, a.Reset(r.Context(), member(r), r.PathValue("step")))
	})
	mux.HandleFunc("DELETE /member", func(w http.ResponseWriter, r *http.Request) {
		if member(r) == "" {
			writeJSON(w, http.StatusBadRequest, failure{"forgetting a member needs the Member header"})
			return
		}
		writeDone(w, a.Forget(r.Context(), member(r)))
	})
	if s, ok := a.(Searcher); ok && chosen[CapSearch] > 0 {
		mux.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
			items, err := s.Search(r.Context(), member(r), r.URL.Query().Get("q"))
			writeItems(w, items, err)
		})
	}
	if b, ok := a.(Browser); ok && chosen[CapBrowse] > 0 {
		mux.HandleFunc("GET /browse", func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			page, err := b.Browse(r.Context(), member(r), q.Get("id"), q.Get("cursor"))
			if page.Items == nil {
				page.Items = []Item{}
			}
			writeAnswer(w, page, err)
		})
	}
	if l, ok := a.(LyricsSource); ok && chosen[CapLyrics] > 0 {
		mux.HandleFunc("GET /lyrics/{id}", func(w http.ResponseWriter, r *http.Request) {
			lyrics, err := l.Lyrics(r.Context(), member(r), r.PathValue("id"))
			writeAnswer(w, lyrics, err)
		})
	}
	if c, ok := a.(Continuer); ok && chosen[CapContinuation] > 0 {
		mux.HandleFunc("GET /continuation", func(w http.ResponseWriter, r *http.Request) {
			items, err := c.Continue(r.Context(), member(r), r.URL.Query()["played"])
			writeItems(w, items, err)
		})
	}
	if p, ok := a.(LivePlayer); ok && chosen[CapLive] > 0 {
		mux.HandleFunc("GET /play/{id}", func(w http.ResponseWriter, r *http.Request) { play(w, r, p) })
	}
	var running sync.WaitGroup
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		running.Add(1)
		defer running.Done()
		mux.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: counted, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	err := srv.Serve(l)
	running.Wait() // ctx has ended, so each request is stopping what it started
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func member(r *http.Request) string { return r.Header.Get(MemberHeader) }

type handler struct {
	a        Adapter
	mu       sync.Mutex
	revision uint64
	changed  Notifier
}

// track numbers the adapter's status changes so a poll never misses one: it
// waits on the next change before raising the revision for the last one.
func (h *handler) track(ctx context.Context) {
	_, next := h.a.Status("")
	for {
		select {
		case <-ctx.Done():
			return
		case <-next:
		}
		_, next = h.a.Status("")
		h.mu.Lock()
		h.revision++
		h.changed.Notify()
		h.mu.Unlock()
	}
}

func (h *handler) status(w http.ResponseWriter, r *http.Request) {
	after, waiting := uint64(0), r.URL.Query().Has("after")
	if waiting {
		after, _ = strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	}
	timeout := time.NewTimer(pollWait)
	defer timeout.Stop()
	for {
		h.mu.Lock()
		revision, wait := h.revision, h.changed.Wait()
		h.mu.Unlock()
		if !waiting || revision > after {
			st, _ := h.a.Status(member(r))
			writeJSON(w, http.StatusOK, revised{Revision: revision, Status: st})
			return
		}
		select {
		case <-wait:
		case <-timeout.C:
			waiting = false
		case <-r.Context().Done():
			return
		}
	}
}

func (h *handler) setup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, failure{err.Error()})
		return
	}
	writeDone(w, h.a.Setup(r.Context(), member(r), r.PathValue("step"), body.Value))
}

// writeDone answers a setup request: 204, or the failure that says why.
func writeDone(w http.ResponseWriter, err error) {
	if err != nil {
		writeJSON(w, http.StatusBadRequest, failure{err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeItems(w http.ResponseWriter, items []Item, err error) {
	if items == nil {
		items = []Item{}
	}
	writeAnswer(w, itemList{Items: items}, err)
}

func writeAnswer(w http.ResponseWriter, v any, err error) {
	switch {
	case errors.Is(err, ErrNoLyrics), errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, failure{err.Error()})
	case err != nil:
		writeJSON(w, http.StatusBadGateway, failure{err.Error()})
	default:
		writeJSON(w, http.StatusOK, v)
	}
}

func play(w http.ResponseWriter, r *http.Request, p LivePlayer) {
	startMs, _ := strconv.ParseInt(r.URL.Query().Get("start_ms"), 10, 64)
	out := &responsePCM{w: w}
	out.flusher, _ = w.(http.Flusher)
	err := p.Play(r.Context(), member(r), r.PathValue("id"), max(startMs, 0), out)
	switch {
	case !out.started && err != nil:
		writeAnswer(w, nil, err)
	case !out.started:
		writeJSON(w, http.StatusBadGateway, failure{"the track played no audio"})
	case r.Context().Err() != nil: // the server stopped the track
	case err != nil:
		end, _ := json.Marshal(failure{err.Error()})
		out.record(recordEnd, end)
	default:
		out.record(recordEnd, []byte("{}"))
	}
}

// responsePCM sends the rate as a header, then each write as an audio record.
type responsePCM struct {
	w       http.ResponseWriter
	flusher http.Flusher
	started bool
}

func (p *responsePCM) SetRate(hz float64) {
	h := p.w.Header()
	h.Set(rateHeader, strconv.FormatFloat(hz, 'g', -1, 64))
	h.Set("Content-Type", "application/octet-stream")
	p.w.WriteHeader(http.StatusOK)
	p.started = true
}

func (p *responsePCM) Write(b []byte) (int, error) {
	if !p.started {
		return 0, errors.New("adapter: Write before SetRate")
	}
	if err := p.record(recordAudio, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (p *responsePCM) record(kind byte, payload []byte) error {
	var head [5]byte
	head[0] = kind
	binary.LittleEndian.PutUint32(head[1:], uint32(len(payload)))
	if _, err := p.w.Write(head[:]); err != nil {
		return err
	}
	if _, err := p.w.Write(payload); err != nil {
		return err
	}
	if p.flusher != nil {
		p.flusher.Flush()
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Remote is an adapter reached over a byte stream. Follow keeps its status
// current.
type Remote struct {
	client   *http.Client
	mu       sync.Mutex
	revision uint64
	status   Status
	members  map[string]revised
	changed  Notifier
}

// Dial returns the Remote that reaches an adapter serving HTTP at address.
func Dial(network, address string) *Remote {
	var d net.Dialer
	return &Remote{members: map[string]revised{}, client: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, address)
		},
		DisableCompression: true,
	}}}
}

// Follow mirrors the adapter's own status until ctx ends or the adapter
// stops answering, which it returns. A long poll that outlasts the adapter's
// 25 seconds by pollMargin counts as not answering.
func (r *Remote) Follow(ctx context.Context) error {
	var v revised
	if err := r.timedGet(ctx, StatusTimeout, "", "/status", nil, &v); err != nil {
		return err
	}
	for first := true; ; first = false {
		r.mu.Lock()
		if first || v.Revision != r.revision {
			r.revision, r.status = v.Revision, v.Status
			clear(r.members)
			r.changed.Notify()
		}
		r.mu.Unlock()
		if err := r.timedGet(ctx, pollWait+pollMargin, "", "/status", url.Values{"after": {strconv.FormatUint(v.Revision, 10)}}, &v); err != nil {
			return err
		}
	}
}

// timedGet is get bounded by limit; running out is ErrNotAnswering.
func (r *Remote) timedGet(ctx context.Context, limit time.Duration, member, path string, q url.Values, v any) error {
	bounded, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	err := r.get(bounded, member, path, q, v)
	if err != nil && ctx.Err() == nil && bounded.Err() != nil {
		return fmt.Errorf("%w: %s took longer than %s", ErrNotAnswering, path, limit)
	}
	return err
}

// Status returns the adapter's own status and a channel closed when any
// member's status changes.
func (r *Remote) Status() (Status, <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status, r.changed.Wait()
}

// MemberStatus returns what member sees. It asks the adapter once per
// revision and member, and waits at most StatusTimeout for the answer.
func (r *Remote) MemberStatus(ctx context.Context, member string) (Status, error) {
	if member == "" {
		st, _ := r.Status()
		return st, nil
	}
	r.mu.Lock()
	cached, ok := r.members[member]
	revision := r.revision
	r.mu.Unlock()
	if ok && cached.Revision >= revision {
		return cached.Status, nil
	}
	var v revised
	if err := r.timedGet(ctx, StatusTimeout, member, "/status", nil, &v); err != nil {
		return Status{}, err
	}
	r.mu.Lock()
	if v.Revision >= r.revision {
		r.members[member] = v
	}
	r.mu.Unlock()
	return v.Status, nil
}

// Setup completes step for member. What member sees is asked again after it.
func (r *Remote) Setup(ctx context.Context, member, step, value string) error {
	body, _ := json.Marshal(map[string]string{"value": value})
	return r.change(ctx, http.MethodPost, member, "/setup/"+url.PathEscape(step), body)
}

// Reset undoes step for member.
func (r *Remote) Reset(ctx context.Context, member, step string) error {
	return r.change(ctx, http.MethodDelete, member, "/setup/"+url.PathEscape(step), nil)
}

// Forget has the adapter delete everything it keeps for member.
func (r *Remote) Forget(ctx context.Context, member string) error {
	if member == "" {
		return errors.New("forgetting a member needs its handle")
	}
	return r.change(ctx, http.MethodDelete, member, "/member", nil)
}

// change sends a request that changes member's status and drops what was
// cached for member, so the next MemberStatus shows the change.
func (r *Remote) change(ctx context.Context, method, member, path string, body []byte) error {
	req, _ := http.NewRequestWithContext(ctx, method, "http://adapter"+path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	setMember(req, member)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := responseError(resp); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.members, member)
	r.mu.Unlock()
	return nil
}

func (r *Remote) Search(ctx context.Context, member, query string) ([]Item, error) {
	var list itemList
	return list.Items, r.get(ctx, member, "/search", url.Values{"q": {query}}, &list)
}

func (r *Remote) Browse(ctx context.Context, member, id, cursor string) (Page, error) {
	q := url.Values{}
	if id != "" {
		q.Set("id", id)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var page Page
	return page, r.get(ctx, member, "/browse", q, &page)
}

func (r *Remote) Lyrics(ctx context.Context, member, id string) (Lyrics, error) {
	if !ValidItemID(id) {
		return Lyrics{}, ErrNoLyrics
	}
	var l Lyrics
	err := r.get(ctx, member, "/lyrics/"+url.PathEscape(id), nil, &l)
	if errors.Is(err, ErrNotFound) {
		return Lyrics{}, ErrNoLyrics
	}
	return l, err
}

func (r *Remote) Continue(ctx context.Context, member string, played []string) ([]Item, error) {
	var list itemList
	return list.Items, r.get(ctx, member, "/continuation", url.Values{"played": played}, &list)
}

// Play copies id's live audio into out from startMs until it ends.
func (r *Remote) Play(ctx context.Context, member, id string, startMs int64, out PCM) error {
	if !ValidItemID(id) {
		return ErrNotFound
	}
	u := "http://adapter/play/" + url.PathEscape(id)
	if startMs > 0 {
		u += "?start_ms=" + strconv.FormatInt(startMs, 10)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	setMember(req, member)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := responseError(resp); err != nil {
		return err
	}
	rate, err := strconv.ParseFloat(resp.Header.Get(rateHeader), 64)
	if err != nil || rate <= 0 {
		return errors.New("the adapter sent no sample rate")
	}
	out.SetRate(rate)
	return readRecords(ctx, bufio.NewReaderSize(resp.Body, 64<<10), out)
}

// readRecords copies audio records into out until the end record, which
// says whether the track played through. Records of other types are skipped.
func readRecords(ctx context.Context, r *bufio.Reader, out PCM) error {
	var head [5]byte
	for {
		if _, err := io.ReadFull(r, head[:]); err != nil {
			return truncated(ctx, err)
		}
		n := int64(binary.LittleEndian.Uint32(head[1:]))
		switch head[0] {
		case recordAudio:
			if _, err := io.CopyN(out, r, n); err != nil {
				return truncated(ctx, err)
			}
		case recordEnd:
			if n > maxEndRecord {
				return fmt.Errorf("the adapter's end record is %d bytes, more than %d", n, maxEndRecord)
			}
			b := make([]byte, n)
			if _, err := io.ReadFull(r, b); err != nil {
				return truncated(ctx, err)
			}
			var end failure
			if err := json.Unmarshal(b, &end); err != nil {
				return fmt.Errorf("the adapter's end record is not a JSON object: %w", err)
			}
			if end.Message != "" {
				return errors.New(end.Message)
			}
			return nil
		default:
			if _, err := io.CopyN(io.Discard, r, n); err != nil {
				return truncated(ctx, err)
			}
		}
	}
}

// truncated explains a live body that stopped before its end record.
func truncated(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errors.New("the adapter's audio stopped before its end record")
	}
	return err
}

func setMember(req *http.Request, member string) {
	if member != "" {
		req.Header.Set(MemberHeader, member)
	}
}

func (r *Remote) get(ctx context.Context, member, path string, q url.Values, v any) error {
	u := "http://adapter" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	setMember(req, member)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := responseError(resp); err != nil {
		return err
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("%s answered: %w", path, err)
	}
	return nil
}

// StatusError is a request the adapter answered with a failure.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "the adapter answered " + strconv.Itoa(e.Code)
}

// Is makes a 404 match ErrNotFound.
func (e *StatusError) Is(target error) bool {
	return target == ErrNotFound && e.Code == http.StatusNotFound
}

func responseError(resp *http.Response) error {
	if resp.StatusCode < 300 {
		return nil
	}
	var f failure
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&f)
	return &StatusError{Code: resp.StatusCode, Message: f.Message}
}
