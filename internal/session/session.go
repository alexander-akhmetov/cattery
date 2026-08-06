// Package session snapshots the kitty tab tree and puts it back.
//
// kitty 0.48 does the layout half: `save_as_session` writes a session file
// naming every tab, its directory, its layout, and one `launch` line per
// window, and `goto_session` recreates those tabs. kitty does not record which
// agent session a tab held.
//
// Save runs kitty's action and rewrites the `launch` lines: it drops the
// picker's overlay window, drops the live agent state, keeps the AGENT_RESUME
// command the agent published, and drops the recorded command so the tab comes
// back as a plain shell. Restore recreates the tabs and types each AGENT_RESUME
// at the prompt of its window, with no carriage return.
package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/alexander-akhmetov/cattery/internal/kitty"
	"github.com/alexander-akhmetov/cattery/internal/shellquote"
)

// varResume holds a command that reopens the agent session a window held.
// Agents publish it; cattery never reads inside it.
const varResume = "AGENT_RESUME"

// varPrefix marks the user variables this package understands. Everything under
// it except varResume is live state that must not reach a snapshot.
const varPrefix = "AGENT_"

// snapshotMode keeps a snapshot private: it holds every prompt the user typed
// at an agent.
const snapshotMode = 0o600

// How long Restore waits for restored windows to draw a shell prompt before it
// types into them anyway. Typing early is safe, because kitty buffers the text
// until the shell reads it. Variables, so tests need not wait.
var (
	readyDeadline = 3 * time.Second
	readyPoll     = 100 * time.Millisecond
)

// envSessionFile names the snapshot when no path is given on the command line.
const envSessionFile = "CATTERY_SESSION_FILE"

// DefaultPath is the snapshot to use when the caller names none:
// CATTERY_SESSION_FILE, else a file under kitty's own sessions directory.
func DefaultPath() (string, error) {
	if path := os.Getenv(envSessionFile); path != "" {
		return Abs(path)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to put a snapshot in: %w", err)
	}
	return filepath.Join(home, ".local", "share", "kitty", "sessions", "agents.kitty-session"), nil
}

// Abs resolves a snapshot path against the directory cattery was run from.
// Every path handed to Save or Restore must go through this. kitty resolves a
// relative path against directories of its own: save_as_session against the
// kitty process's directory, goto_session against the kitty configuration
// directory. One relative string would name three files.
func Abs(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve the snapshot path %q: %w", path, err)
	}
	return abs, nil
}

// EnsureDir creates the directory a snapshot goes in, private because the
// snapshots inside hold every prompt the user typed at an agent.
func EnsureDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o700)
}

// Client is the part of kitty remote control this package needs.
type Client interface {
	Action(ctx context.Context, arg string) error
	SendText(ctx context.Context, id int, text string) error
	Windows(ctx context.Context) ([]kitty.Window, error)
}

// Stats is what a save or a restore did, for the CLI summary and the picker's
// notice.
type Stats struct {
	Tabs      int // tabs in the snapshot
	Resumable int // windows in the snapshot carrying a resume command
	Typed     int // resume commands sent into restored windows
}

// Saved is the one-line summary of a save, printed by both the CLI and the
// picker.
func (s Stats) Saved(path string) string {
	return fmt.Sprintf("saved %s, %s -> %s",
		plural(s.Tabs, "tab"), plural(s.Resumable, "resumable agent"), path)
}

// Restored is the one-line summary of a restore. It always names both counts:
// a restore that passed its readiness deadline types fewer commands than the
// snapshot holds and still returns no error.
func (s Stats) Restored(run bool) string {
	verb := "typed"
	if run {
		verb = "ran"
	}
	return fmt.Sprintf("restored %s, %s %d of %d resume commands",
		plural(s.Tabs, "tab"), verb, s.Typed, s.Resumable)
}

// Incomplete reports a restore that left resume commands untyped.
func (s Stats) Incomplete() bool { return s.Typed < s.Resumable }

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// Save writes a snapshot of the current kitty instance to path, which must be
// absolute. Abs and DefaultPath both return absolute paths.
//
// The previous snapshot moves to <path>.prev. Nothing replaces <path> until the
// rewrite has finished, so a failure leaves the last good snapshot in place.
func Save(ctx context.Context, client Client, path string) (Stats, error) {
	native, cleanup, err := kittySnapshot(ctx, client, path)
	if err != nil {
		return Stats{}, err
	}
	defer cleanup()

	data, err := os.ReadFile(native)
	if err != nil {
		return Stats{}, fmt.Errorf("read kitty's session file: %w", err)
	}
	rewritten, stats := rewrite(data)
	// A running kitty always has one tab, so no tabs means the action produced
	// nothing. Installing that would rotate a good snapshot away.
	if stats.Tabs == 0 {
		return Stats{}, errors.New("kitty wrote no tabs to its session file")
	}

	if err := install(path, rewritten); err != nil {
		return Stats{}, err
	}
	return stats, nil
}

