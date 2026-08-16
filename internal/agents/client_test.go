package agents

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexander-akhmetov/cattery/internal/agent"
	"github.com/alexander-akhmetov/cattery/internal/kitty"
)

type fakeKitty struct {
	agents  []agent.Agent
	err     error
	windows []kitty.Window
	winErr  error

	// started closes once ListAgents is running, and wait blocks it there. The
	// pair is how a test proves the two listers overlap.
	started chan struct{}
	wait    chan struct{}

	focused  []int
	focusErr error
	launched [][]string

	// read records the window ids Text was asked for, and screen is what it
	// answers with.
	read    []int
	screen  string
	textErr error

	// sent records what Send typed, per window, and vars records the user
	// variables the seen marker published.
	sent    []string
	sendErr error
	vars    []string
}

func (f *fakeKitty) ListAgents(context.Context) ([]agent.Agent, error) {
	if f.started != nil {
		close(f.started)
	}
	if f.wait != nil {
		<-f.wait
	}
	return f.agents, f.err
}

func (f *fakeKitty) FocusWindow(_ context.Context, id int) error {
	f.focused = append(f.focused, id)
	return f.focusErr
}

func (f *fakeKitty) Windows(context.Context) ([]kitty.Window, error) {
	return f.windows, f.winErr
}

func (f *fakeKitty) Launch(_ context.Context, args []string) error {
	f.launched = append(f.launched, args)
	return nil
}

func (f *fakeKitty) Text(_ context.Context, id int) (string, error) {
	f.read = append(f.read, id)
	return f.screen, f.textErr
}

func (f *fakeKitty) SendText(_ context.Context, id int, text string) error {
	f.sent = append(f.sent, fmt.Sprintf("%d:%q", id, text))
	return f.sendErr
}

func (f *fakeKitty) SetUserVars(_ context.Context, id int, vars []string) error {
	f.vars = append(f.vars, fmt.Sprintf("%d:%s", id, strings.Join(vars, " ")))
	return nil
}

type fakeTmux struct {
	agents  []agent.Agent
	err     error
	started chan struct{}
	wait    chan struct{}

	// dead names the targets Alive answers "gone" for, and aliveErr is tmux
	// failing to answer at all.
	dead     []string
	aliveErr error
	asked    []string

	// captured records the targets Capture was asked for, and screen is what it
	// answers with.
	captured   []string
	screen     string
	captureErr error

	// sent records what SendKeys typed, per target, and marked the targets
	// MarkSeen was called for.
	sent    []string
	sendErr error
	marked  []string
}

func (f *fakeTmux) ListAgents(context.Context) ([]agent.Agent, error) {
	if f.started != nil {
		close(f.started)
	}
	if f.wait != nil {
		<-f.wait
	}
	return f.agents, f.err
}

func (f *fakeTmux) Alive(_ context.Context, target string) (bool, error) {
	f.asked = append(f.asked, target)
	return !slices.Contains(f.dead, target), f.aliveErr
}

func (f *fakeTmux) Capture(_ context.Context, target string) (string, error) {
	f.captured = append(f.captured, target)
	return f.screen, f.captureErr
}

func (f *fakeTmux) SendKeys(_ context.Context, target, data string) error {
	f.sent = append(f.sent, fmt.Sprintf("%s:%q", target, data))
	return f.sendErr
}

func (f *fakeTmux) MarkSeen(_ context.Context, target string) error {
	f.marked = append(f.marked, target)
	return nil
}

func newTestClient(k *fakeKitty, tm *fakeTmux) *Client {
	return &Client{kitty: k, tmux: tm, repos: agent.NewResolver(), exe: "/usr/local/bin/cattery", now: time.Now}
}

