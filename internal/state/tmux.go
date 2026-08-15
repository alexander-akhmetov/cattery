package state

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The pane options the tmux transport keeps beyond the shared contract. The
// kitty watcher holds the same facts on kitty's boss object; a tmux pane has
// nowhere else to put them, and the picker derives "done" from them.
//
// The names carry no "@": option() adds the prefix tmux keeps user options
// under, so these read like the kitty variables they mirror.
const (
	varWorked = "AGENT_WORKED" // the agent has worked since it last cleared
	varSeen   = "AGENT_SEEN"   // someone has attached since it went idle
	varSince  = "AGENT_SINCE"  // unix seconds of the last state change
)

// tmuxTransport publishes the AGENT_* contract as options on one tmux pane. It
// is what a detached pane needs: the OSC escape has no terminal to reach.
type tmuxTransport struct {
	tmux string // the binary, a field so a test can point it at a stub
	pane string // $TMUX_PANE, e.g. "%17"
}

// Publish writes the batch as pane options, in one tmux process.
//
// It also maintains what the kitty watcher would: @AGENT_SINCE moves only when
// the state word changes, so the picker's elapsed time counts one state rather
// than one hook; @AGENT_WORKED marks an agent that has something to finish; and
// @AGENT_SEEN goes as soon as it works again, so the next idle is reported.
func (t tmuxTransport) Publish(vars []Var) error {
	state, publishes := stateWord(vars)
	options := make([]Var, 0, len(vars)+3)
	options = append(options, vars...)

	switch {
	case !publishes:
	case state.Delete:
		// The agent is gone. Without this the next agent in the pane would
		// inherit "has worked", and report "done" the moment it starts idle.
		options = append(options, Var{Name: varWorked, Delete: true})
	default:
		if t.previousState() != state.Value {
			options = append(options, Var{Name: varSince, Value: strconv.FormatInt(time.Now().Unix(), 10)})
		}
		if state.Value == "working" || state.Value == "blocked" {
			options = append(options,
				Var{Name: varWorked, Value: "1"},
				Var{Name: varSeen, Delete: true},
			)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	return exec.CommandContext(ctx, t.tmux, t.args(options)...).Run()
}

// args chains every update into one tmux command line. A ";" argument is
// tmux's command separator, so this is one process rather than one per option.
func (t tmuxTransport) args(vars []Var) []string {
	args := make([]string, 0, len(vars)*7)
	for i, v := range vars {
		if i > 0 {
			args = append(args, ";")
		}
		args = append(args, "set", "-p")
		if v.Delete {
			args = append(args, "-u")
		}
		args = append(args, "-t", t.pane, option(v.Name))
		if !v.Delete {
			args = append(args, escapeArg(v.Value))
		}
	}
	return args
}

// escapeArg protects a value from the command splitter. tmux ends a command at
// any argument that ends in ";", not only at one that is nothing else, and
// AGENT_MSG carries whatever the user typed: a prompt ending in ";" would lose
// that character, and a prompt that is only ";" would end the command and drop
// every update chained behind it.
//
// A "\;" ending is the escaped form, and tmux hands back the ";". Backslashes
// already in the value stay as they are: only the terminator needs the escape.
func escapeArg(value string) string {
	if !strings.HasSuffix(value, ";") {
		return value
	}
	return value[:len(value)-1] + `\;`
}

// previousState is the state word already on the pane, or "" when the pane has
// none. A failure reads as "no state", which republishes @AGENT_SINCE: the
// elapsed time is then wrong by seconds instead of never starting.
func (t tmuxTransport) previousState() string {
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, t.tmux, "show", "-pqv", "-t", t.pane, option(varState)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// stateWord picks the AGENT_STATE update out of a batch, and reports whether
// the batch carries one at all.
func stateWord(vars []Var) (Var, bool) {
	for _, v := range vars {
		if v.Name == varState {
			return v, true
		}
	}
	return Var{}, false
}

// option is the pane-option name for a user variable. tmux keeps user options
// under an "@" prefix; kitty's user variables have no prefix.
func option(name string) string { return "@" + name }
