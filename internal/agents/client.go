// Package agents is the picker's view of every host at once: it lists kitty
// windows and tmux panes as one inventory, and reaches whichever agent the user
// picked.
package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/alexander-akhmetov/cattery/internal/agent"
	"github.com/alexander-akhmetov/cattery/internal/kitty"
	"github.com/alexander-akhmetov/cattery/internal/tmux"
)

// varView marks a kitty window holding a read-only view of a tmux agent. Its
// value is the tmux target, so a second Enter on the same agent finds the tab
// that already shows it.
const varView = "AGENT_VIEW"

// varSeen is how the picker tells the kitty watcher that the user has looked at
// an agent. The watcher keeps its seen windows in its own process, where
// nothing outside kitty can reach them, so this is the way in. The watcher
// clears the variable once it has read it.
const varSeen = "AGENT_SEEN"

// KittyClient is the kitty remote control this package needs.
type KittyClient interface {
	ListAgents(ctx context.Context) ([]agent.Agent, error)
	FocusWindow(ctx context.Context, id int) error
	Windows(ctx context.Context) ([]kitty.Window, error)
	Launch(ctx context.Context, args []string) error
	Text(ctx context.Context, id int) (string, error)
	SendText(ctx context.Context, id int, text string) error
	SetUserVars(ctx context.Context, id int, vars []string) error
}

// TmuxClient is the tmux half.
type TmuxClient interface {
	ListAgents(ctx context.Context) ([]agent.Agent, error)
	Alive(ctx context.Context, target string) (bool, error)
	Capture(ctx context.Context, target string) (string, error)
	SendKeys(ctx context.Context, target, data string) error
	MarkSeen(ctx context.Context, target string) error
}

// Client lists and reaches agents in every host.
type Client struct {
	kitty KittyClient
	tmux  TmuxClient

	// repos is shared by both hosts, so a kitty window and a tmux pane in one
	// directory cost a single git lookup between them.
	repos *agent.Resolver

	// exe is the cattery binary a viewer tab runs. The picker is that binary,
	// so a copy installed anywhere still finds itself.
	exe string

	// now is the clock the stalled rule reads. A field, so a test can fix it.
	now func() time.Time
}

// NewClient wires the composite around a kitty client the caller keeps: save
// and restore need the same one.
func NewClient(k *kitty.Client) *Client {
	return &Client{
		kitty: k,
		tmux:  tmux.NewClient(),
		repos: agent.NewResolver(),
		exe:   executable(),
		now:   time.Now,
	}
}

func executable() string {
	if path, err := os.Executable(); err == nil {
		return path
	}
	return "cattery"
}

// ListAgents merges the hosts into one sorted inventory.
//
// The two listers run at once, because each spawns a process. A host that fails
// does not hide the other's agents: its rows are missing and the error explains
// why, which the picker shows as a warning above the list it does have.
func (c *Client) ListAgents(ctx context.Context) ([]agent.Agent, error) {
	var (
		fromKitty, fromTmux []agent.Agent
		kittyErr, tmuxErr   error
		wg                  sync.WaitGroup
	)
	wg.Go(func() { fromKitty, kittyErr = c.kitty.ListAgents(ctx) })
	wg.Go(func() { fromTmux, tmuxErr = c.tmux.ListAgents(ctx) })
	wg.Wait()

	merged := make([]agent.Agent, 0, len(fromKitty)+len(fromTmux))
	merged = append(merged, fromKitty...)
	merged = append(merged, fromTmux...)
	// One repository pass over the whole set, then one sort, so agents of both
	// hosts land in the same project groups.
	c.repos.Populate(ctx, merged)
	c.markStalled(merged)
	agent.Sort(merged)

	if err := errors.Join(wrap("kitty", kittyErr), wrap("tmux", tmuxErr)); err != nil {
		return merged, err
	}
	return merged, nil
}

// markStalled promotes a working agent whose tool has outlived the threshold.
//
// Idempotent: an agent the kitty watcher has already moved to "stalled" is not
// working any more and is left alone. Deriving it here as well is what shows a
// stalled tmux pane, which has no watcher at all, and a stalled kitty window in
// a kitty that has not run `cattery setup` since this version.
func (c *Client) markStalled(agents []agent.Agent) {
	now := c.now()
	for i := range agents {
		if agent.Stalled(agents[i], now) {
			agents[i].Display = "stalled"
		}
	}
}