func TestListAgents(t *testing.T) {
	// One repository, reached from a kitty window and from a tmux pane. Both
	// rows have to come back in the same group, which is what says the
	// repository pass ran over the merged set rather than per host.
	t.Run("both hosts land in one sorted inventory", func(t *testing.T) {
		repo := initRepo(t)
		client := newTestClient(
			&fakeKitty{agents: []agent.Agent{
				{ID: 4, Host: agent.HostKitty, Display: "idle", CWD: repo},
				{ID: 2, Host: agent.HostKitty, Display: "working", CWD: repo},
			}},
			&fakeTmux{agents: []agent.Agent{
				{ID: 17, Host: agent.HostTmux, Display: "working", CWD: repo, Target: "kontora:3.%17"},
			}},
		)

		got, err := client.ListAgents(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		keys := make([]string, 0, len(got))
		for _, a := range got {
			keys = append(keys, a.Key())
			if a.Project != filepath.Base(repo) {
				t.Errorf("%s project: got %q, want %q", a.Key(), a.Project, filepath.Base(repo))
			}
			if a.Branch != "main" {
				t.Errorf("%s branch: got %q, want main", a.Key(), a.Branch)
			}
		}
		// Sorted once, over everything: inside one project the order is by id.
		if want := []string{"kitty:2", "kitty:4", "tmux:%17"}; !slices.Equal(keys, want) {
			t.Fatalf("order: got %v, want %v", keys, want)
		}
	})

	// Derived here rather than published: nothing fires while a tool hangs, so
	// a state the writer set would never arrive. Doing it in the merge covers
	// both hosts, including tmux, which has no watcher at all.
	t.Run("a tool that has run too long reads as stalled", func(t *testing.T) {
		now := time.Unix(1700000000, 0)
		ago := func(d time.Duration) time.Time { return now.Add(-d) }
		client := newTestClient(
			&fakeKitty{agents: []agent.Agent{
				{ID: 1, Host: agent.HostKitty, Display: "working", Tool: "bash: sleep 900", ToolSince: ago(11 * time.Minute)},
				{ID: 2, Host: agent.HostKitty, Display: "working", Tool: "bash: go test", ToolSince: ago(time.Minute)},
				// A Claude agent publishes no tool, so it never gets here.
				{ID: 3, Host: agent.HostKitty, Display: "working"},
			}},
			&fakeTmux{agents: []agent.Agent{
				{ID: 17, Host: agent.HostTmux, Display: "working", Tool: "bash: sleep 900", ToolSince: ago(time.Hour)},
			}},
		)
		client.now = func() time.Time { return now }

		got, err := client.ListAgents(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		displays := map[string]string{}
		for _, a := range got {
			displays[a.Key()] = a.Display
		}
		want := map[string]string{
			"kitty:1": "stalled", "kitty:2": "working", "kitty:3": "working", "tmux:%17": "stalled",
		}
		if !maps.Equal(displays, want) {
			t.Fatalf("displays: got %v, want %v", displays, want)
		}
	})

	// Each lister spawns a process, so they run at once. Both block until the
	// other has started; a sequential implementation deadlocks and the test
	// times out.
	t.Run("the listers run concurrently", func(t *testing.T) {
		k := &fakeKitty{started: make(chan struct{}), wait: make(chan struct{})}
		tm := &fakeTmux{started: make(chan struct{}), wait: make(chan struct{})}
		go func() {
			<-k.started
			<-tm.started
			close(k.wait)
			close(tm.wait)
		}()

		if _, err := newTestClient(k, tm).ListAgents(context.Background()); err != nil {
			t.Fatalf("list: %v", err)
		}
	})

	cases := []struct {
		name     string
		kittyErr error
		tmuxErr  error
		wantKeys []string
		wantErr  string
	}{
		{
			name:     "a tmux failure keeps the kitty rows",
			tmuxErr:  errors.New("unknown option -Q"),
			wantKeys: []string{"kitty:2"},
			wantErr:  "tmux: unknown option -Q",
		},
		{
			name:     "a kitty failure keeps the tmux rows",
			kittyErr: errors.New("no listening socket"),
			wantKeys: []string{"tmux:%17"},
			wantErr:  "kitty: no listening socket",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := &fakeKitty{err: tc.kittyErr}
			if tc.kittyErr == nil {
				k.agents = []agent.Agent{{ID: 2, Host: agent.HostKitty, Display: "working"}}
			}
			tm := &fakeTmux{err: tc.tmuxErr}
			if tc.tmuxErr == nil {
				tm.agents = []agent.Agent{{ID: 17, Host: agent.HostTmux, Display: "working", Target: "kontora:3.%17"}}
			}

			got, err := newTestClient(k, tm).ListAgents(context.Background())

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error: got %v, want one naming %q", err, tc.wantErr)
			}
			var keys []string
			for _, a := range got {
				keys = append(keys, a.Key())
			}
			if !slices.Equal(keys, tc.wantKeys) {
				t.Fatalf("rows: got %v, want %v", keys, tc.wantKeys)
			}
		})
	}
}

