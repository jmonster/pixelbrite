package adapter

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
)

// Main runs an adapter serving what newAdapter makes for its data folder,
// following the handshake in README.md, and exits when the server closes its
// stdin. An adapter that is an io.Closer is closed first.
func Main(newAdapter func(dir string) Adapter) {
	if err := run(newAdapter, Supported(), os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(newAdapter func(dir string) Adapter, ours map[string]Versions, args []string, stdin io.Reader, stdout io.Writer) error {
	flags := flag.NewFlagSet("adapter", flag.ContinueOnError)
	listen := flags.String("listen", "", "Unix socket to serve")
	data := flags.String("data", "", "the adapter's data folder")
	offered := flags.String("capabilities", "", "the version of each capability the server chose, name=version,...")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *listen == "" || *data == "" || *offered == "" {
		return errors.New("usage: PROGRAM -listen SOCKET -data DIR -capabilities NAME=VERSION,...")
	}
	chosen, err := ParseCapabilities(*offered)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*data, 0o700); err != nil {
		return err
	}
	a := newAdapter(*data)
	if c, ok := a.(io.Closer); ok {
		defer c.Close()
	}
	if reason := unserved(a, chosen, ours); reason != "" {
		_, _ = fmt.Fprintf(stdout, "incompatible %s\n", reason)
		return &IncompatibleError{Reason: reason}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, stdin)
		cancel()
	}()

	_ = os.Remove(*listen)
	l, err := net.Listen("unix", *listen)
	if err != nil {
		return err
	}
	defer os.Remove(*listen)
	if _, err := fmt.Fprintln(stdout, "ready"); err != nil {
		return err
	}
	return Serve(ctx, l, a, chosen)
}

// unserved says which chosen capability a cannot serve at its version, or
// returns "" when it serves them all.
func unserved(a Adapter, chosen map[string]int, ours map[string]Versions) string {
	for _, name := range sortedKeys(chosen) {
		if !ours[name].Includes(chosen[name]) {
			return fmt.Sprintf("the server chose %s %d and the adapter speaks %s", name, chosen[name], ours[name])
		}
		var serves bool
		switch name {
		case CapStatus:
			serves = true
		case CapSearch:
			_, serves = a.(Searcher)
		case CapBrowse:
			_, serves = a.(Browser)
		case CapLyrics:
			_, serves = a.(LyricsSource)
		case CapContinuation:
			_, serves = a.(Continuer)
		case CapLive:
			_, serves = a.(LivePlayer)
		}
		if !serves {
			return "the adapter does not serve " + name
		}
	}
	if _, ok := chosen[CapStatus]; !ok {
		return "the server chose no version of " + CapStatus
	}
	return ""
}
