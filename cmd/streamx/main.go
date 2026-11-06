// Command streamx is a terminal client for gRPC and streaming APIs.
//
//	streamx [dir] [--env NAME]          open the TUI on a collection
//	streamx run <path|glob>... [flags]  run requests headless, exit non-zero on failure
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"

	"github.com/abdussalamfaqih/streamx/internal/core"
	_ "github.com/abdussalamfaqih/streamx/internal/engine/grpc" // engines register themselves
	_ "github.com/abdussalamfaqih/streamx/internal/engine/http"
	_ "github.com/abdussalamfaqih/streamx/internal/engine/ws"
	"github.com/abdussalamfaqih/streamx/internal/runner"
	"github.com/abdussalamfaqih/streamx/internal/tui"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// vars collects repeated --var name=value flags.
type vars map[string]string

func (v vars) String() string { return "" }
func (v vars) Set(s string) error {
	k, val, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("want name=value, got %q", s)
	}
	v[k] = val
	return nil
}

// version is set by the release build (-ldflags "-X main.version=v1.2.3").
var version = ""

func versionString() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version // go install …@v1.2.3
	}
	return "dev"
}

const usage = `usage: streamx [collection-dir] [--env NAME]
       streamx run <path|glob>... [--env NAME] [--var name=value]... [-v] [--transcript file]
       streamx version`

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, usage)
		fs.PrintDefaults()
	}
	return fs
}

// parseArgs allows flags before, between and after positional args
// (streamx run dir --env local), returning the positional ones.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
}

const envFlag = "environment name (environments/<name>.yml)"

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, "streamx", versionString())
		return runner.ExitOK
	case "run":
		return runCmd(ctx, args[1:], stdout, stderr)
	}
	return tuiCmd(args, stderr)
}

// runCmd is `streamx run`: run request files headless.
func runCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("run", stderr)
	o := runner.Options{Vars: vars{}, Out: stdout}
	fs.StringVar(&o.Env, "env", "", envFlag)
	fs.Var(vars(o.Vars), "var", "set a variable, name=value (repeatable)")
	fs.BoolVar(&o.Verbose, "v", false, "print every event")
	transcript := fs.String("transcript", "", "write every event as JSON lines to `file` (secrets masked; overwrites)")
	paths, err := parseArgs(fs, args)
	if err != nil {
		return runner.ExitConfig
	}
	if len(paths) == 0 {
		fs.Usage()
		return runner.ExitConfig
	}
	if *transcript == "" {
		return runner.Run(ctx, paths, o)
	}
	f, err := core.CreateTranscript(*transcript, true)
	if err != nil {
		fmt.Fprintln(stderr, "streamx:", err)
		return runner.ExitConfig
	}
	o.Transcript = f
	code := runner.Run(ctx, paths, o)
	if err := f.Close(); err != nil {
		fmt.Fprintln(stderr, "streamx: transcript:", err)
		code = max(code, runner.ExitConfig)
	}
	return code
}

// tuiCmd is `streamx [dir]`: open the TUI.
func tuiCmd(args []string, stderr io.Writer) int {
	fs := newFlags("streamx", stderr)
	o := tui.Options{}
	fs.StringVar(&o.Env, "env", "", envFlag)
	dirs, err := parseArgs(fs, args)
	if err != nil {
		return runner.ExitConfig
	}
	if len(dirs) > 1 {
		fs.Usage()
		return runner.ExitConfig
	}
	if len(dirs) == 1 {
		o.Dir = dirs[0]
	}
	if err := tui.Run(o); err != nil {
		fmt.Fprintln(stderr, "streamx:", err)
		return runner.ExitConfig
	}
	return runner.ExitOK
}
