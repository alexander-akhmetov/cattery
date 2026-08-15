package agents

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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

func newTestClient(k *fakeKitty, tm *fakeTmux) *Client {
	return &Client{kitty: k, tmux: tm, repos: agent.NewResolver(), exe: "/usr/local/bin/cattery"}
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
