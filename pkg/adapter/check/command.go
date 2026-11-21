package check

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"pixelbrite.site/adapter"
)

const usageTemplate = `usage: PROGRAM spec
       PROGRAM check [-query TEXT] [-setup STEP=VALUE]... [-audio DURATION] [-data DIR] [-quiet] PATH

spec prints the adapter spec this program implements.

check runs the adapter folder or package at PATH, calls every capability its
adapter.json declares, and reports every problem. -setup values complete setup
steps first: owner steps for the server, member steps for the check's member.
-data keeps the adapter's data in DIR between checks: the member stays set up,
done steps are not redone, and the member is neither reset nor forgotten, so a
check against a real account is the same member in the same browser each run.
It exits 1 if anything fails.`

// setupValues collects repeated -setup STEP=VALUE flags.
type setupValues map[string]string

func (s setupValues) String() string { return "" }

func (s setupValues) Set(v string) error {
	step, value, ok := strings.Cut(v, "=")
	if !ok || step == "" {
		return fmt.Errorf("-setup takes STEP=VALUE, not %q", v)
	}
	s[step] = value
	return nil
}

// Command runs the spec or check subcommand in args for the program named
// program and returns its exit status.
func Command(program string, args []string, stdout, stderr io.Writer) int {
	usage := strings.ReplaceAll(usageTemplate, "PROGRAM", program)
	if len(args) == 1 && args[0] == "spec" {
		fmt.Fprint(stdout, adapter.Spec)
		return 0
	}
	if len(args) < 1 || args[0] != "check" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	flags := flag.NewFlagSet("adapter check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprintln(stderr, usage) }
	setup := setupValues{}
	query := flags.String("query", "", "what the search check asks for")
	audio := flags.Duration("audio", 3*time.Second, "how much live audio the play check reads")
	quiet := flags.Bool("quiet", false, "leave out the adapter's own log")
	data := flags.String("data", "", "keep the adapter's data in this folder between checks")
	flags.Var(setup, "setup", "complete a setup step first, STEP=VALUE (repeatable)")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	opts := Options{Query: *query, Setup: setup, Audio: *audio, Stderr: stderr, Data: *data}
	if *quiet {
		opts.Stderr = nil
	}
	checks := 0
	failures := Run(ctx, flags.Arg(0), opts, func(f Finding) {
		checks++
		fmt.Fprintf(stdout, "%-4s  %-14s %s\n", f.Outcome, f.Check, f.Message)
	})
	fmt.Fprintf(stdout, "%d findings, %d failed\n", checks, failures)
	if failures > 0 {
		return 1
	}
	return 0
}