func TestFocus(t *testing.T) {
	tmuxAgent := agent.Agent{ID: 17, Host: agent.HostTmux, Display: "working", Target: "kontora:3.%17"}

	t.Run("a kitty agent is focused by window id", func(t *testing.T) {
		k := &fakeKitty{}
		client := newTestClient(k, &fakeTmux{})

		if err := client.Focus(context.Background(), agent.Agent{ID: 12, Host: agent.HostKitty}); err != nil {
			t.Fatalf("focus: %v", err)
		}
		if !slices.Equal(k.focused, []int{12}) {
			t.Fatalf("focused: got %v, want [12]", k.focused)
		}
		if len(k.launched) != 0 {
			t.Fatalf("launched %v for a kitty agent", k.launched)
		}
	})

	t.Run("a tmux agent opens a read-only viewer tab", func(t *testing.T) {
		k := &fakeKitty{}
		client := newTestClient(k, &fakeTmux{})

		if err := client.Focus(context.Background(), tmuxAgent); err != nil {
			t.Fatalf("focus: %v", err)
		}
		want := []string{
			"--type=tab",
			"--title", "ro kontora:3.%17",
			"--var", "AGENT_VIEW=kontora:3.%17",
			"--", "/usr/local/bin/cattery", "attach", "kontora:3.%17",
		}
		if len(k.launched) != 1 || !slices.Equal(k.launched[0], want) {
			t.Fatalf("launched:\n got %v\nwant %v", k.launched, want)
		}
		if len(k.focused) != 0 {
			t.Fatalf("focused %v instead of launching a viewer", k.focused)
		}
	})

	// A second Enter on one agent would otherwise attach twice, the way a
	// second goto_session would rebuild every tab.
	t.Run("an agent already open focuses its viewer tab", func(t *testing.T) {
		k := &fakeKitty{windows: []kitty.Window{
			// The second agent of a split window: same window, another pane, and
			// its own viewer tab.
			{ID: 5, UserVars: map[string]string{"AGENT_VIEW": "kontora:3.%18"}},
			{ID: 6, UserVars: map[string]string{"AGENT_VIEW": "kontora:3.%17"}},
			{ID: 7},
		}}
		client := newTestClient(k, &fakeTmux{})

		if err := client.Focus(context.Background(), tmuxAgent); err != nil {
			t.Fatalf("focus: %v", err)
		}
		if !slices.Equal(k.focused, []int{6}) {
			t.Fatalf("focused: got %v, want [6]", k.focused)
		}
		if len(k.launched) != 0 {
			t.Fatalf("launched a second viewer: %v", k.launched)
		}
	})

	// Without the window inventory the duplicate guard cannot answer, and
	// guessing would leave the user with two viewers of one agent.
	t.Run("an unreadable window list stops the attach", func(t *testing.T) {
		k := &fakeKitty{winErr: errors.New("no listening socket")}
		client := newTestClient(k, &fakeTmux{})

		err := client.Focus(context.Background(), tmuxAgent)

		if err == nil || !strings.Contains(err.Error(), "no listening socket") {
			t.Fatalf("error: got %v, want kitty's reason", err)
		}
		if len(k.launched) != 0 {
			t.Fatalf("launched a viewer anyway: %v", k.launched)
		}
	})

	// A pane can die between the reload that listed it and the keypress. The
	// viewer tab would open, fail, and be closed by kitty too fast to read.
	t.Run("a pane that went away is reported instead of opening a tab", func(t *testing.T) {
		k := &fakeKitty{}
		tm := &fakeTmux{dead: []string{tmuxAgent.Target}}
		client := newTestClient(k, tm)

		err := client.Focus(context.Background(), tmuxAgent)

		if err == nil || !strings.Contains(err.Error(), tmuxAgent.Target) {
			t.Fatalf("error: got %v, want one naming the pane", err)
		}
		if len(k.launched) != 0 {
			t.Fatalf("launched a viewer for a dead pane: %v", k.launched)
		}
	})

	t.Run("a tmux that cannot answer stops the attach", func(t *testing.T) {
		k := &fakeKitty{}
		tm := &fakeTmux{aliveErr: errors.New("no server running")}
		client := newTestClient(k, tm)

		err := client.Focus(context.Background(), tmuxAgent)

		if err == nil || !strings.Contains(err.Error(), "no server running") {
			t.Fatalf("error: got %v, want tmux's reason", err)
		}
		if len(k.launched) != 0 {
			t.Fatalf("launched a viewer anyway: %v", k.launched)
		}
	})

	t.Run("a tmux agent with no target cannot be reached", func(t *testing.T) {
		k := &fakeKitty{}
		client := newTestClient(k, &fakeTmux{})

		if err := client.Focus(context.Background(), agent.Agent{ID: 17, Host: agent.HostTmux}); err == nil {
			t.Fatal("expected an error")
		}
		if len(k.launched) != 0 || len(k.focused) != 0 {
			t.Fatalf("reached kitty anyway: launched=%v focused=%v", k.launched, k.focused)
		}
	})
}

