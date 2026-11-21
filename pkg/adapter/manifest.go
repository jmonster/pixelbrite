package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// ManifestName is the file in an adapter's folder that describes it.
const ManifestName = "adapter.json"

// Live audio paces.
const (
	// PaceReader produces audio as fast as the server reads it.
	PaceReader = "reader"
	// PaceRealtime produces audio at playing speed whatever the server does.
	PaceRealtime = "realtime"
)

// Manifest is adapter.json: what an adapter is, read without running it.
type Manifest struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	// Capabilities maps each capability served to the versions it speaks.
	Capabilities map[string]Versions `json:"capabilities"`
	// Live describes the live audio of an adapter serving CapLive.
	Live *Live `json:"live,omitempty"`
	// Programs maps each supported platform, such as darwin-arm64, to its
	// program's slash-separated path inside the adapter's folder.
	Programs map[string]string `json:"programs"`
	// Requires names programs the server provides to the adapter, such as
	// RequireChrome. See Provided.
	Requires []string `json:"requires,omitempty"`
}

// RequireChrome is Google Chrome, which the server installs once for every
// adapter that requires it.
const RequireChrome = "chrome"

// RequirementEnv is the environment variable that carries the path of the
// provided program name to the adapter: PIXELBRITE_ and the name in capitals,
// hyphens as underscores. It is unset while the program is not installed.
func RequirementEnv(name string) string {
	return "PIXELBRITE_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// Live is what the server needs to know to play an adapter's live audio.
type Live struct {
	Pace string `json:"pace"`
	// Streams is how many tracks the adapter plays at once for one member;
	// zero means no limit.
	Streams int  `json:"streams"`
	Seek    bool `json:"seek"`
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ReservedID names the server's own music among the sources features use;
// no adapter may take it.
const ReservedID = "library"

// ValidID reports whether id can name an adapter: lowercase letters, digits
// and hyphens, starting with a letter or digit, and not ReservedID.
func ValidID(id string) bool { return idPattern.MatchString(id) && id != ReservedID }

// Platform is the server's platform as manifests name it: GOOS-GOARCH.
func Platform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// ReadManifest reads and checks dir's adapter.json.
func ReadManifest(dir string) (Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", ManifestName, err)
	}
	return m, m.Validate()
}

// Validate reports every way m breaks the manifest rules.
func (m Manifest) Validate() error {
	var errs []error
	switch {
	case m.ID == ReservedID:
		errs = append(errs, fmt.Errorf("id %q is reserved for the server's own music", m.ID))
	case !ValidID(m.ID):
		errs = append(errs, fmt.Errorf("id %q is not lowercase letters, digits and hyphens", m.ID))
	}
	if strings.TrimSpace(m.Name) == "" {
		errs = append(errs, errors.New("name is empty"))
	}
	if strings.TrimSpace(m.Version) == "" {
		errs = append(errs, errors.New("version is empty"))
	}
	if _, ok := m.Capabilities[CapStatus]; !ok {
		errs = append(errs, fmt.Errorf("capabilities lack %q, which every adapter serves", CapStatus))
	}
	_, live := m.Capabilities[CapLive]
	switch {
	case live && m.Live == nil:
		errs = append(errs, fmt.Errorf("capability %q needs a live section", CapLive))
	case !live && m.Live != nil:
		errs = append(errs, fmt.Errorf("a live section without the %q capability", CapLive))
	case live && m.Live.Pace != PaceReader && m.Live.Pace != PaceRealtime:
		errs = append(errs, fmt.Errorf("live pace %q is neither %q nor %q", m.Live.Pace, PaceReader, PaceRealtime))
	case live && m.Live.Streams < 0:
		errs = append(errs, errors.New("live streams is negative"))
	}
	if len(m.Programs) == 0 {
		errs = append(errs, errors.New("programs is empty"))
	}
	seen := map[string]bool{}
	for _, name := range m.Requires {
		switch {
		case !idPattern.MatchString(name):
			errs = append(errs, fmt.Errorf("requirement %q is not lowercase letters, digits and hyphens", name))
		case seen[name]:
			errs = append(errs, fmt.Errorf("requirement %q is listed twice", name))
		}
		seen[name] = true
	}
	for _, platform := range sortedKeys(m.Programs) {
		if _, err := programPath(m.Programs[platform]); err != nil {
			errs = append(errs, fmt.Errorf("program for %s: %w", platform, err))
		}
	}
	return errors.Join(errs...)
}

// Program returns the path of the program for this platform inside dir, or
// an error saying the adapter has none.
func (m Manifest) Program(dir string) (string, error) {
	rel, ok := m.Programs[Platform()]
	if !ok {
		return "", fmt.Errorf("%s has no program for %s", m.Name, Platform())
	}
	clean, err := programPath(rel)
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, filepath.FromSlash(clean))
	info, err := os.Stat(p)
	switch {
	case err != nil:
		return "", fmt.Errorf("%s's program for %s is missing", m.Name, Platform())
	case !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode()&0o111 == 0):
		return "", fmt.Errorf("%s's program for %s is not executable", m.Name, Platform())
	}
	return p, nil
}