// kittySnapshot asks kitty to write its own session file beside the target and
// returns its path. The temporary file shares the target's directory, so the
// rename into place stays inside one filesystem.
func kittySnapshot(ctx context.Context, client Client, path string) (string, func(), error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cattery-native-*.kitty-session")
	if err != nil {
		return "", nil, fmt.Errorf("create a temporary session file: %w", err)
	}
	name := tmp.Name()
	tmp.Close()
	cleanup := func() { os.Remove(name) }

	// The action and its arguments go in one string. Separate argv entries make
	// kitty open a window titled "Invalid save_as_session command line".
	//
	// --use-foreground-process is deliberately absent. It records the process
	// the window is running, and restore would start that again.
	if err := client.Action(ctx, "save_as_session --save-only "+shellquote.Quote(name)); err != nil {
		cleanup()
		return "", nil, err
	}
	return name, cleanup, nil
}

// install puts data at path with the previous file kept as <path>.prev.
func install(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cattery-snapshot-*")
	if err != nil {
		return fmt.Errorf("create a temporary snapshot: %w", err)
	}
	staged := tmp.Name()
	defer os.Remove(staged) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write the snapshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write the snapshot: %w", err)
	}
	// CreateTemp already makes the file 0600. Set it anyway: the mode is part of
	// the contract, and chmod ignores umask.
	if err := os.Chmod(staged, snapshotMode); err != nil {
		return fmt.Errorf("set the snapshot mode: %w", err)
	}

	prev := path + ".prev"
	rotated := false
	switch info, err := os.Stat(path); {
	case err == nil && !info.Mode().IsRegular():
		// Renaming would move a directory aside and leave a session file where
		// it used to be.
		return fmt.Errorf("%s is not a regular file", path)
	case err == nil:
		if err := os.Rename(path, prev); err != nil {
			return fmt.Errorf("keep the previous snapshot: %w", err)
		}
		rotated = true
	}
	if err := os.Rename(staged, path); err != nil {
		// Put the old snapshot back, or the user is left with neither.
		if rotated {
			_ = os.Rename(prev, path)
		}
		return fmt.Errorf("install the snapshot: %w", err)
	}
	return nil
}

// Restore recreates the tabs in path and leaves each restored agent's resume
// command at the prompt of its window. With run set it presses return for them.
// path must be absolute; see Abs.
func Restore(ctx context.Context, client Client, path string, run bool) (Stats, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Stats{}, fmt.Errorf("read the snapshot: %w", err)
	}
	stats := count(data)
	name := sessionName(path)

	// goto_session is not idempotent: a second run builds a second copy of
	// every tab. kitty tags the windows it created with the file's name, which
	// is the only way to notice from outside.
	windows, err := client.Windows(ctx)
	if err != nil {
		return Stats{}, err
	}
	if slices.ContainsFunc(windows, func(w kitty.Window) bool { return w.SessionName == name }) {
		return Stats{}, fmt.Errorf("session %q is already open: restoring again would duplicate every tab, "+
			"so close it first with `kitten @ action 'close_session %s'`", name, path)
	}

	if err := client.Action(ctx, "goto_session "+shellquote.Quote(path)); err != nil {
		return Stats{}, err
	}

	targets, err := awaitResumable(ctx, client, name, stats.Resumable)
	if err != nil {
		return stats, err
	}
	stats.Typed, err = typeResumeCommands(ctx, client, targets, run)
	return stats, err
}

// awaitResumable waits for the restored windows carrying a resume command to
// draw a shell prompt, and returns them ordered by window id. want is how many
// the snapshot holds.
//
// The count is what makes an early return safe. kitty creates the windows as it
// works through the session file, so a poll that catches one ready says nothing
// about the rest. Windows with nothing to type are never waited for: a snapshot
// is mostly plain shells, and one slow shell must not hold up the agents.
//
// Passing the deadline is not a failure. kitty buffers text sent to a window
// that has not started reading, so the command appears a moment later.
func awaitResumable(ctx context.Context, client Client, name string, want int) ([]kitty.Window, error) {
	if want == 0 {
		return nil, nil
	}
	deadline := time.Now().Add(readyDeadline)
	var (
		targets []kitty.Window
		lastErr error
	)
	for {
		windows, err := client.Windows(ctx)
		if err != nil {
			lastErr = err
		} else {
			lastErr = nil
			targets = resumableIn(windows, name)
			if len(targets) >= want && !slices.ContainsFunc(targets, func(w kitty.Window) bool { return !w.AtPrompt }) {
				return targets, nil
			}
		}
		if !time.Now().Before(deadline) {
			// Report a failure only when nothing came back at all.
			if len(targets) == 0 && lastErr != nil {
				return nil, lastErr
			}
			return targets, nil
		}
		select {
		case <-ctx.Done():
			return targets, ctx.Err()
		case <-time.After(readyPoll):
		}
	}
}

