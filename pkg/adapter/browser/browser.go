// Package browser plays web pages in Google Chrome for adapters and captures
// what they play. An adapter that lists adapter.RequireChrome in its manifest
// gets Chrome's path from the server (Chrome); it launches one Chrome per
// profile folder, drives the page over Chrome's DevTools pipe, and captures
// that Chrome's audio as live PCM with Capture. Chrome runs as an ordinary
// desktop browser: on macOS and Windows in a window that can be minimized
// once it has played, on Linux on a private virtual display, so a server
// with no screen plays the same way.
package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pixelbrite.site/adapter"
)

var (
	// ErrExited is Chrome quitting while it was in use.
	ErrExited = errors.New("Google Chrome quit")
	// ErrNoSound is playback that ended before Chrome played any sound.
	ErrNoSound = errors.New("Google Chrome played no sound")
	// ErrNoChrome is an adapter started before the server installed Chrome.
	ErrNoChrome = errors.New("Google Chrome is not installed on this server yet")
)

// closeLimit bounds Chrome's own shutdown before it is killed.
const closeLimit = 5 * time.Second

// Chrome is the path of the Google Chrome the server provides, or "".
func Chrome() string { return os.Getenv(adapter.RequirementEnv(adapter.RequireChrome)) }

// args start Chrome on profile with the DevTools protocol on fds 3 and 4.
// Pages play without a click, and a minimized or covered window keeps its
// timers.
func args(profile string, extra []string) []string {
	return append([]string{
		"--user-data-dir=" + profile,
		"--remote-debugging-pipe",
		"--no-first-run",
		"--no-default-browser-check",
		"--use-mock-keychain",
		"--lang=en-US",
		"--autoplay-policy=no-user-gesture-required",
		"--disable-background-timer-throttling",
		"--disable-renderer-backgrounding",
		"--disable-backgrounding-occluded-windows",
	}, append(extra, "about:blank")...)
}

// Browser is one running Chrome and its first page.
type Browser struct {
	cmd     *exec.Cmd
	done    chan struct{} // closed once Chrome exited
	cdp     *cdp
	session string // the page's DevTools session
	window  int
	host    *host // the display and audio Chrome uses here
	// minimized is set once the window is minimized. Chrome defers media in
	// a hidden page that has never played, so a window is minimized only
	// after the page has played.
	minimized bool
}

// Launch starts Chrome at exe with its profile in the folder profile and
// attaches to its page.
func Launch(ctx context.Context, exe, profile string) (*Browser, error) {
	if exe == "" {
		return nil, ErrNoChrome
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, err
	}
	h, err := startHost(ctx, profile)
	if err != nil {
		return nil, err
	}
	b, err := launch(ctx, exe, profile, h, nil)
	if errors.Is(err, errNoSandbox) {
		// Some containers forbid Chrome's sandbox; it then runs without it
		// and says so every time.
		fmt.Fprintf(os.Stderr, "browser: Chrome runs without its sandbox: this system does not allow it, or the server runs as root\n")
		b, err = launch(ctx, exe, profile, h, []string{"--no-sandbox"})
	}
	if err != nil {
		h.stop()
		return nil, err
	}
	return b, nil
}

// errNoSandbox is Chrome refusing to start without a usable sandbox.
var errNoSandbox = errors.New("Chrome found no usable sandbox")

func launch(ctx context.Context, exe, profile string, h *host, extra []string) (*Browser, error) {
	// Chrome reads commands from fd 3 and writes replies to fd 4.
	cmdR, cmdW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	replyR, replyW, err := os.Pipe()
	if err != nil {
		cmdR.Close()
		cmdW.Close()
		return nil, err
	}
	cmd := exec.Command(exe, args(profile, append(h.args(), extra...))...)
	cmd.Env = append(os.Environ(), h.env()...)
	cmd.ExtraFiles = []*os.File{cmdR, replyW}
	stderr := &tail{}
	cmd.Stderr = stderr
	err = cmd.Start()
	cmdR.Close()
	replyW.Close()
	if err != nil {
		cmdW.Close()
		replyR.Close()
		return nil, fmt.Errorf("Google Chrome did not start: %w", err)
	}
	b := &Browser{cmd: cmd, done: make(chan struct{}), cdp: newCDP(cmdW, replyR), host: h}
	go func() {
		_ = cmd.Wait()
		cmdW.Close()
		replyR.Close()
		close(b.done)
	}()
	if err := b.attach(ctx); err != nil {
		b.kill()
		if said := stderr.String(); strings.Contains(said, "No usable sandbox") || strings.Contains(said, "Running as root without --no-sandbox") {
			return nil, errNoSandbox
		}
		if last := stderr.String(); last != "" {
			return nil, fmt.Errorf("%w; Chrome said: %s", err, last)
		}
		return nil, err
	}
	return b, nil
}

