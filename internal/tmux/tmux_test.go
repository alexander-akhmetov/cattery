package tmux

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alexander-akhmetov/cattery/internal/agent"
)

// The same table as DeriveDisplayTest in tests/cattery_watcher_test.py, with
// the watcher's window bookkeeping written as the two pane options that stand
// in for it. Two implementations of one rule drift silently otherwise.
func TestDisplay(t *testing.T) {
	cases := []struct {
		name   string
		state  string
		worked string
		seen   string
		want   string
	}{
		{name: "working", state: "working", want: "working"},
		{name: "working ignores seen", state: "working", worked: "1", seen: "1", want: "working"},
		{name: "blocked", state: "blocked", want: "blocked"},
		{name: "blocked ignores seen", state: "blocked", worked: "1", seen: "1", want: "blocked"},
		{name: "finished unseen", state: "idle", worked: "1", want: "done"},
		{name: "already acknowledged", state: "idle", worked: "1", seen: "1", want: "idle"},
		{name: "idle before any work", state: "idle", want: "idle"},
		{name: "idle before any work, acknowledged", state: "idle", seen: "1", want: "idle"},
		{name: "no state", want: ""},
		{name: "unknown state", state: "thinking", worked: "1", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := display(tc.state, tc.worked, tc.seen); got != tc.want {
				t.Fatalf("display(%q, %q, %q): got %q, want %q", tc.state, tc.worked, tc.seen, got, tc.want)
			}
		})
	}
}

// pane builds one list-panes row the way listFormat prints it.
func pane(id, session, index, name, path, title, kind, state, msg, since, worked, seen string) string {
	return strings.Join([]string{id, session, index, name, path, title, kind, state, msg, since, worked, seen}, fieldSep)
}

// A kontora agent: one detached window per ticket, named after the ticket, with
// the worktree as its directory.
const kontoraPane = "%17\x1fkontora\x1f3\x1fal-67je\x1f/Users/x/.kontora/worktrees/astra-l/al-67je" +
	"\x1f◐ Run /code-review\x1fclaude\x1fworking\x1frun the review\x1f1700000000\x1f1\x1f"

