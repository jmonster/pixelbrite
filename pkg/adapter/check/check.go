// Package check tests an adapter against the spec in README.md without a
// server: it runs the adapter, calls every capability it declares, and
// reports each problem.
package check

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"pixelbrite.site/adapter"
	"pixelbrite.site/adapter/pack"
)

// Options steer a check.
type Options struct {
	// Query is what the search check asks for.
	Query string
	// Setup holds values for setup steps, by step ID, completed in the
	// order the adapter lists them: owner steps for the server, member
	// steps for the check's member.
	Setup map[string]string
	// Audio is how much live audio the play check reads.
	Audio time.Duration
	// Stderr receives the adapter's stderr; nil discards it.
	Stderr io.Writer
}

// Finding is one result of a check.
type Finding struct {
	Check   string
	Outcome string // ok, fail or skip
	Message string
}

// Outcomes of a finding.
const (
	OK   = "ok"
	Fail = "fail"
	Skip = "skip"
)

// checkMember is the member handle the check makes requests for, in the
// form the server issues: 32 lowercase hexadecimal characters.
const checkMember = "c0ffeec0ffeec0ffeec0ffeec0ffee00"

// Run checks the adapter folder or package file at path, reports each
// finding as it goes, and returns how many failed.
func Run(ctx context.Context, path string, opts Options, report func(Finding)) int {
	c := &checker{ctx: ctx, opts: opts, report: report}
	if c.opts.Query == "" {
		c.opts.Query = "love"
	}
	if c.opts.Audio <= 0 {
		c.opts.Audio = 3 * time.Second
	}
	c.run(path)
	return c.failures
}

type checker struct {
	ctx      context.Context
	opts     Options
	report   func(Finding)
	failures int
	manifest adapter.Manifest
	proc     *adapter.Process
}

func (c *checker) ok(check, format string, args ...any) {
	c.report(Finding{check, OK, fmt.Sprintf(format, args...)})
}

func (c *checker) fail(check, format string, args ...any) {
	c.failures++
	c.report(Finding{check, Fail, fmt.Sprintf(format, args...)})
}

func (c *checker) skip(check, format string, args ...any) {
	c.report(Finding{check, Skip, fmt.Sprintf(format, args...)})
}

func (c *checker) run(path string) {
	work, err := os.MkdirTemp("", "pixelbrite-adapter-check-")
	if err != nil {
		c.fail("package", "%v", err)
		return
	}
	defer os.RemoveAll(work)
	dir, ok := c.open(path, work)
	if !ok || !c.readManifest(dir) {
		return
	}
	c.programs(dir)
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	p, err := adapter.Start(ctx, dir, c.manifest, filepath.Join(work, "data"), c.opts.Stderr)
	if err != nil {
		c.fail("handshake", "%v", err)
		return
	}
	defer p.Close()
	c.proc = p
	c.ok("handshake", "ready, serving %s", adapter.FormatCapabilities(p.Capabilities))
	for _, name := range p.Skipped {
		c.fail("capabilities", "the server speaks no version of %s %s", name, c.manifest.Capabilities[name])
	}
	followed := make(chan error, 1)
	_, first := p.Status()
	go func() { followed <- p.Follow(ctx) }()
	select {
	case <-first:
	case err := <-followed:
		c.fail("status", "following /status: %v", err)
		return
	}

	ready := c.status()
	if !ready {
		return
	}
	var tracks []adapter.Item
	if p.Capabilities[adapter.CapSearch] > 0 {
		tracks = append(tracks, c.search()...)
	}
	if p.Capabilities[adapter.CapBrowse] > 0 {
		tracks = append(tracks, c.browse()...)
	}
	var track adapter.Item
	if len(tracks) > 0 {
		track = tracks[0]
	}
	if p.Capabilities[adapter.CapLyrics] > 0 {
		c.lyrics(track)
	}
	if p.Capabilities[adapter.CapContinuation] > 0 {
		c.continuation(track)
	}
	if p.Capabilities[adapter.CapLive] > 0 {
		c.live(track)
	}
	c.redoAndForget()
}

