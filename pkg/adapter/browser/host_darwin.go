package browser

import "context"

// host is what Chrome plays on here: macOS gives it a window and the real
// audio device, which the tap mutes while capturing.
type host struct{}

func startHost(context.Context, string) (*host, error) { return &host{}, nil }
func (*host) args() []string                           { return nil }
func (*host) env() []string                            { return nil }
func (*host) windowed() bool                           { return true }
func (*host) stop()                                    {}
