// Package tmux finds agents running in tmux panes and opens a read-only view of
// one.
//
// A tmux agent publishes the same AGENT_* contract as a kitty agent, as pane
// options named @AGENT_STATE and friends. Nothing watches those the way the
// kitty watcher watches user variables, so the "done" state is derived here, at
// read time, from two extra options the state writer maintains.
package tmux

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alexander-akhmetov/cattery/internal/agent"
)

// viewPrefix marks the grouped sessions `cattery attach` creates. They mirror
// another session's windows, so the lister has to tell them apart from the
// session that owns the agent.
const viewPrefix = "cattery-view-"

// fieldSep separates the fields of one list-panes row. A pane title is free
// text the agent writes, and tmux does not escape it, so the separator has to
// be a byte no title carries: ASCII unit separator.
const fieldSep = "\x1f"

// The pane options carrying the agent contract. @AGENT_WORKED and @AGENT_SEEN
// are what the kitty watcher keeps on the boss object instead.
const (
	optKind   = "@AGENT_KIND"
	optState  = "@AGENT_STATE"
	optMsg    = "@AGENT_MSG"
	optSince  = "@AGENT_SINCE"
	optWorked = "@AGENT_WORKED"
	optSeen   = "@AGENT_SEEN"
)

// listFormat is the row `tmux list-panes -a` prints per pane. The order matches
// the fields constant below.
var listFormat = strings.Join([]string{
	"#{pane_id}", "#{session_name}", "#{window_index}", "#{window_name}",
	"#{pane_current_path}", "#{pane_title}",
	"#{" + optKind + "}", "#{" + optState + "}", "#{" + optMsg + "}",
	"#{" + optSince + "}", "#{" + optWorked + "}", "#{" + optSeen + "}",
}, fieldSep)

// The index of each field in a list-panes row.
const (
	fPaneID = iota
	fSession
	fWindowIndex
	fWindowName
	fPath
	fTitle
	fKind
	fState
	fMsg
	fSince
	fWorked
	fSeen
	fieldCount
)

// Client runs tmux commands.
type Client struct {
	tmux string
}

// installPrefixes are the tmux locations to try when PATH has none.
var installPrefixes = []string{
	"/opt/homebrew/bin/tmux",              // Homebrew, Apple silicon
	"/usr/local/bin/tmux",                 // Homebrew, Intel
	"/home/linuxbrew/.linuxbrew/bin/tmux", // Linuxbrew
}

// NewClient resolves the tmux binary and returns a ready client.
func NewClient() *Client {
	return &Client{tmux: tmuxPath(installPrefixes)}
}

// tmuxPath finds tmux for a process kitty started itself.
//
// The picker and `cattery attach` both run as the command of a kitty window,
// which means no shell and no shell profile: their PATH is kitty's own. A kitty
// started from the Dock has the launchd PATH (/usr/bin:/bin:/usr/sbin:/sbin),
// which carries no Homebrew, so the lookup fails, ListAgents reads that as "no
// tmux on this machine", and every tmux agent leaves the picker without a word.
func tmuxPath(fallbacks []string) string {
	if p, err := exec.LookPath("tmux"); err == nil {
		return p
	}
	for _, fallback := range fallbacks {
		if _, err := os.Stat(fallback); err == nil {
			return fallback
		}
	}
	return "tmux"
}

// ListAgents returns every tmux pane publishing @AGENT_STATE.
//
// No tmux server is not a failure. Most machines running the picker have no
// tmux agents at all, and a red banner over the kitty agents would be wrong.
func (c *Client) ListAgents(ctx context.Context) ([]agent.Agent, error) {
	out, err := exec.CommandContext(ctx, c.tmux, "list-panes", "-a", "-F", listFormat).Output()
	if err != nil {
		if noServer(err) {
			return nil, nil
		}
		return nil, commandError(err)
	}
	return parseAgents(out), nil
}

// noServer reports the failures that mean "there are no tmux agents here":
// tmux is not running, or is not installed at all. A missing binary arrives as
// ErrNotFound from a PATH lookup and as ErrNotExist from an absolute path.
func noServer(err error) bool {
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return true
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	reason := strings.ToLower(string(exitErr.Stderr))
	return strings.Contains(reason, "no server running") || strings.Contains(reason, "error connecting")
}

// commandError keeps tmux's own explanation. Output() stashes stderr on the
// ExitError but leaves Error() as "exit status 1", which tells the user nothing
// in the picker's banner.
func commandError(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if reason := condense(string(exitErr.Stderr)); reason != "" {
			return errors.New(reason)
		}
	}
	return err
}