// Preview reaches the host the agent runs in and nothing else. A kitty window
// is read by id; a tmux pane is read by its target, which is what carries the
// pane id.
func TestPreview(t *testing.T) {
	cases := []struct {
		name        string
		a           agent.Agent
		wantRead    []int
		wantCapture []string
		wantErr     string
	}{
		{
			name:     "a kitty agent is read by window id",
			a:        agent.Agent{ID: 12, Host: agent.HostKitty},
			wantRead: []int{12},
		},
		{
			// An agent listed before internal/kitty grew a Host defaults to
			// kitty, the same way Key() does.
			name:     "an agent with no host reads as kitty",
			a:        agent.Agent{ID: 5},
			wantRead: []int{5},
		},
		{
			name:        "a tmux agent is read by target",
			a:           agent.Agent{ID: 17, Host: agent.HostTmux, Target: "kontora:3.%17"},
			wantCapture: []string{"kontora:3.%17"},
		},
		{
			name:    "a tmux agent with no target reaches neither host",
			a:       agent.Agent{ID: 17, Host: agent.HostTmux},
			wantErr: "no pane to read",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := &fakeKitty{screen: "kitty screen"}
			tm := &fakeTmux{screen: "tmux screen"}
			client := newTestClient(k, tm)

			screen, err := client.Preview(context.Background(), tc.a)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error: got %v, want one containing %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("preview: %v", err)
			}
			if !slices.Equal(k.read, tc.wantRead) {
				t.Errorf("kitty read: got %v, want %v", k.read, tc.wantRead)
			}
			if !slices.Equal(tm.captured, tc.wantCapture) {
				t.Errorf("tmux captured: got %v, want %v", tm.captured, tc.wantCapture)
			}
			if len(tc.wantRead) > 0 && screen != k.screen {
				t.Errorf("screen: got %q, want %q", screen, k.screen)
			}
			if len(tc.wantCapture) > 0 && screen != tm.screen {
				t.Errorf("screen: got %q, want %q", screen, tm.screen)
			}
		})
	}
}