// redoAndForget undoes each member step the check completed and does it
// again, then has the adapter forget the check's member.
func (c *checker) redoAndForget() {
	st, err := c.proc.MemberStatus(c.ctx, checkMember)
	if err != nil {
		c.fail("reset", "%v", err)
		return
	}
	for _, step := range st.Setup {
		value, given := c.opts.Setup[step.ID]
		if !step.Done || !given || step.Audience != adapter.AudienceMember {
			continue
		}
		if err := c.proc.Reset(c.ctx, checkMember, step.ID); err != nil {
			c.fail("reset", "%s: %v", step.ID, err)
			continue
		}
		after, err := c.proc.MemberStatus(c.ctx, checkMember)
		if err != nil || stepDone(after, step.ID) {
			c.fail("reset", "%s is still done after DELETE /setup/%s (%v)", step.ID, step.ID, err)
			continue
		}
		if err := c.proc.Setup(c.ctx, checkMember, step.ID, value); err != nil {
			c.fail("reset", "%s could not be done again: %v", step.ID, err)
			continue
		}
		c.ok("reset", "%s undone and done again", step.ID)
	}
	if err := c.proc.Forget(c.ctx, checkMember); err != nil {
		c.fail("forget", "DELETE /member: %v", err)
		return
	}
	after, err := c.proc.MemberStatus(c.ctx, checkMember)
	switch {
	case err != nil:
		c.fail("forget", "%v", err)
	case hasMemberSteps(after) && after.Ready:
		c.fail("forget", "the member is still ready after DELETE /member")
	default:
		c.ok("forget", "the member's state is gone")
	}
}

func stepDone(st adapter.Status, id string) bool {
	for _, step := range st.Setup {
		if step.ID == id {
			return step.Done
		}
	}
	return false
}

func hasMemberSteps(st adapter.Status) bool {
	for _, step := range st.Setup {
		if step.Audience == adapter.AudienceMember {
			return true
		}
	}
	return false
}

// open returns the adapter folder at path, unpacking a package file into work.
func (c *checker) open(path, work string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil {
		c.fail("package", "%v", err)
		return "", false
	}
	if info.IsDir() {
		return path, true
	}
	root, err := pack.Unpack(c.ctx, path, filepath.Join(work, "package"))
	if err != nil {
		c.fail("package", "%v", err)
		return "", false
	}
	c.ok("package", "unpacked")
	return root, true
}

func (c *checker) readManifest(dir string) bool {
	m, err := adapter.ReadManifest(dir)
	if err != nil {
		var joined interface{ Unwrap() []error }
		if errors.As(err, &joined) {
			for _, e := range joined.Unwrap() {
				c.fail("manifest", "%v", e)
			}
		} else {
			c.fail("manifest", "%v", err)
		}
		return false
	}
	c.manifest = m
	c.ok("manifest", "%s %s (%s)", m.Name, m.Version, m.ID)
	return true
}

func (c *checker) programs(dir string) {
	if err := pack.MarkPrograms(dir, c.manifest); err != nil {
		c.fail("programs", "%v", err)
	}
	platforms := make([]string, 0, len(c.manifest.Programs))
	for platform := range c.manifest.Programs {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)
	for _, platform := range platforms {
		rel := c.manifest.Programs[platform]
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			if platform == adapter.Platform() {
				c.fail("programs", "%s names %s, which the package lacks", platform, rel)
			} else { // packages served through an index often carry one platform
				c.skip("programs", "this package carries no program for %s", platform)
			}
		}
	}
}