// condense flattens command output to one line, for the picker's single-line
// banner.
func condense(out string) string { return strings.Join(strings.Fields(out), " ") }

// parseAgents turns list-panes rows into agents, keeping the panes that opted
// into the contract by publishing a state word this picker understands.
//
// A pane in a session that a viewer is grouped with appears once per session,
// so the same pane arrives more than once. The row naming the session that owns
// the pane wins: its target is the one that stays valid after the viewer goes.
func parseAgents(data []byte) []agent.Agent {
	var (
		agents []agent.Agent
		byPane = map[string]int{} // pane id -> index in agents
	)
	for line := range strings.SplitSeq(strings.TrimRight(string(data), "\n"), "\n") {
		fields := strings.Split(line, fieldSep)
		if len(fields) != fieldCount {
			continue
		}
		a, ok := parseAgent(fields)
		if !ok {
			continue
		}
		viewer := strings.HasPrefix(fields[fSession], viewPrefix)
		i, seen := byPane[fields[fPaneID]]
		switch {
		case !seen:
			byPane[fields[fPaneID]] = len(agents)
			if viewer {
				// Keep the row for now: another session may own this pane, and
				// its row replaces this one below.
				a.Target = ""
			}
			agents = append(agents, a)
		case !viewer && agents[i].Target == "":
			agents[i] = a
		}
	}
	// A pane whose only session is a viewer has no target that outlives the
	// viewer, so there is nothing worth attaching to.
	kept := agents[:0]
	for _, a := range agents {
		if a.Target != "" {
			kept = append(kept, a)
		}
	}
	return kept
}

// parseAgent builds one agent from a list-panes row, reporting whether the pane
// publishes agent state at all.
func parseAgent(fields []string) (agent.Agent, bool) {
	display := display(fields[fState], fields[fWorked], fields[fSeen])
	if display == "" {
		return agent.Agent{}, false
	}
	id, err := strconv.Atoi(strings.TrimPrefix(fields[fPaneID], "%"))
	if err != nil {
		return agent.Agent{}, false
	}
	a := agent.Agent{
		ID:      id,
		Host:    agent.HostTmux,
		Kind:    fields[fKind],
		Display: display,
		// The pane title is what agents set for themselves, so the picker's
		// second line has something to show before @AGENT_MSG exists.
		Title: fields[fTitle],
		CWD:   fields[fPath],
		Msg:   fields[fMsg],
		// The window name is display metadata and can repeat; the index is what
		// tmux resolves back to this window. The pane id follows it, because a
		// window can hold two agents and everything past the window index has to
		// address one of them: the marker the attach writes, and the tab the
		// picker matches when the same agent is picked twice.
		Target: fields[fSession] + ":" + fields[fWindowIndex] + "." + fields[fPaneID],
	}
	if secs, err := strconv.ParseInt(fields[fSince], 10, 64); err == nil && secs > 0 {
		a.Since = time.Unix(secs, 0)
	}
	return a, true
}

// display turns the agent's own state word into what the picker shows. It
// mirrors _derive_display in kitty/cattery_watcher.py, with the watcher's
// per-window bookkeeping read off the pane instead:
//
//	worked  the agent has been working or blocked since it last cleared
//	seen    someone has attached to the pane since it went idle
//
// An empty result means the pane is not an agent, or announced a word this
// picker does not know.
func display(state, worked, seen string) string {
	switch state {
	case "working", "blocked":
		return state
	case "idle":
		// An agent that announces idle at startup has finished nothing, and one
		// the user has already looked at has nothing left to report.
		if worked != "" && seen == "" {
			return "done"
		}
		return "idle"
	default:
		return ""
	}
}

// Alive reports whether target still names a live pane.
//
// The picker asks before it opens a viewer tab. A pane can die between the
// reload that listed it and the keypress that picks it, and then the tab opens,
// `cattery attach` fails, and kitty closes the tab before anyone reads why.
//
// A tmux that cannot answer at all is an agent that cannot be reached either,
// so a missing binary and a stopped server both read as "gone" rather than as a
// failure to report.
func (c *Client) Alive(ctx context.Context, target string) (bool, error) {
	_, _, pane, err := splitTarget(target)
	if err != nil {
		return false, err
	}
	err = exec.CommandContext(ctx, c.tmux, "list-panes", "-t", pane, "-F", "#{pane_id}").Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) || noServer(err) {
		return false, nil
	}
	return false, err
}

