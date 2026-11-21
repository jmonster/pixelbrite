//go:build darwin

package browser

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -mmacosx-version-min=14.2
#cgo LDFLAGS: -framework CoreAudio -framework Foundation
#include "tap_darwin.h"
*/
import "C"

import (
	"context"
	"fmt"
	"syscall"
	"time"
	"unsafe"

	"pixelbrite.site/adapter"
)

// Unavailable says why this computer cannot capture Chrome, or "": macOS
// process taps arrived in macOS 14.2.
func Unavailable() string {
	release, _ := syscall.Sysctl("kern.osproductversion")
	var major, minor int
	_, _ = fmt.Sscanf(release, "%d.%d", &major, &minor)
	if major > 14 || major == 14 && minor >= 2 {
		return ""
	}
	return "Capturing Google Chrome's sound needs macOS 14.2 or later."
}

// Capture captures Chrome's sound, muted, from the moment it plays, into out
// as float32 stereo frames until ctx ends or finished closes and the last of
// its sound is drained. It returns ErrNoSound when finished closes before
// Chrome played. Sound lost because out read too slowly fails the capture.
func (b *Browser) Capture(ctx context.Context, finished <-chan struct{}, out adapter.PCM) error {
	return tapProcess(ctx, b.cmd.Process.Pid, either(finished, b.done), out)
}

// tapProcess captures the audio of parent's child processes, muted, from the
// moment one of them plays, into out as float32 stereo frames until ctx ends
// or finished closes (the track ended, or Chrome quit) and the last of its
// audio is drained. Audio lost because out read too slowly
// fails the track once it has played.
func tapProcess(ctx context.Context, parent int, finished <-chan struct{}, out adapter.PCM) error {
	var handle unsafe.Pointer
	var status C.int
	for handle == nil {
		if handle = C.pb_tap_open(C.int(parent), &status); handle != nil {
			break
		}
		if status != 0 {
			return fmt.Errorf("macOS refused to capture Google Chrome's sound (Core Audio error %d). Allow Pixel Brite to capture system audio in System Settings › Privacy & Security.", int(status))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-finished:
			return ErrNoSound
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer C.pb_tap_close(handle)
	rate := float64(C.pb_tap_rate(handle))
	out.SetRate(rate)

	samples := make([]float32, 4800*2)
	bytes := unsafe.Slice((*byte)(unsafe.Pointer(&samples[0])), len(samples)*4)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	done := false
	for {
		for {
			n := int(C.pb_tap_read(handle, (*C.float)(unsafe.Pointer(&samples[0])), C.int(len(samples))))
			if n == 0 {
				break
			}
			if _, err := out.Write(bytes[:n*4]); err != nil {
				return err
			}
		}
		if done {
			if lost := uint64(C.pb_tap_dropped(handle)); lost > 0 {
				return fmt.Errorf("Chrome's sound skipped %.0f ms because the server read it too slowly", float64(lost)/2/rate*1000)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-finished:
			done = true // drain what the ring still holds, then end
		case <-tick.C:
		}
	}
}
