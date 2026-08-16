// Command cattery is the whole of cattery: an installer, the agent-state
// writer, and the agent picker.
//
//	cattery                  the picker, a full-screen list kitty runs in an
//	                         overlay window
//	cattery -print           the same inventory without the TUI
//	cattery -version         print the version and exit
//	cattery help [command]   the command list, or one command
//	cattery setup            install the kitty files and the config they need
//	cattery state <x>        publish this window's agent state
//	cattery save [path]      snapshot the kitty tab tree
//	cattery restore [path]   put a snapshot back
//	cattery attach <target>  watch a tmux agent read-only
//	cattery events           print agent state transitions as JSON lines
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
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/alexander-akhmetov/cattery"
	"github.com/alexander-akhmetov/cattery/internal/agent"
	"github.com/alexander-akhmetov/cattery/internal/agents"
	"github.com/alexander-akhmetov/cattery/internal/events"
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
	cmdEvents  = "events"
	cmdVersion = "version"
	cmdHelp    = "help"
)

// subcommand is one word the command line accepts. The same entry answers
// route and the help text, so a command cannot be listed in one and missing
// from the other.
type subcommand struct {
	name string
	// operands is the rest of the usage line, empty for a command that takes
	// none. Flags are not in here: they come from commandFlags.
	operands string
	// summary is the one line the command list prints. details is the rest,
	// printed by `cattery help <command>` and wrapped where it is written.
	summary string
	details string
}

// subcommands is the whole command list, in the order the help prints it. A
// plain literal on purpose: a closure in here that reached back for a flag set
// would be an initialization cycle.
var subcommands = []subcommand{
	{
		name:    cmdSetup,
		summary: "install the kitty files and the config they need",
		details: `Copies the watcher and the kittens into kitty's config directory, keeps its
own block in kitty.conf, and offers to wire the Claude Code hooks and the pi
extension. It writes tab_bar.py only when that directory has none, because an
existing one is yours.

Those are copies, and upgrading the binary does not touch them, so run setup
again after every upgrade. Reload kitty afterwards.`,
	},
	{
		name:     cmdState,
		operands: "<working|blocked|idle|clear>",
		summary:  "publish this window's agent state",
		details: `Writes the AGENT_* variables that the tab marker and the picker read, on the
tmux pane when $TMUX and $TMUX_PANE are set and on the kitty window
otherwise.

Claude Code runs this from five hooks, and the shell wrappers run "clear"
after the agent exits. It is meant to be called by an agent rather than
typed, and it exits 0 whatever happens: a failure here would show up in the
agent's own transcript.`,
	},
	{
		name:     cmdSave,
		operands: "[path]",
		summary:  "snapshot the kitty tab tree",
		details: `Writes every kitty tab, its layout, and each agent's resume command to a
session file that restore can read back.

With no path the snapshot goes to $CATTERY_SESSION_FILE, else to the default
under kitty's sessions directory. tmux agents are not recorded: a pane
belongs to whatever started it.`,
	},
	{
		name:     cmdRestore,
		operands: "[path]",
		summary:  "put a snapshot back",
		details: `Opens the tabs the snapshot recorded and types each agent's resume command at
its prompt, leaving it there for you to read before it runs.

Reads the same default path as save. It refuses to run twice against one
snapshot, because kitty would build a second copy of every tab.`,
	},
	{
		name:     cmdAttach,
		operands: "<session>:<window>.<pane id>",
		summary:  "watch a tmux agent read-only",
		details: `Opens a read-only view of one tmux pane, for example

  cattery attach dev:3.%17

Keys do nothing and your terminal size does not resize the agent's pane.
"prefix d" detaches. The view is a tmux session of its own, grouped with the
agent's, so two viewers never fight over which window it shows.

The picker does this for you on Enter, and "cattery -print" prints the target
of every tmux agent.`,
	},
	{
		name:    cmdEvents,
		summary: "print agent state transitions as JSON lines",
		details: `Prints one JSON object per agent state change, until you interrupt it or the
pipe it writes to closes. Something other than cattery can then react to
them:

  cattery events | jq -r --unbuffered 'select(.to == "blocked") | .cwd'

The command binds a unix socket and registers it with the running kitty, so
it needs the kitten that "cattery setup" installs. Nothing is buffered: an
event that fires while nobody is subscribed is gone, and there is no replay.
tmux agents emit nothing, because no watcher runs there.

A kitty restart drops the subscription, and the command exits 3 when it
notices, for a supervisor to start it again.`,
	},
}