// wrap names the host a failure came from. The picker draws the message with
// nothing around it, and "no server running" alone says nothing about which
// half of the list is missing.
func wrap(host string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", host, err)
}

// Focus brings the user to an agent: a kitty window is focused, a tmux pane is
// shown read-only in a kitty tab of its own.
//
// A tmux agent gets one viewer tab at a time. Enter on an agent already open
// focuses that tab instead of attaching twice, the same guard `cattery restore`
// applies to a session file.
func (c *Client) Focus(ctx context.Context, a agent.Agent) error {
	if a.Host != agent.HostTmux {
		return c.kitty.FocusWindow(ctx, a.ID)
	}
	if a.Target == "" {
		return fmt.Errorf("tmux agent %d has no window to attach to", a.ID)
	}
	windows, err := c.kitty.Windows(ctx)
	if err != nil {
		return err
	}
	for _, w := range windows {
		if w.UserVars[varView] == a.Target {
			return c.kitty.FocusWindow(ctx, w.ID)
		}
	}
	// A pane can die between the reload that listed it and this keypress. The
	// picker reports that here, rather than in a viewer tab that closes itself.
	alive, err := c.tmux.Alive(ctx, a.Target)
	if err != nil {
		return err
	}
	if !alive {
		return fmt.Errorf("tmux pane %s is gone", a.Target)
	}
	return c.kitty.Launch(ctx, viewerArgs(c.exe, a.Target))
}

// Preview returns the screen one agent is showing, for the picker's sidebar.
// It reads and changes nothing: unlike Focus, it leaves the user where they
// are, and it reaches an unfocused window and a detached pane alike.
func (c *Client) Preview(ctx context.Context, a agent.Agent) (string, error) {
	if a.Host != agent.HostTmux {
		return c.kitty.Text(ctx, a.ID)
	}
	if a.Target == "" {
		return "", fmt.Errorf("tmux agent %d has no pane to read", a.ID)
	}
	return c.tmux.Capture(ctx, a.Target)
}

// Send types raw terminal input at one agent, the bytes a terminal would have
// delivered for those keys. It is what the read-write preview forwards through,
// and the only thing in the picker that changes what an agent is doing.
//
// Neither host reports a send that went nowhere. kitty documents send-text as
// always succeeding, even when its match found no window, and tmux delivers to
// a pane in copy mode without the program ever seeing it. The caller cannot
// treat a nil error as "the agent received this".
func (c *Client) Send(ctx context.Context, a agent.Agent, data string) error {
	if a.Host != agent.HostTmux {
		return c.kitty.SendText(ctx, a.ID, data)
	}
	if a.Target == "" {
		return fmt.Errorf("tmux agent %d has no pane to type into", a.ID)
	}
	return c.tmux.SendKeys(ctx, a.Target, data)
}

// MarkSeen records that the user has looked at an agent, which drops its "done"
// marker. Typing at one counts, the same way jumping to it does.
//
// The kitty half goes through the watcher rather than around it: the set of
// seen windows lives in the watcher's own process, so the picker publishes a
// user variable and the watcher picks it up, clears it, and redraws the tab.
func (c *Client) MarkSeen(ctx context.Context, a agent.Agent) error {
	if a.Host != agent.HostTmux {
		return c.kitty.SetUserVars(ctx, a.ID, []string{varSeen + "=1"})
	}
	if a.Target == "" {
		return fmt.Errorf("tmux agent %d has no pane to mark", a.ID)
	}
	return c.tmux.MarkSeen(ctx, a.Target)
}

// viewerArgs is the kitty tab that shows one tmux agent. The title says the
// view is read-only, and AGENT_VIEW is what the duplicate guard matches on.
func viewerArgs(exe, target string) []string {
	return []string{
		"--type=tab",
		"--title", "ro " + target,
		"--var", varView + "=" + target,
		"--", exe, "attach", target,
	}
}