func TestListAgents(t *testing.T) {
	t.Run("a kontora agent", func(t *testing.T) {
		client := &Client{tmux: fakeTmux(t, "cat <<'EOF'\n"+kontoraPane+"\nEOF")}

		agents, err := client.ListAgents(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(agents) != 1 {
			t.Fatalf("got %d agents, want 1: %+v", len(agents), agents)
		}
		want := agent.Agent{
			ID: 17, Host: agent.HostTmux, Kind: "claude", Display: "working",
			Title:  "◐ Run /code-review",
			CWD:    "/Users/x/.kontora/worktrees/astra-l/al-67je",
			Msg:    "run the review",
			Since:  time.Unix(1700000000, 0),
			Target: "kontora:3.%17",
		}
		if agents[0] != want {
			t.Fatalf("agent:\n got %+v\nwant %+v", agents[0], want)
		}
		if got := agents[0].Key(); got != "tmux:%17" {
			t.Errorf("key: got %q, want tmux:%%17", got)
		}
	})

	t.Run("only panes publishing a state", func(t *testing.T) {
		rows := strings.Join([]string{
			kontoraPane,
			// A plain shell: every agent option empty.
			pane("%2", "work", "0", "zsh", "/Users/x", "zsh", "", "", "", "", "", ""),
			// A kind with no state is not an agent either: the writer clears the
			// state first, and the tag can outlive it.
			pane("%3", "work", "1", "zsh", "/Users/x", "zsh", "pi", "", "", "", "1", ""),
			// A word this picker does not know.
			pane("%4", "work", "2", "zsh", "/Users/x", "zsh", "pi", "thinking", "", "", "", ""),
		}, "\n")
		client := &Client{tmux: fakeTmux(t, "cat <<'EOF'\n"+rows+"\nEOF")}

		agents, err := client.ListAgents(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(agents) != 1 || agents[0].ID != 17 {
			t.Fatalf("got %+v, want only pane %%17", agents)
		}
	})

	t.Run("display states", func(t *testing.T) {
		rows := strings.Join([]string{
			pane("%1", "work", "0", "w", "/p", "t", "pi", "working", "", "", "1", ""),
			pane("%2", "work", "1", "w", "/p", "t", "pi", "blocked", "", "", "1", ""),
			pane("%3", "work", "2", "w", "/p", "t", "pi", "idle", "", "", "1", ""),
			pane("%4", "work", "3", "w", "/p", "t", "pi", "idle", "", "", "1", "1"),
			pane("%5", "work", "4", "w", "/p", "t", "pi", "idle", "", "", "", ""),
		}, "\n")
		client := &Client{tmux: fakeTmux(t, "cat <<'EOF'\n"+rows+"\nEOF")}

		agents, err := client.ListAgents(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		got := make([]string, 0, len(agents))
		for _, a := range agents {
			got = append(got, a.Display)
		}
		want := []string{"working", "blocked", "done", "idle", "idle"}
		if !slices.Equal(got, want) {
			t.Fatalf("displays: got %v, want %v", got, want)
		}
	})

	// A split window holds two agents, and everything the picker does with a
	// target addresses one of them: the "seen" marker the attach writes, and the
	// tab it matches when the same agent is picked twice.
	t.Run("two agents in one window get their own targets", func(t *testing.T) {
		rows := strings.Join([]string{
			pane("%17", "work", "1", "w", "/p", "left", "pi", "working", "", "", "1", ""),
			pane("%18", "work", "1", "w", "/p", "right", "claude", "blocked", "", "", "1", ""),
		}, "\n")
		client := &Client{tmux: fakeTmux(t, "cat <<'EOF'\n"+rows+"\nEOF")}

		agents, err := client.ListAgents(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		got := make([]string, 0, len(agents))
		for _, a := range agents {
			got = append(got, a.Target)
		}
		if want := []string{"work:1.%17", "work:1.%18"}; !slices.Equal(got, want) {
			t.Fatalf("targets: got %v, want %v", got, want)
		}
	})

	// A grouped session mirrors the windows of the session it was made from, so
	// list-panes reports one pane once per session.
	t.Run("a pane seen through a viewer is listed once", func(t *testing.T) {
		viewer := pane("%17", viewPrefix+"kontora-991", "3", "al-67je",
			"/Users/x/.kontora/worktrees/astra-l/al-67je", "◐ Run /code-review",
			"claude", "working", "run the review", "1700000000", "1", "")
		for _, order := range []struct {
			name string
			rows []string
		}{
			{name: "owner first", rows: []string{kontoraPane, viewer}},
			{name: "viewer first", rows: []string{viewer, kontoraPane}},
		} {
			t.Run(order.name, func(t *testing.T) {
				client := &Client{tmux: fakeTmux(t, "cat <<'EOF'\n"+strings.Join(order.rows, "\n")+"\nEOF")}

				agents, err := client.ListAgents(context.Background())
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				if len(agents) != 1 {
					t.Fatalf("got %d agents, want 1: %+v", len(agents), agents)
				}
				if agents[0].Target != "kontora:3.%17" {
					t.Errorf("target: got %q, want kontora:3.%%17", agents[0].Target)
				}
			})
		}
	})

	// The owning session is gone, so nothing names this pane in a way that
	// outlives the viewer.
	t.Run("a pane only a viewer can see is dropped", func(t *testing.T) {
		row := pane("%17", viewPrefix+"kontora-991", "3", "al-67je", "/p", "t", "claude", "working", "", "", "1", "")
		client := &Client{tmux: fakeTmux(t, "cat <<'EOF'\n"+row+"\nEOF")}

		agents, err := client.ListAgents(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(agents) != 0 {
			t.Fatalf("got %+v, want none", agents)
		}
	})

	t.Run("the query tmux runs", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "argv")
		client := &Client{tmux: fakeTmux(t, `printf '%s\n' "$@" > `+log)}

		if _, err := client.ListAgents(context.Background()); err != nil {
			t.Fatalf("list: %v", err)
		}
		args := readLines(t, log)
		want := []string{"list-panes", "-a", "-F", listFormat}
		if !slices.Equal(args, want) {
			t.Fatalf("argv: got %v, want %v", args, want)
		}
		for _, option := range []string{optKind, optState, optMsg, optSince, optWorked, optSeen} {
			if !strings.Contains(listFormat, "#{"+option+"}") {
				t.Errorf("format does not ask for %s: %s", option, listFormat)
			}
		}
	})
}

// A machine with no tmux running, or none installed, has no tmux agents. That
// is not an error: the picker would show a banner over the kitty agents.
func TestListAgentsWithoutAServer(t *testing.T) {
	cases := []struct {
		name string
		tmux string
	}{
		{name: "no server running", tmux: fakeTmux(t, "printf 'no server running on /tmp/tmux-501/default\\n' >&2; exit 1")},
		{name: "error connecting", tmux: fakeTmux(t, "printf 'error connecting to /tmp/tmux-501/default (No such file)\\n' >&2; exit 1")},
		{name: "tmux is not installed", tmux: filepath.Join(t.TempDir(), "absent-tmux")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agents, err := (&Client{tmux: tc.tmux}).ListAgents(context.Background())
			if err != nil {
				t.Fatalf("list: got %v, want nil", err)
			}
			if len(agents) != 0 {
				t.Fatalf("agents: got %+v, want none", agents)
			}
		})
	}
}

// Any other failure is real, and the banner shows tmux's own reason.
func TestListAgentsError(t *testing.T) {
	client := &Client{tmux: fakeTmux(t, "printf 'unknown option\\n-Q\\n' >&2; exit 1")}

	_, err := client.ListAgents(context.Background())

	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); got != "unknown option -Q" {
		t.Fatalf("error: got %q, want tmux's reason on one line", got)
	}
}

func TestSplitTarget(t *testing.T) {
	cases := []struct {
		name    string
		target  string
		session string
		index   string
		pane    string
		wantErr bool
	}{
		{name: "session, window index, pane", target: "kontora:3.%17", session: "kontora", index: "3", pane: "%17"},
		{name: "index zero", target: "kontora:0.%1", session: "kontora", index: "0", pane: "%1"},
		// tmux allows both in a session name and reads its own targets from the
		// end, so this one has to as well.
		{name: "a session name holding a dot and a colon", target: "a.b:c:3.%17", session: "a.b:c", index: "3", pane: "%17"},
		{name: "no pane", target: "kontora:3", wantErr: true},
		{name: "a pane index is not a pane id", target: "kontora:3.1", wantErr: true},
		{name: "empty pane", target: "kontora:3.", wantErr: true},
		{name: "no window", target: "kontora.%17", wantErr: true},
		{name: "empty window", target: "kontora:.%17", wantErr: true},
		{name: "no session", target: ":3.%17", wantErr: true},
		{name: "a window name is not an identity", target: "kontora:al-67je.%17", wantErr: true},
		{name: "empty", target: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session, index, pane, err := splitTarget(tc.target)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("splitTarget(%q): got no error", tc.target)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitTarget(%q): %v", tc.target, err)
			}
			if session != tc.session || index != tc.index || pane != tc.pane {
				t.Fatalf("splitTarget(%q): got %q/%q/%q, want %q/%q/%q",
					tc.target, session, index, pane, tc.session, tc.index, tc.pane)
			}
		})
	}
}