func lookup(name string) (subcommand, bool) {
	for _, sc := range subcommands {
		if sc.name == name {
			return sc, true
		}
	}
	return subcommand{}, false
}

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
	cmd, err := route(args)
	if err != nil {
		// -h reaches this only behind another flag; on its own it is a command.
		if errors.Is(err, flag.ErrHelp) {
			usage(os.Stdout)
			return 0
		}
		fmt.Fprintln(os.Stderr, "cattery:", err)
		fmt.Fprintln(os.Stderr, "run `cattery help` for the command list")
		return 2
	}

	switch cmd.name {
	case cmdVersion:
		fmt.Println(versionString())
		return 0
	case cmdHelp:
		// Help was asked for, so it is the output, not a diagnostic.
		return runHelp(os.Stdout, cmd.args)
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
	case cmdEvents:
		return runEvents(kitty.NewClient(), os.Stdout, cmd.args)
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
//
// Every way of asking for help routes to cmdHelp rather than to the flag
// package's own ErrHelp, so all of it prints to stdout and in one format.
// route itself prints nothing: a bad command line is an error the caller
// reports once.
func route(args []string) (command, error) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		if args[0] == cmdHelp {
			return helpCommand(args[1:])
		}
		sc, ok := lookup(args[0])
		if !ok {
			return command{}, fmt.Errorf("unknown command %q", args[0])
		}
		if rest := args[1:]; len(rest) == 1 && isHelpFlag(rest[0]) {
			return command{name: cmdHelp, args: []string{sc.name}}, nil
		}
		return command{name: sc.name, args: args[1:]}, nil
	}
	if len(args) > 0 && isHelpFlag(args[0]) {
		return command{name: cmdHelp}, nil
	}

	flags, printOnly, showVersion := pickerFlags()
	flags.SetOutput(io.Discard)
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

// --- help -----------------------------------------------------------------

func isHelpFlag(arg string) bool {
	return arg == "-h" || arg == "--help" || arg == "-help"
}

// helpCommand reads what `cattery help` was pointed at.
func helpCommand(args []string) (command, error) {
	switch len(args) {
	case 0:
		return command{name: cmdHelp}, nil
	case 1:
		if _, ok := lookup(args[0]); !ok {
			return command{}, fmt.Errorf("unknown command %q", args[0])
		}
		return command{name: cmdHelp, args: args}, nil
	default:
		return command{}, errors.New("help takes one command at most")
	}
}

func runHelp(out io.Writer, args []string) int {
	if len(args) == 0 {
		usage(out)
		return 0
	}
	// route only passes a name it found, so the lookup cannot fail here.
	sc, ok := lookup(args[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "cattery: unknown command %q\n", args[0])
		return 2
	}
	commandHelp(out, sc)
	return 0
}

// usage prints the whole command line: what cattery is, the commands, and the
// picker's own flags.
func usage(out io.Writer) {
	fmt.Fprint(out, `cattery marks each kitty tab with what its agent is doing, and lists every
agent in a picker.

Usage:
  cattery [flags]            open the picker
  cattery <command> [args]
  cattery help <command>     what one command does

Commands:
`)
	rows := make([][2]string, len(subcommands))
	for i, sc := range subcommands {
		rows[i] = [2]string{signature(sc), sc.summary}
	}
	columns(out, rows)

	flags, _, _ := pickerFlags()
	fmt.Fprint(out, "\nFlags:\n")
	columns(out, flagRows(flags))
}

// commandHelp prints one command's usage line, what it does, and its flags.
func commandHelp(out io.Writer, sc subcommand) {
	fmt.Fprintf(out, "Usage: cattery %s\n\n%s\n", signature(sc), sc.summary)
	if sc.details != "" {
		fmt.Fprintf(out, "\n%s\n", sc.details)
	}
	rows := flagRows(commandFlags(sc.name))
	if len(rows) == 0 {
		return
	}
	fmt.Fprint(out, "\nFlags:\n")
	columns(out, rows)
}

func signature(sc subcommand) string {
	if sc.operands == "" {
		return sc.name
	}
	return sc.name + " " + sc.operands
}

// flagRows describes each flag as a signature and what it does. It replaces
// flag.PrintDefaults, which breaks the line after any name longer than one
// character and so cannot line up with the command list.
func flagRows(flags *flag.FlagSet) [][2]string {
	var rows [][2]string
	if flags == nil {
		return rows
	}
	flags.VisitAll(func(f *flag.Flag) {
		// UnquoteUsage names the value a flag takes, from a backquoted word in
		// the usage text or from the type. It is empty for a bool.
		value, help := flag.UnquoteUsage(f)
		name := "-" + f.Name
		if value != "" {
			name += " " + value
		}
		if f.DefValue != "" && f.DefValue != "false" {
			help += " (default " + f.DefValue + ")"
		}
		rows = append(rows, [2]string{name, help})
	})
	return rows
}