func TestSend(t *testing.T) {
	cases := []struct {
		name      string
		a         agent.Agent
		wantKitty []string
		wantTmux  []string
		wantErr   string
	}{
		{
			name:      "a kitty agent is typed at by window id",
			a:         agent.Agent{ID: 12, Host: agent.HostKitty},
			wantKitty: []string{`12:"\x1b[A"`},
		},
		{
			name:      "an agent with no host reads as kitty",
			a:         agent.Agent{ID: 5},
			wantKitty: []string{`5:"\x1b[A"`},
		},
		{
			name:     "a tmux agent is typed at by target",
			a:        agent.Agent{ID: 17, Host: agent.HostTmux, Target: "kontora:3.%17"},
			wantTmux: []string{`kontora:3.%17:"\x1b[A"`},
		},
		{
			name:    "a tmux agent with no target reaches neither host",
			a:       agent.Agent{ID: 17, Host: agent.HostTmux},
			wantErr: "no pane to type into",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, tm := &fakeKitty{}, &fakeTmux{}

			err := newTestClient(k, tm).Send(context.Background(), tc.a, "\x1b[A")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error: got %v, want one containing %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("send: %v", err)
			}
			if !slices.Equal(k.sent, tc.wantKitty) {
				t.Errorf("kitty sent: got %v, want %v", k.sent, tc.wantKitty)
			}
			if !slices.Equal(tm.sent, tc.wantTmux) {
				t.Errorf("tmux sent: got %v, want %v", tm.sent, tc.wantTmux)
			}
		})
	}
}

// The kitty half cannot write the marker itself: the set of seen windows lives
// inside the watcher's process. It publishes a user variable and lets the
// watcher pick it up.
func TestMarkSeen(t *testing.T) {
	t.Run("a kitty agent is marked through a user variable", func(t *testing.T) {
		k, tm := &fakeKitty{}, &fakeTmux{}

		if err := newTestClient(k, tm).MarkSeen(context.Background(), agent.Agent{ID: 12, Host: agent.HostKitty}); err != nil {
			t.Fatalf("mark seen: %v", err)
		}
		if want := []string{"12:AGENT_SEEN=1"}; !slices.Equal(k.vars, want) {
			t.Fatalf("vars: got %v, want %v", k.vars, want)
		}
		if len(tm.marked) != 0 {
			t.Fatalf("marked %v on the wrong host", tm.marked)
		}
	})

	t.Run("a tmux agent is marked on its pane", func(t *testing.T) {
		k, tm := &fakeKitty{}, &fakeTmux{}
		a := agent.Agent{ID: 17, Host: agent.HostTmux, Target: "kontora:3.%17"}

		if err := newTestClient(k, tm).MarkSeen(context.Background(), a); err != nil {
			t.Fatalf("mark seen: %v", err)
		}
		if want := []string{"kontora:3.%17"}; !slices.Equal(tm.marked, want) {
			t.Fatalf("marked: got %v, want %v", tm.marked, want)
		}
		if len(k.vars) != 0 {
			t.Fatalf("published %v on the wrong host", k.vars)
		}
	})

	t.Run("a tmux agent with no target reaches neither host", func(t *testing.T) {
		err := newTestClient(&fakeKitty{}, &fakeTmux{}).MarkSeen(context.Background(), agent.Agent{ID: 17, Host: agent.HostTmux})
		if err == nil || !strings.Contains(err.Error(), "no pane to mark") {
			t.Fatalf("error: got %v", err)
		}
	})
}

func initRepo(t *testing.T) string {
	t.Helper()
	// t.TempDir() is a symlinked /var path on macOS; git reports the resolved
	// one, so resolve up front or the project comparison fails.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
	return dir
}
