package tmux

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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

// pane builds one list-panes row the way listFormat prints it. It sizes itself
// from fieldCount, so a field appended to listFormat leaves every case here
// valid rather than one short. The variadic tail fills the row from fTool on:
// the tool pair, then the resume command, the pane pid and the pane command.
func pane(id, session, index, name, path, title, kind, state, msg, since, worked, seen string, rest ...string) string {
	fields := make([]string, fieldCount)
	copy(fields, []string{id, session, index, name, path, title, kind, state, msg, since, worked, seen})
	copy(fields[fTool:], rest)
	return strings.Join(fields, fieldSep)
}

// A field added to listFormat without an entry in the iota block would make
// every row one field too wide, and parseAgents drops a row of the wrong width
// without a word: every tmux agent would leave the picker in silence.
func TestListFormatFieldCount(t *testing.T) {
	if got := strings.Count(listFormat, fieldSep); got != fieldCount-1 {
		t.Fatalf("listFormat has %d separators, want %d: the iota block and the format disagree", got, fieldCount-1)
	}
}

// A dev agent: one detached window per ticket, named after the ticket, with
// the worktree as its directory.
var devPane = pane("%17", "dev", "3", "feat-42", "/Users/x/.worktrees/myapp/feat-42",
	"◐ Run /code-review", "claude", "working", "run the review", "1700000000", "1", "")