// columns writes an aligned two-column block.
func columns(out io.Writer, rows [][2]string) {
	width := 0
	for _, row := range rows {
		width = max(width, len(row[0]))
	}
	for _, row := range rows {
		fmt.Fprintf(out, "  %-*s  %s\n", width, row[0], row[1])
	}
}

// --- flag sets --------------------------------------------------------------

// newFlagSet builds a subcommand's flag set. A rejected flag gets the usage
// line and a pointer, not the whole description: the flag package has already
// said what it did not understand.
func newFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet("cattery "+name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.Usage = func() {
		sc, ok := lookup(name)
		if !ok {
			return
		}
		fmt.Fprintf(os.Stderr, "Usage: cattery %s\nrun `cattery help %s` for what it does\n", signature(sc), name)
	}
	return flags
}

func pickerFlags() (*flag.FlagSet, *bool, *bool) {
	flags := flag.NewFlagSet("cattery", flag.ContinueOnError)
	printOnly := flags.Bool("print", false, "list agent windows and exit (no TUI); useful for debugging")
	showVersion := flags.Bool("version", false, "print the version and exit")
	return flags, printOnly, showVersion
}

// commandFlags returns the flag set of a command that has flags, and nil for
// one that has none. The help text builds it the same way the command does, so
// the two cannot describe different flags.
func commandFlags(name string) *flag.FlagSet {
	switch name {
	case cmdSetup:
		flags, _ := setupFlags()
		return flags
	case cmdRestore:
		flags, _ := restoreFlags()
		return flags
	default:
		return nil
	}
}

func setupFlags() (*flag.FlagSet, *setup.Options) {
	var opts setup.Options
	flags := newFlagSet(cmdSetup)
	flags.BoolVar(&opts.DryRun, "dry-run", false, "report every action without changing anything")
	flags.BoolVar(&opts.Yes, "yes", false, "answer yes to the Claude Code and pi questions")
	flags.StringVar(&opts.KittyDir, "kitty-dir", "", "kitty config `directory` (default $KITTY_CONFIG_DIRECTORY, else ~/.config/kitty)")
	return flags, &opts
}

// restoreFlags carries -run off by default. Resuming every agent starts one
// process each, and a resume command can point at a session that no longer
// exists.
func restoreFlags() (*flag.FlagSet, *bool) {
	flags := newFlagSet(cmdRestore)
	run := flags.Bool("run", false, "press return on each resume command instead of leaving it at the prompt")
	return flags, run
}

// --- commands ---------------------------------------------------------------

func runSetup(args []string) int {
	flags, opts := setupFlags()
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	// Only a terminal can answer a question. Behind a pipe there is nobody to
	// ask, so setup skips the agent steps instead of guessing.
	if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		opts.In = os.Stdin
	}
	if err := setup.Run(*opts); err != nil {
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
	path, err := parseSessionArgs(newFlagSet(cmdSave), args)
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
	flags, run := restoreFlags()
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

// exitKittyGone says the kitty that took the subscription has exited. It is
// its own code because the fix is to start the command again, under a kitty
// that is running, which a supervisor can do without reading the message.
const exitKittyGone = 3

// runEvents prints agent state transitions, one JSON object per line, until it
// is interrupted or the pipe it writes to closes.
//
// The subscription is a socket registered with the running kitty, so it needs
// the kitten `cattery setup` installed. An install that predates this version
// has no such file, and kitty refuses the call.
func runEvents(client events.KittenRunner, out io.Writer, args []string) int {
	flags := newFlagSet(cmdEvents)
	if err := flags.Parse(args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "cattery: events takes no arguments")
		return 2
	}
	kittyDir, err := setup.KittyDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cattery:", err)
		return 1
	}

	// Ctrl-C is how this command normally ends, and the unregister has to run
	// before the process does.
	//
	// SIGPIPE is in the list for the same reason. Go makes a broken pipe on
	// stdout a fatal signal unless the program asks for it, so `cattery events |
	// head -1` would die where it stands, leaving its path in kitty's registry
	// and its socket on disk. Asking for it turns the write into an EPIPE the
	// read loop already knows how to end on.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGPIPE)
	defer stop()

	err = events.Subscribe(ctx, client, filepath.Join(kittyDir, cattery.EventsFile), out)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, events.ErrKittyGone):
		fmt.Fprintln(os.Stderr, "cattery:", err)
		return exitKittyGone
	default:
		fmt.Fprintln(os.Stderr, "cattery:", err)
		return 1
	}
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