// resumableIn picks the windows this restore created that have a command to
// type, ordered by window id so the result is stable.
func resumableIn(windows []kitty.Window, name string) []kitty.Window {
	var out []kitty.Window
	for _, w := range windows {
		if w.SessionName == name && w.UserVars[varResume] != "" {
			out = append(out, w)
		}
	}
	slices.SortFunc(out, func(a, b kitty.Window) int { return a.ID - b.ID })
	return out
}

// typeResumeCommands types one resume command into each window. Without run it
// sends no carriage return, so the command waits at the prompt. The command is
// opaque to cattery, and the session behind it may be gone.
//
// A window that refuses the text does not stop the others. The errors are
// reported once every window has been tried.
func typeResumeCommands(ctx context.Context, client Client, targets []kitty.Window, run bool) (int, error) {
	var (
		typed int
		errs  []error
	)
	for _, w := range targets {
		text := w.UserVars[varResume]
		if run {
			text += "\r"
		}
		if err := client.SendText(ctx, w.ID, text); err != nil {
			errs = append(errs, err)
			continue
		}
		typed++
	}
	return typed, errors.Join(errs...)
}

// sessionExtensions are the suffixes kitty drops when it names the windows a
// session file created (SESSION_FILE_EXTENSIONS in kitty's source). Any other
// suffix stays part of the name: /x/agents.snapshot is "agents.snapshot".
var sessionExtensions = []string{".kitty-session", ".kitty_session", ".session"}

// sessionName is what kitty calls the windows a session file created. Getting
// it wrong is silent: restore then recognises none of the windows it made, so
// it types no resume commands and the duplicate guard never fires.
func sessionName(path string) string {
	base := filepath.Base(path)
	for _, ext := range sessionExtensions {
		if trimmed, ok := strings.CutSuffix(base, ext); ok {
			return trimmed
		}
	}
	return base
}

// --- the session-file rewrite ------------------------------------------------

// rewrite strips a native kitty session file of everything that must not come
// back, touching only `launch` lines. Every other keyword stays exactly as
// kitty wrote it: `cd` holds unquoted paths that can contain spaces, and
// `set_layout_state` holds JSON.
func rewrite(data []byte) ([]byte, Stats) {
	lines := strings.Split(string(data), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		kept, drop := rewriteLine(line)
		if drop {
			continue
		}
		out = append(out, kept)
	}
	joined := strings.Join(out, "\n")
	return []byte(joined), count([]byte(joined))
}

// rewriteLine returns one line's replacement, and whether the line goes away.
func rewriteLine(line string) (string, bool) {
	if !isLaunch(line) {
		return line, false
	}
	tokens, ok := splitTokens(line)
	if !ok {
		// Unbalanced quotes mean this is not the line shape the rules expect.
		// Leaving it alone beats rewriting it wrongly.
		return line, false
	}
	if isOverlay(tokens) {
		// The picker is an overlay window, and save_as_session records it, so a
		// snapshot taken from the picker would restore the picker.
		return "", true
	}
	drop := dropped(tokens)
	if !slices.Contains(drop, true) {
		return line, false
	}
	return rebuild(tokens, drop), false
}

// dropped marks the tokens of one launch line that must not survive: every live
// AGENT_* variable, plus the recorded command on a line that carried one.
func dropped(tokens []token) []bool {
	drop := make([]bool, len(tokens))
	agent := false
	for i, tok := range tokens {
		if !isAgentVar(tok.value) {
			continue
		}
		agent = true
		// AGENT_RESUME stays: it tells the restored window which agent session
		// it held.
		if !isResumeVar(tok.value) {
			drop[i] = true
		}
	}
	// An agent window records the command that started it, such as
	// `pi --fork <session>`. Running that again forks a session that ended, so
	// the tab comes back as a plain shell and restore types the resume command
	// into it.
	if agent {
		if cmd := commandStart(tokens); cmd >= 0 {
			for i := cmd; i < len(tokens); i++ {
				drop[i] = true
			}
		}
	}
	return drop
}