// status checks the server's and a member's view, completes the setup
// steps it was given values for, and says whether the member is ready.
func (c *checker) status() bool {
	own, _ := c.proc.Status()
	c.checkStatus("status", own)
	if own.Unavailable != "" {
		c.skip("capabilities", "unavailable on this computer: %s", own.Unavailable)
		return false
	}
	st, err := c.proc.MemberStatus(c.ctx, checkMember)
	if err != nil {
		c.fail("member status", "%v", err)
		return false
	}
	c.checkStatus("member status", st)
	for _, step := range st.Setup {
		if !listed(own, step.ID) {
			c.fail("status", "the member sees step %s, which the server's own /status does not list; the server refuses steps it does not list", step.ID)
		}
	}
	for _, step := range st.Setup {
		value, given := c.opts.Setup[step.ID]
		if step.Done || !given {
			continue
		}
		member := checkMember
		if step.Audience == adapter.AudienceOwner {
			member = ""
		}
		_, changed := c.proc.Status()
		if err := c.proc.Setup(c.ctx, member, step.ID, value); err != nil {
			c.fail("setup", "%s: %v", step.ID, err)
			continue
		}
		select {
		case <-changed:
			c.ok("setup", "%s done; the revision rose", step.ID)
		case <-time.After(5 * time.Second): // an adapter announces a change at once
			c.fail("setup", "%s was accepted but the revision did not rise", step.ID)
		}
	}
	if st, err = c.proc.MemberStatus(c.ctx, checkMember); err != nil {
		c.fail("member status", "%v", err)
		return false
	}
	if !st.Ready {
		var left []string
		for _, step := range st.Setup {
			if !step.Done {
				left = append(left, step.ID)
			}
		}
		c.skip("capabilities", "not ready for a member; pass -setup STEP=VALUE for %s", strings.Join(left, ", "))
		return false
	}
	c.ok("member status", "ready for a member")
	return true
}

func (c *checker) checkStatus(check string, st adapter.Status) {
	bad := 0
	failf := func(format string, args ...any) { bad++; c.fail(check, format, args...) }
	if strings.TrimSpace(st.Name) == "" {
		failf("name is empty")
	}
	if st.Unavailable != "" && len(st.Setup) > 0 {
		failf("unavailable, yet it lists %d setup steps", len(st.Setup))
	}
	seen := map[string]bool{}
	for _, step := range st.Setup {
		if step.ID == "" || seen[step.ID] {
			failf("step %q: ids must be present and unique", step.ID)
		}
		seen[step.ID] = true
		if step.Audience != adapter.AudienceOwner && step.Audience != adapter.AudienceMember {
			failf("step %s: audience %q is neither owner nor member", step.ID, step.Audience)
		}
		if strings.TrimSpace(step.Title) == "" || strings.TrimSpace(step.Button) == "" {
			failf("step %s: title and button must be filled", step.ID)
		}
		switch step.Kind {
		case adapter.StepWebSignIn:
			if u, err := url.Parse(step.URL); err != nil || u.Scheme != "https" || u.Host == "" {
				failf("step %s: a web sign-in needs an https:// url, not %q", step.ID, step.URL)
			}
		case adapter.StepSecret, adapter.StepAction:
		default:
			c.skip(check, "step %s: kind %q is not one this server knows; apps ask for an update", step.ID, step.Kind)
		}
	}
	if bad == 0 {
		c.ok(check, "%s, %d setup steps", readiness(st), len(st.Setup))
	}
}

func listed(st adapter.Status, id string) bool {
	for _, step := range st.Setup {
		if step.ID == id {
			return true
		}
	}
	return false
}

func readiness(st adapter.Status) string {
	switch {
	case st.Unavailable != "":
		return "unavailable"
	case st.Ready:
		return "ready"
	}
	return "not ready"
}