func TestListAgents(t *testing.T) {
	t.Run("a dev agent", func(t *testing.T) {
		client := &Client{tmux: fakeTmux(t, "cat <<'EOF'\n"+devPane+"\nEOF")}

		agents, err := client.ListAgents(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(agents) != 1 {
			t.Fatalf("got %d agents, want 1: %+v", len(agents), agents)
		}
		want := agent.Agent{
			ID: 17, Host: agent.HostTmux, Kind: "claude", Display: "working",
			State:  "working",
			Title:  "◐ Run /code-review",
			CWD:    "/Users/x/.worktrees/myapp/feat-42",
			Msg:    "run the review",
			Since:  time.Unix(1700000000, 0),
			Target: "dev:3.%17",
		}
		// Agent carries a slice of processes, so == does not build any more.
		if !reflect.DeepEqual(agents[0], want) {
			t.Fatalf("agent:\n got %+v\nwant %+v", agents[0], want)
		}
		if got := agents[0].Key(); got != "tmux:%17" {
			t.Errorf("key: got %q, want tmux:%%17", got)
		}
	})

	t.Run("the fingerprint fields", func(t *testing.T) {
		row := pane("%17", "dev", "3", "feat-42", "/w", "t", "claude", "idle", "", "", "1", "",
			"", "", "claude --resume abc-123", "86369", "claude")
		client := &Client{tmux: fakeTmux(t, "cat <<'EOF'\n"+row+"\nEOF")}

		agents, err := client.ListAgents(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(agents) != 1 {
			t.Fatalf("got %d agents, want 1: %+v", len(agents), agents)
		}
		a := agents[0]
		if a.Resume != "claude --resume abc-123" || a.PID != 86369 || a.Command != "claude" {
			t.Errorf("fingerprint: got resume=%q pid=%d command=%q", a.Resume, a.PID, a.Command)
		}
		// The agent's own word and cattery's derived one differ here on
		// purpose: it finished while nobody was attached.
		if a.State != "idle" || a.Display != "done" {
			t.Errorf("state/display: got %q / %q, want idle / done", a.State, a.Display)
		}
	})

	t.Run("a pid that is not a number keeps its row", func(t *testing.T) {
		row := pane("%17", "dev", "3", "feat-42", "/w", "t", "claude", "working", "", "", "", "",
			"", "", "", "not-a-pid", "claude")
		client := &Client{tmux: fakeTmux(t, "cat <<'EOF'\n"+row+"\nEOF")}

		agents, err := client.ListAgents(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(agents) != 1 || agents[0].PID != 0 {
			t.Fatalf("got %+v, want one agent with PID 0", agents)
		}
	})

	t.Run("a row of the wrong width is dropped", func(t *testing.T) {
		// What a tmux that answered the old format would print: three fields
		// short. Reading it positionally would put the tool label in @AGENT_MSG.
		narrow := strings.Join(make([]string, fieldCount-3), fieldSep)
		client := &Client{tmux: fakeTmux(t, "cat <<'EOF'\n"+narrow+"\nEOF")}

		agents, err := client.ListAgents(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(agents) != 0 {
			t.Fatalf("got %+v, want no agents", agents)
		}
	})

	t.Run("only panes publishing a state", func(t *testing.T) {
		rows := strings.Join([]string{
			devPane,
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
		viewer := pane("%17", viewPrefix+"dev-991", "3", "feat-42",
			"/Users/x/.worktrees/myapp/feat-42", "◐ Run /code-review",
			"claude", "working", "run the review", "1700000000", "1", "")
		for _, order := range []struct {
			name string
			rows []string
		}{
			{name: "owner first", rows: []string{devPane, viewer}},
			{name: "viewer first", rows: []string{viewer, devPane}},
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
				if agents[0].Target != "dev:3.%17" {
					t.Errorf("target: got %q, want dev:3.%%17", agents[0].Target)
				}
			})
		}
	})

	// The owning session is gone, so nothing names this pane in a way that
	// outlives the viewer.
	t.Run("a pane only a viewer can see is dropped", func(t *testing.T) {
		row := pane("%17", viewPrefix+"dev-991", "3", "feat-42", "/p", "t", "claude", "working", "", "", "1", "")
		client := &Client{tmux: fakeTmux(t, "cat <<'EOF'\n"+row+"\nEOF")}

		agents, err := client.ListAgents(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(agents) != 0 {
			t.Fatalf("got %+v, want none", agents)
		}
	})

	// A pane keeps its options after the agent in it dies, and `cattery state
	// clear` drops only the state, the kind and the message. So the tool a
	// killed pi was running would otherwise pin a label on the next agent, and
	// with it an hours-old timestamp that reads as stalled at once.
	t.Run("the running tool", func(t *testing.T) {
		cases := []struct {
			name      string
			kind      string
			state     string
			tool      string
			since     string
			wantTool  string
			wantSince time.Time
		}{
			{
				name: "a working agent", state: "working",
				tool: "bash: go test ./...", since: "1700000000",
				wantTool: "bash: go test ./...", wantSince: time.Unix(1700000000, 0),
			},
			{name: "an idle agent ignores a stale label", state: "idle", tool: "bash: go test ./...", since: "1700000000"},
			{
				// The pane a killed pi left behind. `cattery state clear` drops
				// the state, the kind and the message and nothing else, so
				// without the kind test the next agent wears the dead pi's
				// label and reads as stalled at once.
				name: "a claude agent in a pane a pi died in", kind: "claude", state: "working",
				tool: "bash: sleep 900", since: "1700000000",
			},
			{name: "no tool published", state: "working"},
			{
				name: "a label with no timestamp", state: "working", tool: "bash: go test ./...",
				wantTool: "bash: go test ./...",
			},
			{name: "a zero timestamp is not a timestamp", state: "working", tool: "x", since: "0", wantTool: "x"},
			{name: "an unparsable timestamp is dropped", state: "working", tool: "x", since: "soon", wantTool: "x"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				kind := tc.kind
				if kind == "" {
					// pi is the only kind that publishes a tool, so every case
					// that is not about the kind is a pi agent.
					kind = "pi"
				}
				row := pane("%1", "work", "0", "w", "/p", "t", kind, tc.state, "", "", "1", "", tc.tool, tc.since)
				client := &Client{tmux: fakeTmux(t, "cat <<'EOF'\n"+row+"\nEOF")}

				agents, err := client.ListAgents(context.Background())
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				if len(agents) != 1 {
					t.Fatalf("got %d agents, want 1", len(agents))
				}
				if got := agents[0].Tool; got != tc.wantTool {
					t.Errorf("tool: got %q, want %q", got, tc.wantTool)
				}
				if got := agents[0].ToolSince; !got.Equal(tc.wantSince) {
					t.Errorf("tool since: got %v, want %v", got, tc.wantSince)
				}
			})
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
		for _, option := range []string{optKind, optState, optMsg, optSince, optWorked, optSeen, optTool, optToolSince} {
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
		{name: "session, window index, pane", target: "dev:3.%17", session: "dev", index: "3", pane: "%17"},
		{name: "index zero", target: "dev:0.%1", session: "dev", index: "0", pane: "%1"},
		// tmux allows both in a session name and reads its own targets from the
		// end, so this one has to as well.
		{name: "a session name holding a dot and a colon", target: "a.b:c:3.%17", session: "a.b:c", index: "3", pane: "%17"},
		{name: "no pane", target: "dev:3", wantErr: true},
		{name: "a pane index is not a pane id", target: "dev:3.1", wantErr: true},
		{name: "empty pane", target: "dev:3.", wantErr: true},
		{name: "no window", target: "dev.%17", wantErr: true},
		{name: "empty window", target: "dev:.%17", wantErr: true},
		{name: "no session", target: ":3.%17", wantErr: true},
		{name: "a window name is not an identity", target: "dev:feat-42.%17", wantErr: true},
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
			alive, err := (&Client{tmux: tc.tmux}).Alive(context.Background(), "dev:3.%17")
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

		if _, err := client.Alive(context.Background(), "dev:3.%17"); err != nil {
			t.Fatalf("alive: %v", err)
		}
		if got, want := readLines(t, log), []string{"list-panes -t %17 -F #{pane_id}"}; !slices.Equal(got, want) {
			t.Fatalf("commands: got %v, want %v", got, want)
		}
	})

	t.Run("a malformed target is a failure, not an answer", func(t *testing.T) {
		if _, err := (&Client{tmux: "tmux"}).Alive(context.Background(), "dev"); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestCapture(t *testing.T) {
	// The pane id alone, as Alive does: a bare pane id is a valid target and
	// survives the pane moving to another window. -J is absent on purpose,
	// because joining wrapped lines reflows a frame drawn for the pane's width.
	t.Run("the pane is what tmux is asked about", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "argv")
		client := &Client{tmux: fakeTmux(t, `printf '%s\n' "$*" >> `+log)}

		if _, err := client.Capture(context.Background(), "dev:3.%17"); err != nil {
			t.Fatalf("capture: %v", err)
		}
		if got, want := readLines(t, log), []string{"capture-pane -e -p -t %17"}; !slices.Equal(got, want) {
			t.Fatalf("commands: got %v, want %v", got, want)
		}
	})

	t.Run("the screen comes back whole", func(t *testing.T) {
		const screen = "\x1b[38;2;108;112;134m~/p/cattery\x1b[39m\nnono sandbox\n"
		client := &Client{tmux: fakeTmux(t, "cat <<'EOF'\n"+screen+"EOF")}

		got, err := client.Capture(context.Background(), "dev:3.%17")
		if err != nil {
			t.Fatalf("capture: %v", err)
		}
		if got != screen {
			t.Fatalf("screen: got %q, want %q", got, screen)
		}
	})

	// A stopped server means the pane is gone, which is nothing to preview
	// rather than a failure the sidebar should shout about.
	t.Run("no server is an empty screen", func(t *testing.T) {
		client := &Client{tmux: fakeTmux(t, "printf 'no server running on /tmp/x\\n' >&2; exit 1")}

		got, err := client.Capture(context.Background(), "dev:3.%17")
		if err != nil {
			t.Fatalf("capture: %v", err)
		}
		if got != "" {
			t.Fatalf("screen: got %q, want empty", got)
		}
	})

	t.Run("another failure keeps tmux's reason on one line", func(t *testing.T) {
		client := &Client{tmux: fakeTmux(t, "printf \"can't find pane: %%17\\nand more\\n\" >&2; exit 1")}

		if _, err := client.Capture(context.Background(), "dev:3.%17"); err == nil {
			t.Fatal("expected an error")
		} else if !strings.Contains(err.Error(), "find pane") || strings.Contains(err.Error(), "\n") {
			t.Fatalf("error: %q", err)
		}
	})

	t.Run("a malformed target is a failure, not an empty screen", func(t *testing.T) {
		if _, err := (&Client{tmux: "tmux"}).Capture(context.Background(), "dev"); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestViewName(t *testing.T) {
	pid := strconv.Itoa(os.Getpid())
	if got, want := viewName("dev"), viewPrefix+"dev-"+pid; got != want {
		t.Fatalf("view name: got %q, want %q", got, want)
	}
	// Both characters separate the parts of a target, and an agent session can
	// hold either.
	if got := viewName("work.1:x"); strings.ContainsAny(got, ".:") {
		t.Fatalf("view name keeps a character tmux rejects: %q", got)
	}
}

func TestSendKeys(t *testing.T) {
	// tmux reads each -H argument as one byte, not as a code point: a value
	// above 0xff is dropped without a word. So a multibyte rune goes out as the
	// UTF-8 bytes it already is.
	cases := []struct {
		name string
		data string
		want []string
	}{
		{
			name: "plain text",
			data: "hi",
			want: []string{"send-keys -H -t %17 68 69"},
		},
		{
			name: "an escape sequence",
			data: "\x1b[A",
			want: []string{"send-keys -H -t %17 1b 5b 41"},
		},
		{
			name: "a multibyte rune goes out as its bytes",
			data: "界",
			want: []string{"send-keys -H -t %17 e7 95 8c"},
		},
		// A literal argument ending in ";" would end the command there and take
		// every key after it. Hex digits cannot.
		{
			name: "a semicolon is only a number",
			data: ";",
			want: []string{"send-keys -H -t %17 3b"},
		},
		// NUL cannot travel through argv inside a literal string at all.
		{
			name: "NUL survives",
			data: "\x00",
			want: []string{"send-keys -H -t %17 00"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "argv")
			client := &Client{tmux: fakeTmux(t, `printf '%s\n' "$*" >> `+log)}

			if err := client.SendKeys(context.Background(), "dev:3.%17", tc.data); err != nil {
				t.Fatalf("send: %v", err)
			}
			if got := readLines(t, log); !slices.Equal(got, tc.want) {
				t.Fatalf("commands: got %v, want %v", got, tc.want)
			}
		})
	}

	// A paste is longer than one command should carry, and tmux gets an argv
	// rather than stdin.
	t.Run("a long payload is split", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "argv")
		client := &Client{tmux: fakeTmux(t, `printf '%s\n' "$*" >> `+log)}

		if err := client.SendKeys(context.Background(), "dev:3.%17", strings.Repeat("x", sendChunk+1)); err != nil {
			t.Fatalf("send: %v", err)
		}
		got := readLines(t, log)
		if len(got) != 2 {
			t.Fatalf("commands: got %d, want 2", len(got))
		}
		if want := "send-keys -H -t %17 " + strings.TrimSpace(strings.Repeat("78 ", sendChunk)); got[0] != want {
			t.Fatalf("first command: got %q", got[0])
		}
		if got[1] != "send-keys -H -t %17 78" {
			t.Fatalf("second command: got %q", got[1])
		}
	})

	t.Run("nothing to send runs no command", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "argv")
		client := &Client{tmux: fakeTmux(t, `printf '%s\n' "$*" >> `+log)}

		if err := client.SendKeys(context.Background(), "dev:3.%17", ""); err != nil {
			t.Fatalf("send: %v", err)
		}
		if _, err := os.Stat(log); err == nil {
			t.Fatalf("ran %v for an empty payload", readLines(t, log))
		}
	})

	t.Run("a failure keeps tmux's reason on one line", func(t *testing.T) {
		client := &Client{tmux: fakeTmux(t, "printf \"can't find pane: %%17\\nand more\\n\" >&2; exit 1")}

		err := client.SendKeys(context.Background(), "dev:3.%17", "hi")
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "find pane") || strings.Contains(err.Error(), "\n") {
			t.Fatalf("error: %q", err)
		}
	})

	t.Run("a malformed target is a failure", func(t *testing.T) {
		if err := (&Client{tmux: "tmux"}).SendKeys(context.Background(), "dev", "hi"); err == nil {
			t.Fatal("expected an error")
		}
	})
}

// tmux resolves a client for send-keys even though none was asked for, and
// refuses when that client is read-only. `cattery attach` creates read-only
// clients, so an open viewer tab would otherwise break every send from the
// picker, whatever session the target pane is in.
func TestSendKeysSurvivesAReadOnlyClient(t *testing.T) {
	// The fake refuses any send-keys without "-c", the way tmux 3.3 to 3.7 do
	// when the client they picked is read-only.
	const refuseWithoutC = `
printf '%s\n' "$*" >> LOG
case "$*" in
  *" -c "*) exit 0 ;;
esac
printf 'client is read-only\n' >&2
exit 1
`

	t.Run("the send is retried at no client at all", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "argv")
		client := &Client{tmux: fakeTmux(t, strings.ReplaceAll(refuseWithoutC, "LOG", log))}

		if err := client.SendKeys(context.Background(), "dev:3.%17", "h"); err != nil {
			t.Fatalf("send: %v", err)
		}
		want := []string{
			"send-keys -H -t %17 68",
			"send-keys -c  -H -t %17 68",
		}
		if got := readLines(t, log); !slices.Equal(got, want) {
			t.Fatalf("commands: got %v, want %v", got, want)
		}
	})

	// Paying for a failed send before every real one would double the latency
	// of typing for as long as a viewer tab is open.
	t.Run("the answer sticks for the rest of the session", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "argv")
		client := &Client{tmux: fakeTmux(t, strings.ReplaceAll(refuseWithoutC, "LOG", log))}

		for range 2 {
			if err := client.SendKeys(context.Background(), "dev:3.%17", "h"); err != nil {
				t.Fatalf("send: %v", err)
			}
		}
		want := []string{
			"send-keys -H -t %17 68",
			"send-keys -c  -H -t %17 68",
			"send-keys -c  -H -t %17 68",
		}
		if got := readLines(t, log); !slices.Equal(got, want) {
			t.Fatalf("commands: got %v, want %v", got, want)
		}
	})

	// An older tmux has neither the check nor -c, so the retry must not turn a
	// plain failure into a second confusing one.
	t.Run("another failure is not retried", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "argv")
		client := &Client{tmux: fakeTmux(t, `printf '%s\n' "$*" >> `+log+`; printf "can't find pane: %%17\n" >&2; exit 1`)}

		if err := client.SendKeys(context.Background(), "dev:3.%17", "h"); err == nil {
			t.Fatal("expected an error")
		}
		if got := readLines(t, log); len(got) != 1 {
			t.Fatalf("commands: got %v, want one", got)
		}
	})

	// If the retry fails too, the first reason is the one worth reporting: it
	// is what tmux said about the command the user's tmux actually supports.
	t.Run("a failed retry keeps the original reason", func(t *testing.T) {
		client := &Client{tmux: fakeTmux(t, `printf 'client is read-only\n' >&2; exit 1`)}

		err := client.SendKeys(context.Background(), "dev:3.%17", "h")
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), readOnlyClient) {
			t.Fatalf("error: %q", err)
		}
	})
}

