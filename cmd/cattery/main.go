// Command cattery is the whole of cattery: an installer, the agent-state
// writer, and the agent picker.
//
//	cattery                  the picker, a full-screen list kitty runs in an
//	                         overlay window
//	cattery -print           the same inventory without the TUI
//	cattery -version         print the version and exit
//	cattery setup            install the kitty files and the config they need
//	cattery state <x>        publish this window's agent state
//	cattery save [path]      snapshot the kitty tab tree
//	cattery restore [path]   put a snapshot back
//	cattery attach <target>  watch a tmux agent read-only
//
// The picker shows every kitty window carrying AGENT_DISPLAY. The watcher
// derives that variable from the agent's AGENT_STATE, so a window that sets
// AGENT_STATE without a watcher loaded does not appear. It also shows every
// tmux pane carrying @AGENT_STATE, where there is no watcher and the display
// state is derived at read time instead. `cattery setup` binds the picker in
// kitty.conf as:
//
//	map opt+a>opt+a launch --type=overlay --cwd=current --copy-colors /path/to/cattery
//
// That overlay is a normal kitty window, so the picker inherits KITTY_LISTEN_ON
// and drives the same kitty instance over remote control.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/alexander-akhmetov/cattery/internal/agent"
	"github.com/alexander-akhmetov/cattery/internal/agents"
	"github.com/alexander-akhmetov/cattery/internal/kitty"
	"github.com/alexander-akhmetov/cattery/internal/overlay"
	"github.com/alexander-akhmetov/cattery/internal/session"
	"github.com/alexander-akhmetov/cattery/internal/setup"
	"github.com/alexander-akhmetov/cattery/internal/state"
	"github.com/alexander-akhmetov/cattery/internal/tmux"
)

// The names route returns. Plain strings, so the switch that runs them need not
// be exhaustive.
const (
	cmdPicker  = "picker"
	cmdPrint   = "print"
	cmdSetup   = "setup"
	cmdState   = "state"
	cmdSave    = "save"
	cmdRestore = "restore"
	cmdAttach  = "attach"
	cmdVersion = "version"
)

// version is the release this binary was built from. `make build` passes the
// output of `git describe`, goreleaser passes the tag, and the Homebrew formula
// passes the version it installed. None of them runs for `go install`, which
// records the module version in the build info instead.
var version string

func versionString() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "dev"
}

// command is what one argv asks for. Routing is separate from running, so a
// test can check it without launching the TUI or writing to a kitty window.
type command struct {
	name string
	args []string
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cmd, err := route(args, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "cattery:", err)
		return 2
	}

	switch cmd.name {
	case cmdVersion:
		fmt.Println(versionString())
		return 0
	case cmdState:
		// Never fails: a hook error would surface in the agent's transcript.
		state.Run(cmd.args)
		return 0
	case cmdSetup:
		return runSetup(cmd.args)
	case cmdSave:
		// Snapshots are kitty's tab tree. A tmux agent belongs to whatever
		// started it, and restoring one would fork work that is still running.
		return runSave(kitty.NewClient(), os.Stdout, cmd.args)
	case cmdRestore:
		return runRestore(kitty.NewClient(), os.Stdout, cmd.args)
	case cmdAttach:
		return runAttach(tmux.NewClient(), cmd.args)
	case cmdPrint:
		if err := printAgents(agents.NewClient(kitty.NewClient()), os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "cattery:", err)
			return 1
		}
		return 0
	default:
		snapshots := kitty.NewClient()
		program := tea.NewProgram(overlay.New(agents.NewClient(snapshots), snapshots), tea.WithAltScreen())
		if _, err := program.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "cattery:", err)
			return 1
		}
		return 0
	}
}

// route reads the argv. A first argument that does not start with "-" names a
// subcommand; everything else is parsed as flags for the picker.
func route(args []string, out io.Writer) (command, error) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case cmdSetup, cmdState, cmdSave, cmdRestore, cmdAttach:
			return command{name: args[0], args: args[1:]}, nil
		default:
			return command{}, fmt.Errorf("unknown command %q (try setup, state, save, restore, attach, or no argument for the picker)", args[0])
		}
	}

	flags := flag.NewFlagSet("cattery", flag.ContinueOnError)
	flags.SetOutput(out)
	printOnly := flags.Bool("print", false, "list agent windows and exit (no TUI); useful for debugging")
	showVersion := flags.Bool("version", false, "print the version and exit")
	if err := flags.Parse(args); err != nil {
		return command{}, err
	}
	switch {
	case *showVersion:
		return command{name: cmdVersion}, nil
	case *printOnly:
		return command{name: cmdPrint}, nil
	default:
		return command{name: cmdPicker}, nil
	}
}