// attach takes Chrome's first page and turns on its page events, which
// OnNewDocument scripts need, and its media and log events.
func (b *Browser) attach(ctx context.Context) error {
	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := b.cdp.call(ctx, "", "Target.getTargets", nil, &targets); err != nil {
		return err
	}
	target := ""
	for _, t := range targets.TargetInfos {
		if t.Type == "page" {
			target = t.TargetID
			break
		}
	}
	if target == "" {
		return errors.New("Google Chrome opened no page")
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := b.cdp.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target, "flatten": true}, &attached); err != nil {
		return err
	}
	b.session = attached.SessionID
	for _, domain := range []string{"Page.enable", "Media.enable", "Log.enable"} {
		if err := b.cdp.call(ctx, b.session, domain, nil, nil); err != nil {
			return err
		}
	}
	var window struct {
		WindowID int `json:"windowId"`
	}
	if err := b.cdp.call(ctx, "", "Browser.getWindowForTarget", map[string]any{"targetId": target}, &window); err != nil {
		return err
	}
	b.window = window.WindowID
	return nil
}

// Done is closed once Chrome has exited.
func (b *Browser) Done() <-chan struct{} { return b.done }

// Alive reports whether Chrome is still running.
func (b *Browser) Alive() bool {
	select {
	case <-b.done:
		return false
	default:
		return true
	}
}

// Close asks Chrome to close, kills it if it has not within closeLimit, and
// waits for it and the display and audio it used to stop.
func (b *Browser) Close() {
	if b == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeLimit)
	defer cancel()
	_ = b.cdp.call(ctx, "", "Browser.close", nil, nil)
	select {
	case <-b.done:
	case <-ctx.Done():
		b.kill()
	}
	b.host.stop()
}

func (b *Browser) kill() {
	_ = b.cmd.Process.Kill()
	<-b.done
}

// Minimize minimizes the page's window once the page has played, where
// Chrome has a window to minimize.
func (b *Browser) Minimize(ctx context.Context) error {
	if b.minimized || !b.host.windowed() {
		return nil
	}
	err := b.cdp.call(ctx, "", "Browser.setWindowBounds",
		map[string]any{"windowId": b.window, "bounds": map[string]any{"windowState": "minimized"}}, nil)
	b.minimized = err == nil
	return err
}

// Navigate loads url in the page.
func (b *Browser) Navigate(ctx context.Context, url string) error {
	return b.cdp.call(ctx, b.session, "Page.navigate", map[string]any{"url": url}, nil)
}