// checkItems reports problems with items and returns their tracks.
func (c *checker) checkItems(check string, items []adapter.Item) []adapter.Item {
	var tracks []adapter.Item
	seen := map[string]bool{}
	bad := 0
	failf := func(format string, args ...any) { bad++; c.fail(check, format, args...) }
	for i, it := range items {
		switch {
		case it.ID == "":
			failf("item %d has no id", i)
		case seen[it.Kind+"\x00"+it.ID]:
			failf("item %s %s appears twice", it.Kind, it.ID)
		}
		seen[it.Kind+"\x00"+it.ID] = true
		if it.Kind == "" {
			failf("item %s has no kind", it.ID)
		}
		if strings.TrimSpace(it.Title) == "" {
			failf("item %s has no title", it.ID)
		}
		if it.Artwork != "" {
			if u, err := url.Parse(it.Artwork); err != nil || !u.IsAbs() {
				failf("item %s artwork %q is not an absolute URL", it.ID, it.Artwork)
			}
		}
		if it.Kind == adapter.KindTrack {
			if it.DurationMs <= 0 {
				failf("track %s has no duration_ms", it.ID)
			}
			tracks = append(tracks, it)
		}
	}
	if bad == 0 {
		c.ok(check, "%d items, %d tracks", len(items), len(tracks))
	}
	return tracks
}

func (c *checker) search() []adapter.Item {
	items, err := c.proc.Search(c.ctx, checkMember, c.opts.Query)
	if err != nil {
		c.fail("search", "%v", err)
		return nil
	}
	if len(items) == 0 {
		c.skip("search", "nothing matched %q; pass -query to search for something it has", c.opts.Query)
		return nil
	}
	return c.checkItems("search", items)
}

func (c *checker) browse() []adapter.Item {
	page, err := c.proc.Browse(c.ctx, checkMember, "", "")
	if err != nil {
		c.fail("browse", "the top level: %v", err)
		return nil
	}
	items := page.Items
	for pages := 1; page.Next != "" && pages < 3; pages++ {
		if page, err = c.proc.Browse(c.ctx, checkMember, "", page.Next); err != nil {
			c.fail("browse", "the page after the first: %v", err)
			break
		}
		items = append(items, page.Items...)
	}
	tracks := c.checkItems("browse", items)
	for _, it := range items {
		if it.Kind == adapter.KindTrack {
			continue
		}
		inner, err := c.proc.Browse(c.ctx, checkMember, it.ID, "")
		if err != nil {
			c.fail("browse", "%s %s: %v", it.Kind, it.ID, err)
			break
		}
		tracks = append(tracks, c.checkItems("browse "+it.Kind, inner.Items)...)
		break
	}
	if _, err := c.proc.Browse(c.ctx, checkMember, "pixelbrite-check-missing", ""); !errors.Is(err, adapter.ErrNotFound) {
		c.fail("browse", "an unknown id answered %v; want 404", err)
	}
	return tracks
}

func (c *checker) lyrics(track adapter.Item) {
	if track.ID == "" {
		c.skip("lyrics", "no track to ask for")
		return
	}
	l, err := c.proc.Lyrics(c.ctx, checkMember, track.ID)
	switch {
	case errors.Is(err, adapter.ErrNoLyrics):
		c.ok("lyrics", "%s has none (404)", track.ID)
		return
	case err != nil:
		c.fail("lyrics", "%s: %v", track.ID, err)
		return
	}
	var last int64
	for i, line := range l.Lines {
		if !l.Synced && line.StartMs != 0 {
			c.fail("lyrics", "untimed lyrics carry start_ms %d on line %d", line.StartMs, i)
			return
		}
		if line.StartMs < last {
			c.fail("lyrics", "line %d starts at %d ms, before the line above it", i, line.StartMs)
			return
		}
		last = line.StartMs
	}
	c.ok("lyrics", "%d lines, synced %v", len(l.Lines), l.Synced)
}

func (c *checker) continuation(track adapter.Item) {
	var played []string
	if track.ID != "" {
		played = []string{track.ID}
	}
	items, err := c.proc.Continue(c.ctx, checkMember, played)
	if err != nil {
		c.fail("continuation", "%v", err)
		return
	}
	c.checkItems("continuation", items)
}