func runSetup(args []string) int {
	flags := flag.NewFlagSet("cattery setup", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dryRun := flags.Bool("dry-run", false, "report every action without changing anything")
	yes := flags.Bool("yes", false, "answer yes to the Claude Code and pi questions")
	kittyDir := flags.String("kitty-dir", "", "kitty config directory (default $KITTY_CONFIG_DIRECTORY, else ~/.config/kitty)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	opts := setup.Options{DryRun: *dryRun, Yes: *yes, KittyDir: *kittyDir}
	// Only a terminal can answer a question. Behind a pipe there is nobody to
	// ask, so setup skips the agent steps instead of guessing.
	if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		opts.In = os.Stdin
	}
	if err := setup.Run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "cattery:", err)
		return 1
	}
	return 0
}

// Save runs one kitty action. Restore adds the wait for restored windows to
// draw a prompt, plus one send-text per resumable agent.
const (
	saveTimeout    = 15 * time.Second
	restoreTimeout = 60 * time.Second
)

func runSave(client session.Client, out io.Writer, args []string) int {
	flags := flag.NewFlagSet("cattery save", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	path, err := parseSessionArgs(flags, args)
	if err != nil {
		return flagExit(err)
	}
	if err := session.EnsureDir(path); err != nil {
		fmt.Fprintln(os.Stderr, "cattery:", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), saveTimeout)
	defer cancel()
	stats, err := session.Save(ctx, client, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cattery:", err)
		return 1
	}
	fmt.Fprintln(out, stats.Saved(path))
	return 0
}

func runRestore(client session.Client, out io.Writer, args []string) int {
	flags := flag.NewFlagSet("cattery restore", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	// Off by default. Resuming every agent starts one process each, and a
	// resume command can point at a session that no longer exists.
	run := flags.Bool("run", false, "press return on each resume command instead of leaving it at the prompt")
	path, err := parseSessionArgs(flags, args)
	if err != nil {
		return flagExit(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), restoreTimeout)
	defer cancel()
	stats, err := session.Restore(ctx, client, path, *run)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cattery:", err)
		return 1
	}
	fmt.Fprintln(out, stats.Restored(*run))
	return 0
}

// parseSessionArgs reads the flags and the optional snapshot path in either
// order, because Go's flag package stops at the first argument that is not a
// flag and `restore path -run` is natural to type.
//
// With no path the snapshot comes from CATTERY_SESSION_FILE, else the default
// under kitty's sessions directory. A given path is used as given, and made
// absolute: kitty resolves a relative path against directories of its own.
func parseSessionArgs(flags *flag.FlagSet, args []string) (string, error) {
	var positional []string
	for rest := args; ; {
		if err := flags.Parse(rest); err != nil {
			return "", err
		}
		rest = flags.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
	switch len(positional) {
	case 0:
		path, err := session.DefaultPath()
		if err != nil {
			return "", fmt.Errorf("%w: %w", errBadPath, err)
		}
		return path, nil
	case 1:
		path, err := session.Abs(positional[0])
		if err != nil {
			return "", fmt.Errorf("%w: %w", errBadPath, err)
		}
		return path, nil
	default:
		return "", fmt.Errorf("%w: one snapshot path at most, got %d", errBadPath, len(positional))
	}
}

// errBadPath marks the argument problems this file finds itself. The flag
// package has already written its own to stderr.
var errBadPath = errors.New("bad snapshot path")

// flagExit turns a bad command line into an exit status. Asking for the usage
// text and getting it is a success, and the parser has already explained any
// flag it rejected.
func flagExit(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if errors.Is(err, errBadPath) {
		fmt.Fprintln(os.Stderr, "cattery:", err)
	}
	return 2
}

// lister is the inventory `-print` reads, an interface so a test can print
// without a kitty or a tmux server.
type lister interface {
	ListAgents(ctx context.Context) ([]agent.Agent, error)
}

// printAgents writes the inventory as one line per agent. A tmux row carries
// the target `cattery attach` takes; a kitty row is reached by window id and
// has none.
//
// A host that failed still returns the other's rows, so they are printed before
// the error goes back.
func printAgents(client lister, out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	agents, err := client.ListAgents(ctx)
	for _, a := range agents {
		target := ""
		if a.Target != "" {
			target = " target=" + a.Target
		}
		fmt.Fprintf(out, "%-16s %-7s %-7s host=%-5s id=%-5d %-24s %s%s\n",
			a.Project, a.Display, a.Kind, a.Host, a.ID, a.Branch, a.CWD, target)
	}
	return err
}

// runAttach opens a read-only view of one tmux agent, and returns when the
// viewer detaches. It runs without a deadline: the view stays for as long as
// the user watches it.
func runAttach(client *tmux.Client, args []string) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(os.Stderr, "cattery: attach takes one <session>:<window>.<pane id> target")
		return 2
	}
	if err := client.Attach(context.Background(), args[0]); err != nil {
		fmt.Fprintln(os.Stderr, "cattery:", err)
		return 1
	}
	return 0
}