func TestMarkSeen(t *testing.T) {
	// On the pane, not the window: a window target resolves to whichever pane
	// is active, which is the other agent's in a split window.
	t.Run("the pane carries the marker", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "argv")
		client := &Client{tmux: fakeTmux(t, `printf '%s\n' "$*" >> `+log)}

		if err := client.MarkSeen(context.Background(), "dev:3.%17"); err != nil {
			t.Fatalf("mark seen: %v", err)
		}
		if got, want := readLines(t, log), []string{"set -p -t %17 @AGENT_SEEN 1"}; !slices.Equal(got, want) {
			t.Fatalf("commands: got %v, want %v", got, want)
		}
	})

	t.Run("a malformed target is a failure", func(t *testing.T) {
		if err := (&Client{tmux: "tmux"}).MarkSeen(context.Background(), "dev"); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestAttach(t *testing.T) {
	view := viewName("dev")

	t.Run("the commands one attachment runs", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "argv")
		client := &Client{tmux: fakeTmux(t, `printf '%s\n' "$*" >> `+log)}

		if err := client.Attach(context.Background(), "dev:3.%17"); err != nil {
			t.Fatalf("attach: %v", err)
		}

		want := []string{
			// A viewer this pid killed outright left its session behind, and the
			// name would collide.
			"kill-session -t =" + view,
			// Grouped, so the viewer has its own current window.
			"new-session -d -t =dev -s " + view,
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
		err = client.Attach(context.Background(), "dev:3.%17")
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

		err := client.Attach(context.Background(), "dev:3.%17")

		if err == nil {
			t.Fatal("expected the attach failure to surface")
		}
		if !strings.Contains(err.Error(), "dev:3.%17") {
			t.Errorf("error does not name the target: %v", err)
		}
		if got := readLines(t, log); !slices.Contains(got, "kill-session -t ="+view) {
			t.Fatalf("commands: got %v, want a kill-session among them", got)
		}
	})

	t.Run("a session that cannot be grouped is reported", func(t *testing.T) {
		client := &Client{tmux: fakeTmux(t, `printf "can't find session: dev\n" >&2; exit 1`)}

		err := client.Attach(context.Background(), "dev:3.%17")

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

		if err := client.Attach(context.Background(), "dev"); err == nil {
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
		go func() { done <- client.Attach(context.Background(), "dev:3.%17") }()

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
// The picker runs as a kitty window command, with kitty's own PATH. On a Dock
// -started kitty that PATH is launchd's, which carries no Homebrew.
func TestTmuxPath(t *testing.T) {
	onPath := fakeTmux(t, "")
	installed := fakeTmux(t, "")
	absent := filepath.Join(t.TempDir(), "tmux")

	tests := []struct {
		name      string
		path      string
		fallbacks []string
		want      string
	}{
		{name: "found on PATH", path: filepath.Dir(onPath), fallbacks: []string{installed}, want: onPath},
		{name: "PATH without homebrew", fallbacks: []string{absent, installed}, want: installed},
		{name: "not installed at all", fallbacks: []string{absent}, want: "tmux"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			if got := tmuxPath(tc.fallbacks); got != tc.want {
				t.Errorf("tmuxPath: got %q, want %q", got, tc.want)
			}
		})
	}
}

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