func (c *checker) live(track adapter.Item) {
	live := c.manifest.Live
	if err := c.proc.Play(c.ctx, checkMember, "pixelbrite-check-missing", 0, &audioCheck{}); err == nil {
		c.fail("live", "an unknown track played")
	} else if status := (*adapter.StatusError)(nil); !errors.As(err, &status) || status.Message == "" {
		c.fail("live", "an unknown track failed without a status and message: %v", err)
	}
	if track.ID == "" {
		c.skip("live", "no track to play")
		return
	}
	c.play("live", track.ID, 0)
	if live != nil && live.Seek {
		c.play("live seek", track.ID, 1000)
	}
	if live != nil && live.Pace == adapter.PaceReader {
		c.playToEnd(track)
	} else {
		c.skip("live end", "realtime audio takes the track's length to reach its end record")
	}
}

// playToEnd reads a reader-paced track through its end record.
func (c *checker) playToEnd(track adapter.Item) {
	// A hang guard: a reader-paced adapter produces audio faster than it plays.
	ctx, cancel := context.WithTimeout(c.ctx, time.Minute)
	defer cancel()
	out := &audioCheck{}
	err := c.proc.Play(ctx, checkMember, track.ID, 0, out)
	played := time.Duration(track.DurationMs) * time.Millisecond
	switch {
	case err != nil:
		c.fail("live end", "%s after %s of audio: %v", track.ID, out.seconds(), err)
	case out.problem != "":
		c.fail("live end", "%s: %s", track.ID, out.problem)
	case out.bytes%8 != 0:
		c.fail("live end", "%s ended mid-frame after %d bytes", track.ID, out.bytes)
	case played > 0 && (out.seconds() < played-time.Second || out.seconds() > played+time.Second):
		c.fail("live end", "%s played %s, but its duration_ms says %s", track.ID, out.seconds(), played)
	default:
		c.ok("live end", "%s played to its end record: %s of audio", track.ID, out.seconds())
	}
}

func (c *checker) play(check, id string, startMs int64) {
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	out := &audioCheck{want: c.opts.Audio, stop: cancel}
	started := time.Now()
	err := c.proc.Play(ctx, checkMember, id, startMs, out)
	elapsed := time.Since(started)
	switch {
	case out.rate <= 0:
		c.fail(check, "%s sent no audio: %v", id, err)
	case out.problem != "":
		c.fail(check, "%s: %s", id, out.problem)
	case out.bytes%8 != 0 && ctx.Err() == nil: // stopping it may cut the last frame
		c.fail(check, "%s ended mid-frame after %d bytes", id, out.bytes)
	case err != nil && ctx.Err() == nil:
		c.fail(check, "%s failed after %s of audio: %v", id, out.seconds(), err)
	default:
		c.ok(check, "%s at %g Hz: %s of audio in %s", id, out.rate, out.seconds(), elapsed.Round(time.Millisecond))
	}
}

// audioCheck reads live audio until it has want of it, checking each sample.
type audioCheck struct {
	want    time.Duration
	stop    func()
	rate    float64
	bytes   int64
	partial []byte
	problem string
}

func (a *audioCheck) SetRate(hz float64) { a.rate = hz }

func (a *audioCheck) Write(b []byte) (int, error) {
	a.bytes += int64(len(b))
	data := append(a.partial, b...)
	whole := len(data) / 4 * 4
	for i := 0; i < whole && a.problem == ""; i += 4 {
		v := math.Float32frombits(binary.LittleEndian.Uint32(data[i:]))
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			a.problem = fmt.Sprintf("sample %d is not a finite number", (a.bytes-int64(len(data))+int64(i))/4)
		}
	}
	a.partial = append(a.partial[:0], data[whole:]...)
	if a.stop != nil && a.rate > 0 && a.seconds() >= a.want {
		a.stop()
	}
	return len(b), nil
}

func (a *audioCheck) seconds() time.Duration {
	if a.rate <= 0 {
		return 0
	}
	return time.Duration(float64(a.bytes/8) / a.rate * float64(time.Second)).Round(10 * time.Millisecond)
}
