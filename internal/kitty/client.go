// Package kitty talks to a running kitty instance through its remote-control
// CLI (`kitten @ ...`). It enumerates windows that publish the agent-state
// user variables and focuses a chosen window.
package kitty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/alexander-akhmetov/cattery/internal/agent"
)

// Client runs kitty remote-control commands.
type Client struct {
	kitten string
}

// NewClient resolves the kitten binary and returns a ready client.
func NewClient() *Client {
	return &Client{kitten: kittenPath()}
}

func kittenPath() string {
	if p, err := exec.LookPath("kitten"); err == nil {
		return p
	}
	// macOS .app bundle layout fallback, for a PATH without the kitty bundle.
	const fallback = "/Applications/kitty.app/Contents/MacOS/kitten"
	if _, err := os.Stat(fallback); err == nil {
		return fallback
	}
	return "kitten"
}

// ListAgents returns the agent windows known to kitty, in kitty's own order. It
// resolves no repositories and does not sort: the picker merges these with the
// agents of other hosts first, so both passes run once over the whole set.
func (c *Client) ListAgents(ctx context.Context) ([]agent.Agent, error) {
	out, err := c.ls(ctx)
	if err != nil {
		return nil, err
	}
	return parseAgents(out)
}

// FocusWindow focuses the kitty window with the given id, switching OS window
// if needed. kitty explains a rejection on stderr ("no matching window"), so a
// failure carries that reason and the target id.
func (c *Client) FocusWindow(ctx context.Context, id int) error {
	return run(focusCommand(ctx, c.kitten, id), window(id))
}

// run executes one remote-control command and keeps kitty's explanation for a
// failure. CombinedOutput carries it: kitty writes the reason on stderr, and
// the error alone says "exit status 1". what names the target, because a picker
// banner shows the message with nothing around it.
func run(cmd *exec.Cmd, what string) error {
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if reason := condense(string(out)); reason != "" {
		return fmt.Errorf("%s: %s", what, reason)
	}
	return fmt.Errorf("%s: %w", what, err)
}

func window(id int) string { return "window " + strconv.Itoa(id) }

// condense flattens command output to one line. The picker renders errors in a
// single-line banner, and kitty's tracebacks span many lines.
func condense(out string) string {
	return strings.Join(strings.Fields(out), " ")
}

// commandError keeps kitty's explanation for a failed command. Output() stashes
// stderr on the ExitError but leaves Error() as "exit status 1", which tells
// the user nothing in the picker's error banner.
func commandError(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if reason := condense(string(exitErr.Stderr)); reason != "" {
			return errors.New(reason)
		}
	}
	return err
}

func focusCommand(ctx context.Context, kitten string, id int) *exec.Cmd {
	return exec.CommandContext(ctx, kitten, "@", "focus-window", "--match", "id:"+strconv.Itoa(id))
}

// Text returns what one kitty window is showing right now, with its colours.
//
// The extent is the visible screen, which for an agent in the alternate screen
// is the TUI frame rather than the shell scrollback underneath it. Output()
// rather than CombinedOutput(), because kitty's answer is the data: stderr goes
// through commandError instead of into the middle of the screen.
func (c *Client) Text(ctx context.Context, id int) (string, error) {
	out, err := textCommand(ctx, c.kitten, id).Output()
	if err != nil {
		return "", fmt.Errorf("%s: %w", window(id), commandError(err))
	}
	return string(out), nil
}

func textCommand(ctx context.Context, kitten string, id int) *exec.Cmd {
	// --ansi keeps the colours. --add-cursor and --add-wrap-markers are left
	// off: both add escapes to text that goes inside another TUI's frame.
	return exec.CommandContext(ctx, kitten, "@", "get-text",
		"--match", "id:"+strconv.Itoa(id), "--extent", "screen", "--ansi")
}

// SetUserVars publishes user variables on one kitty window, in the order given.
// Each entry is "NAME=value" to set a variable and a bare "NAME" to delete it,
// the form `kitten @ set-user-vars` takes.
//
// The state writer uses this when its OSC escape cannot reach a terminal, which
// is every Claude command hook. It matches by window id, so the value reaches
// the window the agent runs in even when another window is active.
func (c *Client) SetUserVars(ctx context.Context, id int, vars []string) error {
	if len(vars) == 0 {
		return nil
	}
	return run(setUserVarsCommand(ctx, c.kitten, id, vars), window(id))
}

func setUserVarsCommand(ctx context.Context, kitten string, id int, vars []string) *exec.Cmd {
	args := append([]string{"@", "set-user-vars", "--match", "id:" + strconv.Itoa(id)}, vars...)
	return exec.CommandContext(ctx, kitten, args...)
}

// Action runs one kitty action over remote control, as `kitten @ action <arg>`.
//
// The action name and its arguments go in a single string, which is what
// kitty's parser wants. Separate argv entries make kitty open a window titled
// "Invalid <action> command line". Quote any path inside arg with
// internal/shellquote.
func (c *Client) Action(ctx context.Context, arg string) error {
	return run(actionCommand(ctx, c.kitten, arg), fmt.Sprintf("action %q", arg))
}

func actionCommand(ctx context.Context, kitten, arg string) *exec.Cmd {
	return exec.CommandContext(ctx, kitten, "@", "action", arg)
}

