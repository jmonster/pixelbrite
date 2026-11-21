package browser

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pixelbrite.site/adapter"
)

// On Linux each Chrome gets a private virtual display (Xvfb) and a private
// PulseAudio server whose only output is a null sink, so a server with no
// screen or sound card plays the same way, and each Chrome's sound is
// captured apart from every other.

const (
	// helperLimit bounds Xvfb and PulseAudio starting.
	helperLimit = 10 * time.Second
	// sinkName is the null sink Chrome plays into.
	sinkName = "pixelbrite"
	// captureRate is the null sink's rate and the captured PCM's.
	captureRate = 48000
)

type host struct {
	dir     string // runtime folder: sockets and PulseAudio state
	display string
	xvfb    *exec.Cmd
	pulse   *exec.Cmd
}

// Unavailable says why this computer cannot capture Chrome, or "".
func Unavailable() string {
	for _, tool := range []string{"Xvfb", "pulseaudio", "pactl", "parec"} {
		if _, err := exec.LookPath(tool); err != nil {
			return "Playing in Google Chrome on Linux needs Xvfb and PulseAudio (pulseaudio, pactl and parec) installed; " + tool + " is missing."
		}
	}
	return ""
}

func startHost(ctx context.Context, profile string) (*host, error) {
	if reason := Unavailable(); reason != "" {
		return nil, errors.New(reason)
	}
	// Socket paths are short: a long data folder would pass the socket limit.
	dir, err := os.MkdirTemp("", "pbb-")
	if err != nil {
		return nil, err
	}
	h := &host{dir: dir}
	if err := h.startDisplay(ctx); err != nil {
		h.stop()
		return nil, err
	}
	if err := h.startAudio(ctx); err != nil {
		h.stop()
		return nil, err
	}
	return h, nil
}

// startDisplay starts Xvfb on a free display, which it reports on fd 3.
func (h *host) startDisplay(ctx context.Context) error {
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	h.xvfb = exec.Command("Xvfb", "-displayfd", "3", "-screen", "0", "1280x900x24", "-nolisten", "tcp")
	h.xvfb.ExtraFiles = []*os.File{w}
	err = h.xvfb.Start()
	w.Close()
	if err != nil {
		h.xvfb = nil
		return fmt.Errorf("Xvfb did not start: %w", err)
	}
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(r).ReadString('\n')
		line <- strings.TrimSpace(s)
	}()
	select {
	case n := <-line:
		if n == "" {
			return errors.New("Xvfb exited before it opened a display")
		}
		h.display = ":" + n
		return nil
	case <-time.After(helperLimit):
		return fmt.Errorf("Xvfb did not open a display within %s", helperLimit)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *host) socket() string { return filepath.Join(h.dir, "pulse") }

// startAudio starts a PulseAudio server with only a null sink and a socket
// in the runtime folder.
func (h *host) startAudio(ctx context.Context) error {
	h.pulse = exec.Command("pulseaudio", "--daemonize=no", "-n", "--exit-idle-time=-1", "--disable-shm=yes", "--use-pid-file=no",
		"-L", "module-native-protocol-unix socket="+h.socket()+" auth-anonymous=1",
		"-L", fmt.Sprintf("module-null-sink sink_name=%s rate=%d channels=2", sinkName, captureRate))
	h.pulse.Env = append(os.Environ(), "HOME="+h.dir, "XDG_RUNTIME_DIR="+h.dir, "PULSE_RUNTIME_PATH="+h.dir, "PULSE_STATE_PATH="+h.dir)
	stderr := &tail{}
	h.pulse.Stderr = stderr
	if err := h.pulse.Start(); err != nil {
		h.pulse = nil
		return fmt.Errorf("PulseAudio did not start: %w", err)
	}
	exited := make(chan struct{})
	go func() { _ = h.pulse.Wait(); close(exited) }()
	deadline := time.After(helperLimit)
	for {
		if err := exec.CommandContext(ctx, "pactl", "--server=unix:"+h.socket(), "info").Run(); err == nil {
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("PulseAudio exited: %s", stderr.String())
		case <-deadline:
			return fmt.Errorf("PulseAudio did not answer within %s", helperLimit)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// args are Chrome's flags here: a container's small /dev/shm is not used.
func (h *host) args() []string { return []string{"--disable-dev-shm-usage"} }

func (h *host) env() []string {
	return []string{"DISPLAY=" + h.display, "PULSE_SERVER=unix:" + h.socket(), "PULSE_SINK=" + sinkName}
}

// windowed is false: the virtual display is never hidden, so nothing is
// minimized.
func (h *host) windowed() bool { return false }

func (h *host) stop() {
	if h == nil {
		return
	}
	for _, cmd := range []*exec.Cmd{h.pulse, h.xvfb} {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	_ = os.RemoveAll(h.dir)
}

// Capture captures Chrome's sound from the moment it opens a stream on its
// null sink, into out as float32 stereo frames, until ctx ends or finished
// closes. It returns ErrNoSound when finished closes before Chrome played.
func (b *Browser) Capture(ctx context.Context, finished <-chan struct{}, out adapter.PCM) error {
	h := b.host
	server := "--server=unix:" + h.socket()
	finished = either(finished, b.done)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Wait for Chrome's stream: one already open, or the next one.
	events := exec.CommandContext(ctx, "pactl", server, "subscribe")
	pipe, err := events.StdoutPipe()
	if err != nil {
		return err
	}
	if err := events.Start(); err != nil {
		return fmt.Errorf("pactl did not start: %w", err)
	}
	defer func() { cancel(); _ = events.Wait() }()
	newStream := make(chan struct{})
	go func() {
		lines := bufio.NewScanner(pipe)
		for lines.Scan() {
			if strings.Contains(lines.Text(), "'new' on sink-input") {
				close(newStream)
				break
			}
		}
		_, _ = io.Copy(io.Discard, pipe)
	}()
	open, err := exec.CommandContext(ctx, "pactl", server, "list", "short", "sink-inputs").Output()
	if err != nil {
		return fmt.Errorf("pactl could not list Chrome's streams: %w", err)
	}
	if len(strings.TrimSpace(string(open))) == 0 {
		select {
		case <-newStream:
		case <-finished:
			return ErrNoSound
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	rec := exec.CommandContext(ctx, "parec", server, "--device="+sinkName+".monitor", "--raw",
		"--format=float32le", "--channels=2", fmt.Sprintf("--rate=%d", captureRate), "--latency-msec=20")
	pcm, err := rec.StdoutPipe()
	if err != nil {
		return err
	}
	if err := rec.Start(); err != nil {
		return fmt.Errorf("parec did not start: %w", err)
	}
	out.SetRate(captureRate)
	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(out, pcm)
		copied <- err
	}()
	select {
	case err := <-copied:
		_ = rec.Wait()
		if err != nil {
			return err
		}
		return errors.New("parec stopped capturing Chrome's sound")
	case <-finished:
		cancel()
		<-copied
		_ = rec.Wait()
		return nil
	case <-ctx.Done():
		<-copied
		_ = rec.Wait()
		return ctx.Err()
	}
}