// OnNewDocument runs script in every document the page loads from now on,
// before the document's own scripts.
func (b *Browser) OnNewDocument(ctx context.Context, script string) error {
	return b.cdp.call(ctx, b.session, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": script}, nil)
}

// Eval runs expr in the page, waits for it when it is a promise, and decodes
// its JSON-serializable value into v.
func (b *Browser) Eval(ctx context.Context, expr string, v any) error {
	var res struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := b.cdp.call(ctx, b.session, "Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": true}, &res); err != nil {
		return err
	}
	if res.ExceptionDetails != nil {
		return fmt.Errorf("the page failed a script: %s", res.ExceptionDetails.Text)
	}
	if v == nil || len(res.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(res.Result.Value, v)
}

// Events returns Chrome's latest media and log events from the page, oldest
// first, for reporting why playback failed.
func (b *Browser) Events() []string { return b.cdp.recent() }

// Cookie is a cookie as Chrome stores it.
type Cookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	URL      string  `json:"url,omitempty"`
	Domain   string  `json:"domain,omitempty"`
	Path     string  `json:"path,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	HTTPOnly bool    `json:"httpOnly,omitempty"`
	SameSite string  `json:"sameSite,omitempty"`
	Expires  float64 `json:"expires,omitempty"`
}

// CookieFrom is c as Chrome stores it; host is the host that set c. A
// host-only cookie, and every __Host- cookie, belongs to that host. ok is
// false for a cookie that has expired by now.
func CookieFrom(c *http.Cookie, host string, now time.Time) (Cookie, bool) {
	if !c.Expires.IsZero() && !c.Expires.After(now) {
		return Cookie{}, false
	}
	out := Cookie{Name: c.Name, Value: c.Value, Path: c.Path, Secure: c.Secure, HTTPOnly: c.HttpOnly}
	if c.Domain != "" && !strings.HasPrefix(c.Name, "__Host-") {
		out.Domain = c.Domain
	} else {
		out.URL = "https://" + host + "/"
	}
	if out.Path == "" {
		out.Path = "/"
	}
	switch c.SameSite {
	case http.SameSiteLaxMode:
		out.SameSite = "Lax"
	case http.SameSiteStrictMode:
		out.SameSite = "Strict"
	case http.SameSiteNoneMode:
		out.SameSite = "None"
	}
	if !c.Expires.IsZero() {
		out.Expires = float64(c.Expires.UnixMilli()) / 1000
	}
	return out, true
}

// SetCookies stores cookies in Chrome's profile.
func (b *Browser) SetCookies(ctx context.Context, cookies []Cookie) error {
	return b.cdp.call(ctx, "", "Storage.setCookies", map[string]any{"cookies": cookies}, nil)
}

// tail keeps the end of Chrome's stderr for error reports.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

const tailBytes = 2048

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailBytes {
		t.buf = t.buf[len(t.buf)-tailBytes:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// cdp is a DevTools protocol connection over Chrome's pipe: NUL-terminated
// JSON messages, replies matched to calls by id. Media and log events are
// kept for failure reports; other events are dropped.
type cdp struct {
	w       io.WriteCloser
	writeMu sync.Mutex
	next    atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan cdpReply
	events  []string
	closed  chan struct{}
	err     error // why the connection closed; set before closed closes
}

// keptEvents bounds the events kept for a failure report.
const keptEvents = 40

type cdpReply struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newCDP(w io.WriteCloser, r io.Reader) *cdp {
	c := &cdp{w: w, pending: map[int64]chan cdpReply{}, closed: make(chan struct{})}
	go c.read(r)
	return c
}

func (c *cdp) read(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		msg, err := br.ReadBytes(0)
		if err != nil {
			c.err = fmt.Errorf("Chrome closed its control connection: %w", err)
			close(c.closed)
			return
		}
		var reply cdpReply
		if json.Unmarshal(msg[:len(msg)-1], &reply) != nil {
			continue
		}
		if reply.ID == 0 {
			if strings.HasPrefix(reply.Method, "Media.") || reply.Method == "Log.entryAdded" {
				event := string(msg[:len(msg)-1])
				if len(event) > 400 {
					event = event[:400]
				}
				c.mu.Lock()
				c.events = append(c.events, event)
				if len(c.events) > keptEvents {
					c.events = c.events[len(c.events)-keptEvents:]
				}
				c.mu.Unlock()
			}
			continue
		}
		c.mu.Lock()
		ch := c.pending[reply.ID]
		delete(c.pending, reply.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- reply
		}
	}
}

func (c *cdp) recent() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.events...)
}

// call sends method to the browser, or to the page whose session it names,
// and decodes the reply's result into result when it is not nil.
func (c *cdp) call(ctx context.Context, session, method string, params, result any) error {
	id := c.next.Add(1)
	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if session != "" {
		msg["sessionId"] = session
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	ch := make(chan cdpReply, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	c.writeMu.Lock()
	_, err = c.w.Write(append(b, 0))
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("Chrome stopped taking commands: %w", err)
	}
	select {
	case reply := <-ch:
		if reply.Error != nil {
			return fmt.Errorf("Chrome refused %s: %s", method, reply.Error.Message)
		}
		if result != nil {
			return json.Unmarshal(reply.Result, result)
		}
		return nil
	case <-c.closed:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