func programPath(rel string) (string, error) {
	clean := path.Clean(rel)
	if rel == "" || path.IsAbs(rel) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(rel, `\`) {
		return "", fmt.Errorf("%q is not a relative path inside the adapter's folder", rel)
	}
	return clean, nil
}

// Versions is an inclusive range of a capability's versions, written "1"
// or "1-3".
type Versions struct{ Min, Max int }

// ParseVersions reads "MIN-MAX", or "V" for a single version.
func ParseVersions(s string) (Versions, error) {
	lo, hi, ranged := strings.Cut(s, "-")
	if !ranged {
		hi = lo
	}
	min, err1 := strconv.Atoi(lo)
	max, err2 := strconv.Atoi(hi)
	if err1 != nil || err2 != nil || min < 1 || max < min {
		return Versions{}, fmt.Errorf("%q is not a version range", s)
	}
	return Versions{min, max}, nil
}

func (v Versions) String() string {
	if v.Min == v.Max {
		return strconv.Itoa(v.Min)
	}
	return strconv.Itoa(v.Min) + "-" + strconv.Itoa(v.Max)
}

func (v Versions) MarshalJSON() ([]byte, error) { return json.Marshal(v.String()) }

func (v *Versions) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n int
		if json.Unmarshal(b, &n) != nil {
			return fmt.Errorf("version range %s is neither a string nor a number", b)
		}
		s = strconv.Itoa(n)
	}
	parsed, err := ParseVersions(s)
	*v = parsed
	return err
}

// Includes reports whether n is in the range.
func (v Versions) Includes(n int) bool { return v.Min <= n && n <= v.Max }

// Negotiate returns the newest version both ranges include.
func (v Versions) Negotiate(w Versions) (int, bool) {
	n := min(v.Max, w.Max)
	return n, n >= max(v.Min, w.Min)
}

// Supported is the range of each capability this package speaks. A change
// an existing adapter or server could not handle adds a version; new
// optional fields and new step kinds do not.
func Supported() map[string]Versions {
	return map[string]Versions{
		CapStatus:       {1, 1},
		CapSearch:       {1, 1},
		CapBrowse:       {1, 1},
		CapLyrics:       {1, 1},
		CapContinuation: {1, 1},
		CapLive:         {1, 1},
	}
}

// Negotiate picks, for each capability the manifest declares and ours
// speaks, the newest version both speak. Skipped lists the declared
// capabilities with no version in common. An adapter whose status has none
// cannot run.
func Negotiate(declared, ours map[string]Versions) (chosen map[string]int, skipped []string, err error) {
	chosen = map[string]int{}
	for _, name := range sortedKeys(declared) {
		if n, ok := ours[name].Negotiate(declared[name]); ok && ours[name].Min > 0 {
			chosen[name] = n
		} else {
			skipped = append(skipped, name)
		}
	}
	if _, ok := chosen[CapStatus]; !ok {
		return nil, skipped, &IncompatibleError{Reason: fmt.Sprintf("the adapter speaks %s %s and the server %s", CapStatus, declared[CapStatus], ours[CapStatus])}
	}
	return chosen, skipped, nil
}

// IncompatibleError is an adapter and server that cannot work together.
type IncompatibleError struct{ Reason string }

func (e *IncompatibleError) Error() string { return "incompatible: " + e.Reason }

// FormatCapabilities writes chosen versions as the -capabilities argument:
// name=version pairs separated by commas.
func FormatCapabilities(chosen map[string]int) string {
	parts := make([]string, 0, len(chosen))
	for _, name := range sortedKeys(chosen) {
		parts = append(parts, name+"="+strconv.Itoa(chosen[name]))
	}
	return strings.Join(parts, ",")
}

// ParseCapabilities reads the -capabilities argument.
func ParseCapabilities(s string) (map[string]int, error) {
	chosen := map[string]int{}
	for _, part := range strings.Split(s, ",") {
		name, version, ok := strings.Cut(part, "=")
		n, err := strconv.Atoi(version)
		if !ok || name == "" || err != nil || n < 1 {
			return nil, fmt.Errorf("%q is not a list of capability=version", s)
		}
		chosen[name] = n
	}
	return chosen, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
