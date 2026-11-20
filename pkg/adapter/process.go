package adapter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	// ReadyTimeout bounds how long an adapter may take to start serving.
	ReadyTimeout = 30 * time.Second
	// StopGrace is how long an adapter has to exit after its stdin closes
	// before it and every process it started are killed.
	StopGrace = 5 * time.Second
	// pipeGrace is how long the adapter's output is still read after it and
	// its process group are gone, for a descendant that left the group.
	pipeGrace = time.Second
	// maxLine bounds the handshake line.
	maxLine = 64 << 10
)

// Process is a running adapter and the Remote that reaches it.
type Process struct {
	*Remote
	// Capabilities are the versions the server chose for this run.
	Capabilities map[string]int
	// Skipped are the declared capabilities the two sides share no version of.
	Skipped []string
	stdin   *os.File
	pid     int
	stop    sync.Once
	done    chan struct{}
}

// Start runs the adapter in dir described by m, with its state in dataDir,
// and returns once it serves. It returns an *IncompatibleError when the
// adapter and this package cannot work together. Stderr receives the
// adapter's stderr; nil discards it. The adapter stops with ctx, as Close
// stops it.
func Start(ctx context.Context, dir string, m Manifest, dataDir string, stderr io.Writer) (*Process, error) {
	return start(ctx, dir, m, dataDir, stderr, Supported())
}

func start(ctx context.Context, dir string, m Manifest, dataDir string, stderr io.Writer, ours map[string]Versions) (*Process, error) {
	// The program starts in dataDir, so a relative path would resolve there.
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	program, err := m.Program(dir)
	if err != nil {
		return nil, err
	}
	chosen, skipped, err := Negotiate(m.Capabilities, ours)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	sockets, err := os.MkdirTemp(socketRoot(), "pba-")
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(sockets, "adapter.sock")
	if stderr == nil {
		stderr = io.Discard
	}
	// The pipes are ours rather than exec's, so Wait returns when the
	// adapter exits even while a process it started still holds one.
	var pipes [6]*os.File
	closeAll := func() {
		for _, f := range pipes {
			if f != nil {
				_ = f.Close()
			}
		}
	}
	for i := 0; i < len(pipes) && err == nil; i += 2 {
		pipes[i], pipes[i+1], err = os.Pipe()
	}
	if err != nil {
		closeAll()
		_ = os.RemoveAll(sockets)
		return nil, err
	}
	stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW := pipes[0], pipes[1], pipes[2], pipes[3], pipes[4], pipes[5]
	cmd := exec.Command(program, "-listen", socket, "-data", dataDir, "-capabilities", FormatCapabilities(chosen))
	cmd.Dir = dataDir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, stdoutW, stderrW
	ownGroup(cmd)
	if err := cmd.Start(); err != nil {
		closeAll()
		_ = os.RemoveAll(sockets)
		return nil, err
	}
	// The adapter holds its own copies; the server keeps only its ends.
	_, _, _ = stdinR.Close(), stdoutW.Close(), stderrW.Close()
	p := &Process{Capabilities: chosen, Skipped: skipped, stdin: stdinW, pid: cmd.Process.Pid, done: make(chan struct{})}

	answer := make(chan string, 1)
	stdoutDone, stderrDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stdoutDone)
		lines := bufio.NewScanner(stdoutR)
		lines.Buffer(make([]byte, 4096), maxLine)
		switch {
		case lines.Scan():
			answer <- lines.Text()
		case errors.Is(lines.Err(), bufio.ErrTooLong):
			answer <- "too-long"
		default:
			answer <- ""
		}
		_, _ = io.Copy(io.Discard, stdoutR) // later output is read and discarded
	}()
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(stderr, stderrR)
	}()
	go func() {
		_ = cmd.Wait()
		killGroup(cmd.Process) // the adapter's helpers go with it
		grace := time.NewTimer(pipeGrace)
		for _, done := range []chan struct{}{stdoutDone, stderrDone} {
			select {
			case <-done:
			case <-grace.C:
			}
		}
		grace.Stop()
		_, _, _ = stdoutR.Close(), stderrR.Close(), stdinW.Close()
		<-stdoutDone
		<-stderrDone
		_ = os.RemoveAll(sockets)
		close(p.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Close()
		case <-p.done:
		}
	}()

	timeout := time.NewTimer(ReadyTimeout)
	defer timeout.Stop()
	var line string
	select {
	case line = <-answer:
	case <-timeout.C:
		_ = p.Close()
		return nil, fmt.Errorf("not ready after %s", ReadyTimeout)
	case <-ctx.Done():
		_ = p.Close()
		return nil, ctx.Err()
	}
	if err := handshake(line); err != nil {
		_ = p.Close()
		return nil, err
	}
	p.Remote = Dial("unix", socket)
	return p, nil
}

// handshake reads the adapter's first line: "ready" or "incompatible REASON".
func handshake(line string) error {
	word, reason, _ := strings.Cut(line, " ")
	switch word {
	case "ready":
		return nil
	case "incompatible":
		return &IncompatibleError{Reason: reason}
	case "":
		return errors.New("the adapter exited before it was ready")
	case "too-long":
		return fmt.Errorf("the adapter's first line is longer than %d KiB", maxLine>>10)
	}
	return fmt.Errorf("the adapter answered %q", line)
}

// Done is closed once the adapter has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// Close stops the adapter and waits for it to exit: it closes the adapter's
// stdin, which asks it to stop, and kills the adapter and every process it
// started when it has not exited within StopGrace.
func (p *Process) Close() error {
	p.stop.Do(func() { _ = p.stdin.Close() })
	grace := time.NewTimer(StopGrace)
	defer grace.Stop()
	select {
	case <-p.done:
		return nil
	case <-grace.C:
	}
	if proc, err := os.FindProcess(p.pid); err == nil {
		killGroup(proc)
	}
	<-p.done
	return nil
}

// socketRoot is where adapter sockets go: the temporary directory, unless
// its path leaves too little of the 104 bytes a Unix socket path may use.
func socketRoot() string {
	dir := os.TempDir()
	if runtime.GOOS != "windows" && len(dir) > 60 {
		return "/tmp"
	}
	return dir
}
