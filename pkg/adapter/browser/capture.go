package browser

import (
	"context"
	"errors"
	"fmt"
	"time"

	"pixelbrite.site/adapter"
)

// stopLimit bounds stopping the page's playback after a failure.
const stopLimit = 5 * time.Second

// CaptureWhile runs play, which plays one item in b and closes ended the
// moment it is over, while capturing b's sound into out. The capture starts
// first so it catches the item's first sound, and a failed capture stops
// play. After a failure stop is called to stop the page's playback.
func CaptureWhile(ctx context.Context, b *Browser, out adapter.PCM,
	play func(ctx context.Context, ended chan<- struct{}) error, stop func(context.Context)) error {
	playing, cancel := context.WithCancel(ctx)
	defer cancel()
	ended := make(chan struct{})
	captured := make(chan error, 1)
	go func() {
		err := b.Capture(playing, ended, out)
		if err != nil && !errors.Is(err, ErrNoSound) {
			cancel()
		}
		captured <- err
	}()
	played := play(playing, ended)
	capErr := <-captured
	if played != nil || capErr != nil {
		quiet, done := context.WithTimeout(context.Background(), stopLimit)
		stop(quiet)
		done()
	}
	if capErr != nil && errors.Is(played, context.Canceled) && ctx.Err() == nil {
		return fmt.Errorf("capturing Chrome's sound failed: %w", capErr)
	}
	if played != nil {
		return played
	}
	return capErr
}