// Launch opens a window as `kitten @ launch <args...>`. Unlike Action, this is
// a remote-control command with its own parser, so the arguments stay separate
// argv entries and nothing has to be quoted for a shell.
//
// The picker uses it for the read-only tmux viewer tab.
func (c *Client) Launch(ctx context.Context, args []string) error {
	return run(launchCommand(ctx, c.kitten, args), "launch "+strings.Join(args, " "))
}

func launchCommand(ctx context.Context, kitten string, args []string) *exec.Cmd {
	return exec.CommandContext(ctx, kitten, append([]string{"@", "launch"}, args...)...)
}

// SendText types text into one kitty window, as if the user had typed it. It
// adds no carriage return; the caller appends one to run what it sent.
//
// kitty reports no error when a match finds nothing, so the id must come from
// Windows.
func (c *Client) SendText(ctx context.Context, id int, text string) error {
	return run(sendTextCommand(ctx, c.kitten, id, text), window(id))
}

func sendTextCommand(ctx context.Context, kitten string, id int, text string) *exec.Cmd {
	// The text goes in on stdin, which kitty documents as "sent as is, not
	// interpreted for escapes". kitty reads Python escapes out of a positional
	// text argument: the POSIX '\'' idiom shellquote emits would arrive as ''',
	// and a \n in a path would become a real newline that runs the command.
	cmd := exec.CommandContext(ctx, kitten, "@", "send-text", "--match", "id:"+strconv.Itoa(id), "--stdin")
	cmd.Stdin = strings.NewReader(text)
	return cmd
}

// Windows returns every kitty window, unfiltered. Session restore needs this
// rather than ListAgents: a freshly restored window has published no agent
// state yet, and carries only the AGENT_RESUME from its snapshot.
func (c *Client) Windows(ctx context.Context) ([]Window, error) {
	out, err := c.ls(ctx)
	if err != nil {
		return nil, err
	}
	return parseWindows(out)
}

// ls is the window inventory as JSON. It calls Output() instead of run(),
// because kitty's answer is the point; commandError digs the explanation out of
// the ExitError instead of mixing stderr into the JSON.
func (c *Client) ls(ctx context.Context) ([]byte, error) {
	out, err := exec.CommandContext(ctx, c.kitten, "@", "ls").Output()
	if err != nil {
		return nil, commandError(err)
	}
	return out, nil
}

// --- kitten @ ls JSON shape (only the fields we use) ------------------------

type rawWindow struct {
	ID          int               `json:"id"`
	Title       string            `json:"title"`
	CWD         string            `json:"cwd"`
	CreatedAt   int64             `json:"created_at"` // unix nanoseconds
	UserVars    map[string]string `json:"user_vars"`
	AtPrompt    bool              `json:"at_prompt"`
	SessionName string            `json:"session_name"`
}

type rawTab struct {
	Windows []rawWindow `json:"windows"`
}

type rawOSWindow struct {
	Tabs []rawTab `json:"tabs"`
}

// Window is one kitty window as `kitten @ ls` reports it, with no filtering.
type Window struct {
	ID       int
	Title    string
	CWD      string
	UserVars map[string]string

	// CreatedAt is when kitty opened the window; zero when kitty did not say.
	CreatedAt time.Time

	// SessionName is the session file that created this window, named after the
	// file's basename without its extension. Empty for a window the user
	// opened. kitty records it per window; its tab dictionaries have no such
	// key.
	SessionName string

	// AtPrompt is true once the shell has drawn a prompt and is waiting. Restore
	// uses it to decide when a restored window can be typed into.
	AtPrompt bool
}

// parseWindows decodes `kitten @ ls` output into every window it reports. It
// keeps windows with no user variables, because a window restored from a
// snapshot has published no agent state yet.
func parseWindows(data []byte) ([]Window, error) {
	var osWindows []rawOSWindow
	if err := json.Unmarshal(data, &osWindows); err != nil {
		return nil, err
	}

	var windows []Window
	for _, osw := range osWindows {
		for _, tab := range osw.Tabs {
			for _, w := range tab.Windows {
				window := Window{
					ID:          w.ID,
					Title:       w.Title,
					CWD:         w.CWD,
					UserVars:    w.UserVars,
					SessionName: w.SessionName,
					AtPrompt:    w.AtPrompt,
				}
				if w.CreatedAt > 0 {
					window.CreatedAt = time.Unix(0, w.CreatedAt)
				}
				windows = append(windows, window)
			}
		}
	}
	return windows, nil
}

// parseAgents keeps the windows that published an AGENT_DISPLAY value. It never
// touches git and never sorts; the picker does both once for every host.
func parseAgents(data []byte) ([]agent.Agent, error) {
	windows, err := parseWindows(data)
	if err != nil {
		return nil, err
	}

	var agents []agent.Agent
	for _, w := range windows {
		display := w.UserVars["AGENT_DISPLAY"]
		if display == "" {
			continue
		}
		a := agent.Agent{
			ID:        w.ID,
			Host:      agent.HostKitty,
			Kind:      w.UserVars["AGENT_KIND"],
			Display:   display,
			Title:     w.Title,
			CWD:       w.CWD,
			Msg:       w.UserVars["AGENT_MSG"],
			CreatedAt: w.CreatedAt,
		}
		if raw := w.UserVars["AGENT_SINCE"]; raw != "" {
			if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
				a.Since = time.Unix(secs, 0)
			}
		}
		agents = append(agents, a)
	}
	return agents, nil
}