// Attach opens a read-only view of the window named by target
// ("<session>:<window index>.<pane id>") and returns when the viewer detaches.
//
// The view is a session of its own, grouped with the agent's: grouped sessions
// share windows but keep their own current window, so a viewer neither drags
// the agent's session to another window nor fights a second viewer. `-r` is
// tmux's "read-only,ignore-size" pair, which is what keeps the viewer from
// typing at the agent and from resizing its pane.
func (c *Client) Attach(ctx context.Context, target string) error {
	session, index, pane, err := splitTarget(target)
	if err != nil {
		return err
	}
	view := viewName(session)

	// A signal has to end the attach through the cleanup below, not through Go's
	// default disposition. kitty sends SIGHUP when the viewer tab closes, which
	// is the ordinary way out of a viewer, and a process that dies of it leaves
	// the grouped session standing.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// A viewer killed outright still leaks its session, and pids come back
	// around. Without this the next attach from that pid fails on the name.
	_ = c.kill(view)

	// "=" is tmux's exact-name match, so a session whose name is a prefix of
	// another one is not grouped with the wrong windows.
	if err := c.run(ctx, "new-session", "-d", "-t", "="+session, "-s", view); err != nil {
		return err
	}
	// The view session outlives the client that made it, so it has to go on the
	// way out: after a detach, and after an attach that never started.
	defer func() { _ = c.kill(view) }()

	if err := c.run(ctx, "select-window", "-t", view+":"+index); err != nil {
		return err
	}
	// On the pane, not on the window: a window target resolves to whichever pane
	// is active, which is another agent's when the window is split. Best effort,
	// because the marker only decides whether the picker still calls this agent
	// "done", and a pane that went away is not worth failing the attach.
	_ = c.run(ctx, "set", "-p", "-t", pane, optSeen, "1")

	// A child process, not syscall.Exec: the deferred cleanup has to run.
	attach := exec.CommandContext(ctx, c.tmux, "attach-session", "-r", "-t", view)
	attach.Stdin, attach.Stdout, attach.Stderr = os.Stdin, os.Stdout, os.Stderr
	// A signalled attach gets SIGTERM rather than the default kill, so the tmux
	// client hands the terminal back before it goes.
	attach.Cancel = func() error { return attach.Process.Signal(syscall.SIGTERM) }
	attach.WaitDelay = 2 * time.Second
	if err := attach.Run(); err != nil {
		return fmt.Errorf("attach to %s: %w", target, err)
	}
	return nil
}

// splitTarget reads a "<session>:<window index>.<pane id>" target.
//
// The window index has to be a number and the pane a "%" id: a window name
// there would resolve to a different window as soon as two windows share a
// name, and a pane index moves when a neighbour closes. Both are read from the
// end, because tmux allows ":" and "." in a session name and resolves its own
// targets the same way.
func splitTarget(target string) (session, index, pane string, err error) {
	fail := func(format string, args ...any) (string, string, string, error) {
		return "", "", "", fmt.Errorf(format, args...)
	}
	dot := strings.LastIndex(target, ".")
	if dot < 0 {
		return fail("target %q is not <session>:<window>.<pane>", target)
	}
	pane = target[dot+1:]
	session, index, found := cutLast(target[:dot], ":")
	switch {
	case !found:
		return fail("target %q is not <session>:<window>.<pane>", target)
	case session == "":
		return fail("target %q names no session", target)
	case index == "":
		return fail("target %q names no window", target)
	}
	if _, err := strconv.Atoi(index); err != nil {
		return fail("target %q: window %q is not an index", target, index)
	}
	digits, isPaneID := strings.CutPrefix(pane, "%")
	if _, err := strconv.Atoi(digits); !isPaneID || err != nil {
		return fail("target %q: pane %q is not a pane id", target, pane)
	}
	return session, index, pane, nil
}

// cutLast is strings.Cut around the last separator instead of the first.
func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// viewName is the grouped session one attachment creates. The pid keeps two
// viewers of one agent apart. tmux allows "." and ":" in a session name but
// reads both as target separators, so a view named after such a session could
// not be selected by name.
func viewName(session string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '-'
		}
	}, session)
	return viewPrefix + clean + "-" + strconv.Itoa(os.Getpid())
}

func (c *Client) run(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, c.tmux, args...).CombinedOutput()
	if err == nil {
		return nil
	}
	if reason := condense(string(out)); reason != "" {
		return fmt.Errorf("tmux %s: %s", args[0], reason)
	}
	return fmt.Errorf("tmux %s: %w", args[0], err)
}

// kill removes the view session. It runs on its own context: the caller's is
// already cancelled when a signal ends the attach, and the session would leak.
func (c *Client) kill(view string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return c.run(ctx, "kill-session", "-t", "="+view)
}