func TestAlive(t *testing.T) {
	cases := []struct {
		name string
		tmux string
		want bool
	}{
		{name: "a live pane", tmux: fakeTmux(t, "printf '%%17\\n'"), want: true},
		{name: "a pane that went away", tmux: fakeTmux(t, `printf "can't find pane: %%17\n" >&2; exit 1`)},
		{name: "no server running", tmux: fakeTmux(t, "printf 'no server running\\n' >&2; exit 1")},
		{name: "tmux is not installed", tmux: filepath.Join(t.TempDir(), "absent-tmux")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alive, err := (&Client{tmux: tc.tmux}).Alive(context.Background(), "kontora:3.%17")
			if err != nil {
				t.Fatalf("alive: %v", err)
			}
			if alive != tc.want {
				t.Fatalf("alive: got %v, want %v", alive, tc.want)
			}
		})
	}

	t.Run("the pane is what tmux is asked about", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "argv")
		client := &Client{tmux: fakeTmux(t, `printf '%s\n' "$*" >> `+log)}

		if _, err := client.Alive(context.Background(), "kontora:3.%17"); err != nil {
			t.Fatalf("alive: %v", err)
		}
		if got, want := readLines(t, log), []string{"list-panes -t %17 -F #{pane_id}"}; !slices.Equal(got, want) {
			t.Fatalf("commands: got %v, want %v", got, want)
		}
	})

	t.Run("a malformed target is a failure, not an answer", func(t *testing.T) {
		if _, err := (&Client{tmux: "tmux"}).Alive(context.Background(), "kontora"); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestViewName(t *testing.T) {
	pid := strconv.Itoa(os.Getpid())
	if got, want := viewName("kontora"), viewPrefix+"kontora-"+pid; got != want {
		t.Fatalf("view name: got %q, want %q", got, want)
	}
	// Both characters separate the parts of a target, and an agent session can
	// hold either.
	if got := viewName("work.1:x"); strings.ContainsAny(got, ".:") {
		t.Fatalf("view name keeps a character tmux rejects: %q", got)
	}
}

func TestAttach(t *testing.T) {
	view := viewName("kontora")

	t.Run("the commands one attachment runs", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "argv")
		client := &Client{tmux: fakeTmux(t, `printf '%s\n' "$*" >> `+log)}

		if err := client.Attach(context.Background(), "kontora:3.%17"); err != nil {
			t.Fatalf("attach: %v", err)
		}

		want := []string{
			// A viewer this pid killed outright left its session behind, and the
			// name would collide.
			"kill-session -t =" + view,
			// Grouped, so the viewer has its own current window.
			"new-session -d -t =kontora -s " + view,
			"select-window -t " + view + ":3",
			// The agent is no longer "done": someone has looked at it. On the
			// pane, because a window target means whichever pane is active.
			"set -p -t %17 " + optSeen + " 1",
			// -r is tmux's read-only,ignore-size pair.
			"attach-session -r -t " + view,
			"kill-session -t =" + view,
		}
		if got := readLines(t, log); !slices.Equal(got, want) {
			t.Fatalf("commands:\n got %v\nwant %v", got, want)
		}
	})

	t.Run("the viewer sees the agent's terminal", func(t *testing.T) {
		// attach-session takes over this process's terminal, so the child has to
		// inherit stdio rather than have it captured.
		out := filepath.Join(t.TempDir(), "stdout")
		client := &Client{tmux: fakeTmux(t, `case "$1" in attach-session) printf attached ;; esac`)}

		stdout := os.Stdout
		file, err := os.Create(out)
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = file
		err = client.Attach(context.Background(), "kontora:3.%17")
		os.Stdout = stdout
		file.Close()
		if err != nil {
			t.Fatalf("attach: %v", err)
		}

		written, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if string(written) != "attached" {
			t.Fatalf("stdout: got %q, want the child's output", written)
		}
	})

	// The view session outlives the process that made it, so an attach that
	// never started must not leave one behind either.
	t.Run("a failed attach still removes the view session", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "argv")
		client := &Client{tmux: fakeTmux(t, `printf '%s\n' "$*" >> `+log+`
case "$1" in attach-session) exit 1 ;; esac`)}

		err := client.Attach(context.Background(), "kontora:3.%17")

		if err == nil {
			t.Fatal("expected the attach failure to surface")
		}
		if !strings.Contains(err.Error(), "kontora:3.%17") {
			t.Errorf("error does not name the target: %v", err)
		}
		if got := readLines(t, log); !slices.Contains(got, "kill-session -t ="+view) {
			t.Fatalf("commands: got %v, want a kill-session among them", got)
		}
	})

	t.Run("a session that cannot be grouped is reported", func(t *testing.T) {
		client := &Client{tmux: fakeTmux(t, `printf "can't find session: kontora\n" >&2; exit 1`)}

		err := client.Attach(context.Background(), "kontora:3.%17")

		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "can't find session") {
			t.Fatalf("error missing tmux's reason: %v", err)
		}
	})

	t.Run("a target that is not a window index runs nothing", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "argv")
		client := &Client{tmux: fakeTmux(t, `printf '%s\n' "$*" >> `+log)}

		if err := client.Attach(context.Background(), "kontora"); err == nil {
			t.Fatal("expected an error")
		}
		if _, err := os.Stat(log); err == nil {
			t.Fatalf("ran tmux for an invalid target: %v", readLines(t, log))
		}
	})

	// kitty sends SIGHUP when the viewer tab closes, which is the ordinary way
	// out of a viewer. Go terminates on it by default, and the grouped session
	// the attach made would stay behind for good.
	t.Run("a signal ends the attach through the cleanup", func(t *testing.T) {
		dir := t.TempDir()
		log := filepath.Join(dir, "argv")
		attached := filepath.Join(dir, "attached")
		// Short sleeps in a loop, not one long one: the signal kills the stub, and
		// a sleep left behind would hold the test's stdout open until it ended.
		client := &Client{tmux: fakeTmux(t, `printf '%s\n' "$*" >> `+log+`
case "$1" in attach-session) : > `+attached+`; while :; do sleep 0.1; done ;; esac`)}

		done := make(chan error, 1)
		go func() { done <- client.Attach(context.Background(), "kontora:3.%17") }()

		waitForFile(t, attached)
		if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatalf("signal: %v", err)
		}

		select {
		case err := <-done:
			if err == nil {
				t.Fatal("expected the killed attach to report an error")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("attach did not return after the signal")
		}
		if got := readLines(t, log); got[len(got)-1] != "kill-session -t ="+view {
			t.Fatalf("commands: got %v, want the view session killed last", got)
		}
	})
}

// waitForFile blocks until the stub tmux reports it reached a command. The
// signal has to arrive while the handler Attach installed is in place: before
// it, the test process dies of SIGHUP.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// fakeTmux writes a stub tmux script running body, and returns its path.
func fakeTmux(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}
