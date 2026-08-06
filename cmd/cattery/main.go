// Command cattery is the whole of cattery: an installer, the agent-state
// writer, and the agent picker.
//
//	cattery                  the picker, a full-screen list kitty runs in an
//	                         overlay window
//	cattery -print           the same inventory without the TUI
//	cattery setup            install the kitty files and the config they need
//	cattery state <x>        publish this window's agent state
//
// The picker reads the live kitty window inventory and shows every window
// carrying AGENT_DISPLAY, which the kitty watcher derives from the agent's own
// AGENT_STATE, so a window that sets AGENT_STATE with no watcher loaded does
// not appear. `cattery setup` binds it in kitty.conf as:
//
//	map opt+a>opt+a launch --type=overlay --cwd=current --copy-colors /path/to/cattery
//
// That overlay is a normal kitty window, so the picker inherits KITTY_LISTEN_ON
// and `kitten @` remote control works against the same instance.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/alexander-akhmetov/cattery/internal/kitty"
	"github.com/alexander-akhmetov/cattery/internal/overlay"
	"github.com/alexander-akhmetov/cattery/internal/setup"
	"github.com/alexander-akhmetov/cattery/internal/state"
)

// The names route returns. They are plain strings rather than a defined type so
// the switch that runs them does not have to be exhaustive over every one.
const (
	cmdPicker = "picker"
	cmdPrint  = "print"
	cmdSetup  = "setup"
	cmdState  = "state"
)

// command is what one argv asks for. Routing is separate from running so it can
// be tested without launching the TUI or writing to a kitty window.
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
	case cmdState:
		// Never fails: a hook that reported an error would surface it in the
		// agent's transcript, and a state write is not worth that.
		state.Run(cmd.args)
		return 0
	case cmdSetup:
		return runSetup(cmd.args)
	case cmdPrint:
		if err := printAgents(kitty.NewClient()); err != nil {
			fmt.Fprintln(os.Stderr, "cattery:", err)
			return 1
		}
		return 0
	default:
		program := tea.NewProgram(overlay.New(kitty.NewClient()), tea.WithAltScreen())
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
		case cmdSetup, cmdState:
			return command{name: args[0], args: args[1:]}, nil
		default:
			return command{}, fmt.Errorf("unknown command %q (try setup, state, or no argument for the picker)", args[0])
		}
	}

	flags := flag.NewFlagSet("cattery", flag.ContinueOnError)
	flags.SetOutput(out)
	printOnly := flags.Bool("print", false, "list agent windows and exit (no TUI); useful for debugging")
	if err := flags.Parse(args); err != nil {
		return command{}, err
	}
	if *printOnly {
		return command{name: cmdPrint}, nil
	}
	return command{name: cmdPicker}, nil
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
	// Only a terminal can answer a question. A piped stdin reaches setup with
	// nobody behind it, and the agent steps are skipped rather than guessed at.
	if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		opts.In = os.Stdin
	}
	if err := setup.Run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "cattery:", err)
		return 1
	}
	return 0
}

func printAgents(client *kitty.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	agents, err := client.ListAgents(ctx)
	if err != nil {
		return err
	}
	for _, a := range agents {
		fmt.Printf("%-16s %-7s %-7s id=%-5d %-24s %s\n", a.Project, a.Display, a.Kind, a.ID, a.Branch, a.CWD)
	}
	return nil
}
