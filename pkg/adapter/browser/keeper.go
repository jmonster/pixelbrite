package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Keeper keeps one Chrome open for a profile folder between uses: it starts
// Chrome when asked, prepares the page with the open function the adapter
// gives it, and closes Chrome once it has gone Idle without a use. An
// adapter keeps one Keeper per account.
//
// The profile is the account's one browser, as a person keeps theirs: it is
// seeded with the account's sign-in only when it is new or after Reseed, and
// otherwise keeps the cookies the service rotates in it. Only Reset, for a
// member signing out or being forgotten, deletes it.
type Keeper struct {
	Profile string
	Idle    time.Duration
	// Use is held by callers for one exclusive use at a time, such as one
	// song. Uses that share the page with it, such as a script that reads
	// the page, call Ready and Rest without holding Use.
	Use sync.Mutex

	start sync.Mutex // held while Chrome starts, so one runs per profile
	mu    sync.Mutex
	b     *Browser
	uses  int // Ready calls not yet matched by Rest
	timer *time.Timer
}

// reseedFile, in the profile folder, asks the next start to seed it.
const reseedFile = "pixelbrite-reseed"

// Ready returns the running Chrome, starting it and calling open when none
// runs; open seeds the account's sign-in into the page when seed is set,
// which it is for a new profile or after Reseed until an open succeeds. A
// failed open leaves the profile as it is. Ready holds off the idle close
// until every Ready has its Rest.
func (k *Keeper) Ready(ctx context.Context, open func(ctx context.Context, b *Browser, seed bool) error) (*Browser, error) {
	k.start.Lock()
	defer k.start.Unlock()
	k.mu.Lock()
	if k.timer != nil {
		k.timer.Stop()
		k.timer = nil
	}
	b := k.b
	if b != nil && b.Alive() {
		k.uses++
		k.mu.Unlock()
		return b, nil
	}
	k.mu.Unlock()
	seed := k.seeding()
	b, err := Launch(ctx, Chrome(), k.Profile)
	if err != nil {
		return nil, err
	}
	if err := open(ctx, b, seed); err != nil {
		b.Close()
		return nil, err
	}
	if seed {
		_ = os.Remove(filepath.Join(k.Profile, reseedFile))
	}
	k.mu.Lock()
	k.b = b
	k.uses++
	k.mu.Unlock()
	return b, nil
}

// Rest ends one use; Chrome closes once it has gone Idle without a use.
func (k *Keeper) Rest() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.uses > 0 {
		k.uses--
	}
	b := k.b
	if b == nil || k.uses > 0 {
		return
	}
	if k.timer != nil {
		k.timer.Stop()
	}
	k.timer = time.AfterFunc(k.Idle, func() {
		k.mu.Lock()
		if k.b != b {
			k.mu.Unlock()
			return
		}
		k.b, k.timer = nil, nil
		k.mu.Unlock()
		b.Close()
	})
}

// Stop closes Chrome and waits for it to exit.
func (k *Keeper) Stop() {
	k.mu.Lock()
	b := k.b
	k.b = nil
	if k.timer != nil {
		k.timer.Stop()
		k.timer = nil
	}
	k.mu.Unlock()
	b.Close()
}

// seeding reports whether the next start seeds the profile: it is new, or
// Reseed asked for it.
func (k *Keeper) seeding() bool {
	if _, err := os.Stat(k.Profile); errors.Is(err, os.ErrNotExist) {
		return true
	}
	_, err := os.Stat(filepath.Join(k.Profile, reseedFile))
	return err == nil
}

// Reseed closes Chrome and has its next start seed the profile with the
// account's sign-in, as after the member signs in again; the profile and
// everything else the service keeps in it stay.
func (k *Keeper) Reseed() error {
	k.Stop()
	if err := os.MkdirAll(k.Profile, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(k.Profile, reseedFile), nil, 0o600)
}

// Reset closes Chrome and deletes its profile, for a member who signs out
// or is forgotten.
func (k *Keeper) Reset() error {
	k.Stop()
	return os.RemoveAll(k.Profile)
}
