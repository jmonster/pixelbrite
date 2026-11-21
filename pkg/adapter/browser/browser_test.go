package browser

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCookiesReachChromeAsTheBrowserStoredThem(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	later := now.Add(365 * 24 * time.Hour)
	dc, ok := CookieFrom(&http.Cookie{Domain: ".example.com", Name: "session", Value: "s", Path: "/", Expires: later, Secure: true}, "open.example.com", now)
	if !ok || dc.Domain != ".example.com" || dc.URL != "" || !dc.Secure || dc.Expires != float64(later.Unix()) {
		t.Errorf("domain cookie = %+v", dc)
	}
	device, _ := CookieFrom(&http.Cookie{Domain: ".example.com", Name: "__Host-device_id", Value: "dev", Path: "/", Expires: later}, "open.example.com", now)
	if device.Domain != "" || device.URL != "https://open.example.com/" {
		t.Errorf("__Host- cookie = %+v, want host-only on open.example.com", device)
	}
	host, _ := CookieFrom(&http.Cookie{Name: "host_only", Value: "h", SameSite: http.SameSiteLaxMode}, "open.example.com", now)
	if host.Domain != "" || host.URL != "https://open.example.com/" || host.Path != "/" || host.SameSite != "Lax" || host.Expires != 0 {
		t.Errorf("host-only session cookie = %+v", host)
	}
	if _, ok := CookieFrom(&http.Cookie{Domain: ".example.com", Name: "old", Value: "x", Expires: now}, "open.example.com", now); ok {
		t.Error("an expired cookie reached Chrome")
	}
}

// fakeChrome answers DevTools calls on a pipe as Chrome does: each message
// NUL-terminated, events interleaved with replies.
func fakeChrome(t *testing.T, commands io.Reader, replies io.Writer) {
	br := bufio.NewReader(commands)
	for {
		msg, err := br.ReadBytes(0)
		if err != nil {
			return
		}
		var call struct {
			ID        int64  `json:"id"`
			Method    string `json:"method"`
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(msg[:len(msg)-1], &call); err != nil {
			t.Errorf("command is not JSON: %q", msg)
			return
		}
		event, _ := json.Marshal(map[string]any{"method": "Page.frameNavigated", "params": map[string]any{}})
		var reply []byte
		if call.Method == "Fail" {
			reply, _ = json.Marshal(map[string]any{"id": call.ID, "error": map[string]any{"code": -32000, "message": "no"}})
		} else {
			reply, _ = json.Marshal(map[string]any{"id": call.ID, "result": map[string]any{"method": call.Method, "session": call.SessionID}})
		}
		_, _ = replies.Write(append(append(event, 0), append(reply, 0)...))
	}
}

func TestDevToolsPipeMatchesRepliesToCalls(t *testing.T) {
	cmdR, cmdW := io.Pipe()
	replyR, replyW := io.Pipe()
	go fakeChrome(t, cmdR, replyW)
	c := newCDP(cmdW, replyR)
	ctx := context.Background()
	var got struct{ Method, Session string }
	if err := c.call(ctx, "S1", "Runtime.evaluate", map[string]any{"expression": "1"}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Method != "Runtime.evaluate" || got.Session != "S1" {
		t.Errorf("page call answered as %+v", got)
	}
	if err := c.call(ctx, "", "Browser.getVersion", nil, &got); err != nil || got.Session != "" {
		t.Errorf("browser call: %v, %+v", err, got)
	}
	if err := c.call(ctx, "", "Fail", nil, nil); err == nil {
		t.Error("a refused call succeeded")
	}
	replyW.Close()
	if err := c.call(ctx, "", "Browser.getVersion", nil, nil); err == nil {
		t.Error("a call after Chrome closed the pipe succeeded")
	}
}

// Runs where PIXELBRITE_CHROME names a Google Chrome; the page is a data:
// URL, so nothing leaves the computer.
func TestScriptsRunBeforeEachDocument(t *testing.T) {
	if Chrome() == "" {
		t.Skip("PIXELBRITE_CHROME names no Google Chrome")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := Launch(ctx, Chrome(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.OnNewDocument(ctx, `globalThis.hooked = true`); err != nil {
		t.Fatal(err)
	}
	if err := b.Navigate(ctx, `data:text/html,<script>document.title = String(globalThis.hooked)</script>`); err != nil {
		t.Fatal(err)
	}
	title := ""
	for title != "true" && ctx.Err() == nil {
		_ = b.Eval(ctx, `document.title`, &title)
		if title == "undefined" {
			t.Fatal("the page's own script ran before the OnNewDocument script")
		}
	}
	if title != "true" {
		t.Fatalf("title %q", title)
	}
}

func TestAProfileIsSeededOnlyWhenNewOrAfterReseed(t *testing.T) {
	k := &Keeper{Profile: filepath.Join(t.TempDir(), "chrome")}
	if !k.seeding() {
		t.Error("a new profile is not seeded")
	}
	if err := os.MkdirAll(k.Profile, 0o700); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(k.Profile, "Cookies")
	if err := os.WriteFile(kept, []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if k.seeding() {
		t.Error("a profile Chrome made is seeded again")
	}
	if err := k.Reseed(); err != nil {
		t.Fatal(err)
	}
	if !k.seeding() {
		t.Error("a new sign-in is not seeded")
	}
	if b, err := os.ReadFile(kept); err != nil || string(b) != "rotated" {
		t.Errorf("Reseed lost the profile: %q %v", b, err)
	}
	if err := k.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(k.Profile); !os.IsNotExist(err) {
		t.Errorf("Reset kept the profile: %v", err)
	}
}