// rebuild joins the surviving tokens exactly as kitty wrote them. Each token
// keeps its own quoting, so nothing is requoted.
func rebuild(tokens []token, drop []bool) string {
	kept := make([]string, 0, len(tokens))
	for i, tok := range tokens {
		if !drop[i] {
			kept = append(kept, tok.raw)
		}
	}
	return strings.Join(kept, " ")
}

func isLaunch(line string) bool { return line == "launch" || strings.HasPrefix(line, "launch ") }

func isAgentVar(value string) bool { return strings.HasPrefix(value, "--var="+varPrefix) }

func isResumeVar(value string) bool { return strings.HasPrefix(value, "--var="+varResume+"=") }

// isOverlay reports an overlay window of any kind. kitty writes both
// --type=overlay and --type=overlay-main, so the check is a prefix test.
func isOverlay(tokens []token) bool {
	return slices.ContainsFunc(tokens, func(t token) bool {
		return strings.HasPrefix(t.value, "--type=overlay")
	})
}

// commandStart is the index of the first token of the command kitty recorded,
// or -1 for a plain shell window that names no command.
//
// Everything kitty writes before the command is a --flag=value or the
// kitty-unserialize-data blob. That blob is the one non-flag token that does
// not start the command.
func commandStart(tokens []token) int {
	for i, tok := range tokens {
		if i == 0 {
			continue // "launch"
		}
		if strings.HasPrefix(tok.value, "-") || strings.HasPrefix(tok.value, "kitty-unserialize-data=") {
			continue
		}
		return i
	}
	return -1
}

// count reports what a session file holds.
func count(data []byte) Stats {
	var stats Stats
	for line := range strings.SplitSeq(string(data), "\n") {
		switch {
		case line == "new_tab" || strings.HasPrefix(line, "new_tab "):
			stats.Tabs++
		case isLaunch(line):
			tokens, ok := splitTokens(line)
			if ok && slices.ContainsFunc(tokens, func(t token) bool { return isResumeVar(t.value) }) {
				stats.Resumable++
			}
		}
	}
	return stats
}

// --- shell-style tokens -------------------------------------------------------

// token is one word of a launch line, both as kitty wrote it and as a shell
// would read it. The rewrite writes back the raw form, so a surviving token is
// byte-for-byte what kitty produced.
type token struct {
	raw   string
	value string
}

// splitTokens splits a line the way a POSIX shell would, which is how kitty
// wrote it. Unbalanced quotes report false.
func splitTokens(line string) ([]token, bool) {
	var out []token
	for i := 0; i < len(line); {
		if line[i] == ' ' || line[i] == '\t' {
			i++
			continue
		}
		start := i
		value, next, ok := scanToken(line, i)
		if !ok {
			return nil, false
		}
		i = next
		out = append(out, token{raw: line[start:i], value: value})
	}
	return out, true
}

// scanToken reads one word starting at i and returns its unquoted value and the
// index just past it.
func scanToken(line string, i int) (string, int, bool) {
	var value strings.Builder
	for i < len(line) && line[i] != ' ' && line[i] != '\t' {
		var ok bool
		switch line[i] {
		case '\'':
			i, ok = scanQuoted(line, i, &value)
		case '"':
			i, ok = scanDoubleQuoted(line, i, &value)
		case '\\':
			i, ok = scanEscape(line, i, &value)
		default:
			value.WriteByte(line[i])
			i, ok = i+1, true
		}
		if !ok {
			return "", 0, false
		}
	}
	return value.String(), i, true
}

// scanQuoted reads '...', where nothing is special, including a backslash.
// kitty uses this form for a user variable holding spaces.
func scanQuoted(line string, i int, value *strings.Builder) (int, bool) {
	for i++; i < len(line); i++ {
		if line[i] == '\'' {
			return i + 1, true
		}
		value.WriteByte(line[i])
	}
	return 0, false
}

// scanDoubleQuoted reads "...", where a backslash escapes the next byte. kitty
// does not produce this form, but a hand-edited snapshot can.
func scanDoubleQuoted(line string, i int, value *strings.Builder) (int, bool) {
	for i++; i < len(line); i++ {
		if line[i] == '"' {
			return i + 1, true
		}
		if line[i] == '\\' && i+1 < len(line) {
			i++
		}
		value.WriteByte(line[i])
	}
	return 0, false
}

func scanEscape(line string, i int, value *strings.Builder) (int, bool) {
	if i+1 >= len(line) {
		return 0, false
	}
	value.WriteByte(line[i+1])
	return i + 2, true
}
