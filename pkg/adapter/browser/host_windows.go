package browser

import (
	"context"
	"errors"

	"pixelbrite.site/adapter"
)

// host is what Chrome plays on here: Windows gives it a window.
type host struct{}

func startHost(context.Context, string) (*host, error) { return &host{}, nil }
func (*host) args() []string                           { return nil }
func (*host) env() []string                            { return nil }
func (*host) windowed() bool                           { return true }
func (*host) stop()                                    {}

// Unavailable says why this computer cannot capture Chrome, or "".
func Unavailable() string {
	return "Capturing Google Chrome's sound on Windows is not built yet."
}

// Capture fails on Windows until per-process loopback capture is built.
func (b *Browser) Capture(context.Context, <-chan struct{}, adapter.PCM) error {
	return errors.New(Unavailable())
}
