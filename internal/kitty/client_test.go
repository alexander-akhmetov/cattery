package kitty

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alexander-akhmetov/cattery/internal/agent"
)

const sampleLs = `[
  {
    "tabs": [
      {
        "windows": [
          {
            "id": 119,
            "title": "pi - dotfiles",
            "cwd": "/Users/x/projects/dotfiles",
            "created_at": 1700000000000000000,
            "user_vars": {"AGENT_DISPLAY": "working", "AGENT_KIND": "pi", "AGENT_STATE": "working", "AGENT_SINCE": "1700000000"}
          },
          {
            "id": 200,
            "title": "plain shell",
            "cwd": "/Users/x",
            "user_vars": {}
          }
        ]
      },
      {
        "windows": [
          {
            "id": 114,
            "title": "Check bot",
            "cwd": "/Users/x/projects/astra-l",
            "user_vars": {"AGENT_DISPLAY": "blocked", "AGENT_KIND": "claude"}
          }
        ]
      }
    ]
  },
  {
    "tabs": [
      {
        "windows": [
          {
            "id": 118,
            "title": "pi - work",
            "cwd": "/Users/x/work",
            "user_vars": {"AGENT_DISPLAY": "idle", "AGENT_KIND": "pi"}
          }
        ]
      }
    ]
  }
]`

func TestParseAgents(t *testing.T) {
	agents, err := parseAgents([]byte(sampleLs))
	if err != nil {
		t.Fatalf("parseAgents: %v", err)
	}

	// Only the three windows with AGENT_DISPLAY stay. The plain shell drops.
	if len(agents) != 3 {
		t.Fatalf("got %d agents, want 3", len(agents))
	}

	// parseAgents never sorts. Ordering needs the repo lookup ListAgents runs
	// afterwards, so windows come back in kitty's order.
	wantOrder := []struct {
		id      int
		display string
	}{
		{119, "working"},
		{114, "blocked"},
		{118, "idle"},
	}
	for i, w := range wantOrder {
		if agents[i].ID != w.id || agents[i].Display != w.display {
			t.Errorf("position %d: got id=%d display=%s, want id=%d display=%s",
				i, agents[i].ID, agents[i].Display, w.id, w.display)
		}
	}

	if agents[0].Kind != "pi" {
		t.Errorf("working agent kind: got %q, want pi", agents[0].Kind)
	}
	// Every window here is a kitty agent, which is what tells the picker to
	// focus it rather than attach to it.
	for _, a := range agents {
		if a.Host != agent.HostKitty {
			t.Errorf("window %d host: got %q, want %q", a.ID, a.Host, agent.HostKitty)
		}
		if a.Target != "" {
			t.Errorf("window %d target: got %q, want empty", a.ID, a.Target)
		}
	}
	if got := agents[0].Since; !got.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("AGENT_SINCE parse: got %v, want %v", got, time.Unix(1700000000, 0))
	}
	if got := agents[0].CreatedAt; !got.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("created_at parse: got %v, want %v", got, time.Unix(1700000000, 0))
	}
	if !agents[1].Since.IsZero() {
		t.Errorf("missing AGENT_SINCE should be zero time, got %v", agents[1].Since)
	}
	if !agents[1].CreatedAt.IsZero() {
		t.Errorf("missing created_at should be zero time, got %v", agents[1].CreatedAt)
	}
}

// A window outlives its agents, so a label a killed pi left behind must not
// pin itself to whatever runs there next, and a timestamp that is not one must
// not read as decades of running time.
func TestParseAgentsTool(t *testing.T) {
	cases := []struct {
		name      string
		vars      map[string]string
		wantTool  string
		wantSince time.Time
	}{
		{
			name: "a working agent",
			vars: map[string]string{
				"AGENT_DISPLAY": "working", "AGENT_TOOL": "bash: go test ./...",
				"AGENT_TOOL_SINCE": "1700000000",
			},
			wantTool: "bash: go test ./...", wantSince: time.Unix(1700000000, 0),
		},
		{
			// The watcher publishes this one itself, and it is the row the tool
			// line exists for.
			name: "an agent the watcher already called stalled",
			vars: map[string]string{
				"AGENT_DISPLAY": "stalled", "AGENT_TOOL": "bash: sleep 900",
				"AGENT_TOOL_SINCE": "1700000000",
			},
			wantTool: "bash: sleep 900", wantSince: time.Unix(1700000000, 0),
		},
		{
			name: "an idle agent ignores a stale label",
			vars: map[string]string{
				"AGENT_DISPLAY": "idle", "AGENT_TOOL": "bash: go test ./...",
				"AGENT_TOOL_SINCE": "1700000000",
			},
		},
		{
			// The window a killed pi left behind. Nothing clears the pair for
			// the agent that starts next, and it would read as stalled at once.
			name: "a claude agent in a window a pi died in",
			vars: map[string]string{
				"AGENT_DISPLAY": "working", "AGENT_KIND": "claude",
				"AGENT_TOOL": "bash: sleep 900", "AGENT_TOOL_SINCE": "1700000000",
			},
		},
		{name: "no tool published", vars: map[string]string{"AGENT_DISPLAY": "working"}},
		{
			name:     "a label with no timestamp",
			vars:     map[string]string{"AGENT_DISPLAY": "working", "AGENT_TOOL": "bash: x"},
			wantTool: "bash: x",
		},
		{
			name: "a zero timestamp is not a timestamp",
			vars: map[string]string{
				"AGENT_DISPLAY": "working", "AGENT_TOOL": "bash: x", "AGENT_TOOL_SINCE": "0",
			},
			wantTool: "bash: x",
		},
		{
			name: "an unparsable timestamp is dropped",
			vars: map[string]string{
				"AGENT_DISPLAY": "working", "AGENT_TOOL": "bash: x", "AGENT_TOOL_SINCE": "soon",
			},
			wantTool: "bash: x",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// pi is the only kind that publishes a tool, so every case that is
			// not about the kind is a pi agent.
			if _, ok := tc.vars["AGENT_KIND"]; !ok {
				tc.vars["AGENT_KIND"] = "pi"
			}
			vars, err := json.Marshal(tc.vars)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			data := `[{"tabs":[{"windows":[{"id":1,"title":"t","cwd":"/p","user_vars":` + string(vars) + `}]}]}]`

			agents, err := parseAgents([]byte(data))
			if err != nil {
				t.Fatalf("parseAgents: %v", err)
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
}

func TestParseAgentsEmpty(t *testing.T) {
	agents, err := parseAgents([]byte(`[]`))
	if err != nil {
		t.Fatalf("parseAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("got %d agents, want 0", len(agents))
	}
}

func TestParseAgentsInvalid(t *testing.T) {
	if _, err := parseAgents([]byte(`not json`)); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestFocusCommand(t *testing.T) {
	cmd := focusCommand(context.Background(), "kitten", 42)
	want := []string{"kitten", "@", "focus-window", "--match", "id:42"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("focus args: got %v, want %v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("focus arg %d: got %q, want %q", i, cmd.Args[i], want[i])
		}
	}
}

func TestSetUserVarsCommand(t *testing.T) {
	cmd := setUserVarsCommand(context.Background(), "kitten", 42, []string{"AGENT_KIND=claude", "AGENT_STATE=working", "AGENT_MSG"})
	want := []string{
		"kitten", "@", "set-user-vars", "--match", "id:42",
		"AGENT_KIND=claude", "AGENT_STATE=working", "AGENT_MSG",
	}
	if len(cmd.Args) != len(want) {
		t.Fatalf("set-user-vars args: got %v, want %v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("set-user-vars arg %d: got %q, want %q", i, cmd.Args[i], want[i])
		}
	}
}

// The state writer calls this from a Claude hook, which swallows a failure and
// leaves the tab marker frozen. The reason has to survive the call instead of
// arriving as a bare exit status.
func TestSetUserVars(t *testing.T) {
	vars := []string{"AGENT_KIND=claude", "AGENT_STATE=working"}

	t.Run("nothing to publish runs no command", func(t *testing.T) {
		client := &Client{kitten: filepath.Join(t.TempDir(), "absent-kitten")}
		if err := client.SetUserVars(context.Background(), 42, nil); err != nil {
			t.Fatalf("set-user-vars: got %v, want nil", err)
		}
	})

	t.Run("success returns nil", func(t *testing.T) {
		client := &Client{kitten: fakeKitten(t, "exit 0")}
		if err := client.SetUserVars(context.Background(), 42, vars); err != nil {
			t.Fatalf("set-user-vars: got %v, want nil", err)
		}
	})

	cases := []struct {
		name   string
		kitten string
		want   []string
	}{
		{
			name:   "reports kitty's own reason",
			kitten: fakeKitten(t, "printf 'no listening socket\\nfor id:42\\n' >&2; exit 1"),
			want:   []string{"window 42", "no listening socket for id:42"},
		},
		{
			name:   "falls back to the exit status when output is silent",
			kitten: fakeKitten(t, "exit 3"),
			want:   []string{"window 42", "exit status 3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{kitten: tc.kitten}
			err := client.SetUserVars(context.Background(), 42, vars)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error missing %q, got %q", w, err.Error())
				}
			}
		})
	}
}

func TestCondense(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{name: "single line", in: "no matching window", want: "no matching window"},
		{name: "trailing newline", in: "no matching window\n", want: "no matching window"},
		{
			name: "python traceback",
			in:   "Traceback (most recent call last):\n  File \"kitty\", line 1\nError: no matching window for id:42\n",
			want: `Traceback (most recent call last): File "kitty", line 1 Error: no matching window for id:42`,
		},
		{name: "tabs and runs", in: "a\t\tb   c", want: "a b c"},
		{name: "whitespace only", in: " \n\t ", want: ""},
		{name: "empty", in: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := condense(tc.in); got != tc.want {
				t.Fatalf("condense: got %q, want %q", got, tc.want)
			}
		})
	}
}

// A failed jump must name the window it targeted and stay on one line, so the
// overlay banner shows something better than "exit status 1".
func TestFocusWindowError(t *testing.T) {
	cases := []struct {
		name   string
		kitten string
		want   []string
	}{
		{
			name:   "reports command output",
			kitten: fakeKitten(t, "printf 'no matching window\\nfor id:42\\n' >&2; exit 1"),
			want:   []string{"window 42", "no matching window for id:42"},
		},
		{
			name:   "falls back to the exit status when output is silent",
			kitten: fakeKitten(t, "exit 3"),
			want:   []string{"window 42", "exit status 3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{kitten: tc.kitten}
			err := client.FocusWindow(context.Background(), 42)
			if err == nil {
				t.Fatal("expected a focus error")
			}
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("focus error spans lines: %q", err.Error())
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("focus error missing %q, got %q", w, err.Error())
				}
			}
		})
	}

	t.Run("success returns nil", func(t *testing.T) {
		client := &Client{kitten: fakeKitten(t, "exit 0")}
		if err := client.FocusWindow(context.Background(), 42); err != nil {
			t.Fatalf("focus: got %v, want nil", err)
		}
	})
}

// The reload banner shows this error unchanged, so a failed inventory read has
// to carry kitty's own reason.
func TestListAgentsError(t *testing.T) {
	cases := []struct {
		name   string
		kitten string
		want   string
	}{
		{
			name:   "reports stderr",
			kitten: fakeKitten(t, "printf 'no listening socket\\nfor id:1\\n' >&2; exit 1"),
			want:   "no listening socket for id:1",
		},
		{
			name:   "falls back to the exit status when output is silent",
			kitten: fakeKitten(t, "exit 3"),
			want:   "exit status 3",
		},
		{
			name:   "missing kitten binary",
			kitten: filepath.Join(t.TempDir(), "absent-kitten"),
			want:   "absent-kitten",
		},
		{
			name:   "unparseable inventory",
			kitten: fakeKitten(t, "printf 'not json'"),
			want:   "invalid character",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{kitten: tc.kitten}
			_, err := client.ListAgents(context.Background())
			if err == nil {
				t.Fatal("expected a list error")
			}
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("list error spans lines: %q", err.Error())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("list error missing %q, got %q", tc.want, err.Error())
			}
		})
	}
}

// sessionLs is `kitten @ ls` after a session restore: two windows in one tab,
// one of them resumable, a plain window with no user variables at all, and a
// window the user opened, which carries no session name.
//
// session_name sits on the window, the only place kitty reports it. Its tab
// dictionaries carry no such key.
const sessionLs = `[
  {
    "tabs": [
      {
        "windows": [
          {
            "id": 11,
            "title": "shell",
            "cwd": "/Users/x/projects/dotfiles",
            "at_prompt": true,
            "session_name": "agents",
            "user_vars": {"AGENT_RESUME": "pi --session /tmp/a.jsonl"}
          },
          {
            "id": 12,
            "title": "shell",
            "cwd": "/Users/x/projects/dotfiles",
            "at_prompt": false,
            "session_name": "agents",
            "user_vars": {}
          }
        ]
      },
      {
        "windows": [
          {
            "id": 20,
            "title": "plain",
            "cwd": "/Users/x"
          }
        ]
      }
    ]
  },
  {
    "tabs": [
      {
        "windows": [
          {
            "id": 30,
            "title": "restored on its own",
            "cwd": "/Users/x/work",
            "session_name": "agents",
            "at_prompt": true,
            "user_vars": {"AGENT_RESUME": "claude --resume abc-123", "AGENT_DISPLAY": "idle"}
          }
        ]
      }
    ]
  }
]`

func TestParseWindows(t *testing.T) {
	windows, err := parseWindows([]byte(sessionLs))
	if err != nil {
		t.Fatalf("parseWindows: %v", err)
	}

	// Nothing is filtered out. The plain shell and the window with no user_vars
	// key are both here, and parseAgents drops both.
	want := []Window{
		{
			ID: 11, Title: "shell", CWD: "/Users/x/projects/dotfiles",
			SessionName: "agents", AtPrompt: true,
			UserVars: map[string]string{"AGENT_RESUME": "pi --session /tmp/a.jsonl"},
		},
		{
			ID: 12, Title: "shell", CWD: "/Users/x/projects/dotfiles",
			SessionName: "agents", AtPrompt: false,
			UserVars: map[string]string{},
		},
		{ID: 20, Title: "plain", CWD: "/Users/x"},
		{
			ID: 30, Title: "restored on its own", CWD: "/Users/x/work",
			SessionName: "agents", AtPrompt: true,
			UserVars: map[string]string{"AGENT_RESUME": "claude --resume abc-123", "AGENT_DISPLAY": "idle"},
		},
	}
	if len(windows) != len(want) {
		t.Fatalf("got %d windows, want %d: %+v", len(windows), len(want), windows)
	}
	for i, w := range want {
		got := windows[i]
		if got.ID != w.ID || got.Title != w.Title || got.CWD != w.CWD {
			t.Errorf("window %d: got id=%d title=%q cwd=%q, want id=%d title=%q cwd=%q",
				i, got.ID, got.Title, got.CWD, w.ID, w.Title, w.CWD)
		}
		if got.SessionName != w.SessionName {
			t.Errorf("window %d session_name: got %q, want %q", i, got.SessionName, w.SessionName)
		}
		if got.AtPrompt != w.AtPrompt {
			t.Errorf("window %d at_prompt: got %v, want %v", i, got.AtPrompt, w.AtPrompt)
		}
		if !maps.Equal(got.UserVars, w.UserVars) {
			t.Errorf("window %d user_vars: got %v, want %v", i, got.UserVars, w.UserVars)
		}
	}
}

// The picker's inventory keeps its shape, even though restore needed a wider
// one.
func TestParseAgentsIgnoresSessionFields(t *testing.T) {
	agents, err := parseAgents([]byte(sessionLs))
	if err != nil {
		t.Fatalf("parseAgents: %v", err)
	}
	// Only window 30 published AGENT_DISPLAY. AGENT_RESUME alone is not enough.
	if len(agents) != 1 || agents[0].ID != 30 {
		t.Fatalf("got %+v, want only window 30", agents)
	}
}

func TestParseWindowsEmpty(t *testing.T) {
	windows, err := parseWindows([]byte(`[]`))
	if err != nil {
		t.Fatalf("parseWindows: %v", err)
	}
	if len(windows) != 0 {
		t.Fatalf("got %d windows, want 0", len(windows))
	}
}

func TestParseWindowsInvalid(t *testing.T) {
	if _, err := parseWindows([]byte(`not json`)); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

// kitty parses an action and its arguments out of one string. Separate argv
// entries open a window titled "Invalid ... command line" instead of failing,
// so the single string is the point of the method.
func TestActionCommand(t *testing.T) {
	cmd := actionCommand(context.Background(), "kitten", "save_as_session --save-only /tmp/a.kitty-session")
	want := []string{"kitten", "@", "action", "save_as_session --save-only /tmp/a.kitty-session"}
	if !slices.Equal(cmd.Args, want) {
		t.Fatalf("action args: got %v, want %v", cmd.Args, want)
	}
}

// launch is the opposite of action: a remote-control command with its own
// parser, so its arguments stay separate argv entries. The command after "--"
// reaches kitty unsplit, which is what keeps a target with a space in it whole.
func TestLaunchCommand(t *testing.T) {
	args := []string{
		"--type=tab", "--title", "ro kontora:3.%17", "--var", "AGENT_VIEW=kontora:3.%17",
		"--", "/usr/local/bin/cattery", "attach", "kontora:3.%17",
	}
	cmd := launchCommand(context.Background(), "kitten", args)
	want := append([]string{"kitten", "@", "launch"}, args...)
	if !slices.Equal(cmd.Args, want) {
		t.Fatalf("launch args: got %v, want %v", cmd.Args, want)
	}
}

// The preview asks for the visible screen with its colours, and for nothing
// else. --add-cursor and --add-wrap-markers would put escapes into text that
// the picker draws inside its own frame.
func TestTextCommand(t *testing.T) {
	cmd := textCommand(context.Background(), "kitten", 386)
	want := []string{"kitten", "@", "get-text", "--match", "id:386", "--extent", "screen", "--ansi"}
	if !slices.Equal(cmd.Args, want) {
		t.Fatalf("get-text args: got %v, want %v", cmd.Args, want)
	}
}

func TestText(t *testing.T) {
	t.Run("the screen comes back whole", func(t *testing.T) {
		const screen = "\x1b[m❯ \n\x1b[38;5;39mbuilding…\x1b[39m\n"
		client := &Client{kitten: fakeKitten(t, "cat <<'EOF'\n"+screen+"EOF")}

		got, err := client.Text(context.Background(), 386)
		if err != nil {
			t.Fatalf("text: %v", err)
		}
		if got != screen {
			t.Fatalf("screen: got %q, want %q", got, screen)
		}
	})

	// The sidebar draws the reason on one line, and stderr must never reach the
	// column as if it were the agent's screen.
	t.Run("a failure names the window and stays on one line", func(t *testing.T) {
		client := &Client{kitten: fakeKitten(t, "printf 'no matching window\\nsecond line\\n' >&2; exit 1")}

		got, err := client.Text(context.Background(), 386)
		if err == nil {
			t.Fatal("no error for a window kitty does not have")
		}
		if got != "" {
			t.Fatalf("screen: got %q, want empty", got)
		}
		if !strings.Contains(err.Error(), "window 386") || !strings.Contains(err.Error(), "no matching window") {
			t.Fatalf("error: %v", err)
		}
		if strings.Contains(err.Error(), "\n") {
			t.Fatalf("error spans lines: %q", err)
		}
	})
}

// The text goes in on stdin, never as an argument. kitty reads Python escapes
// out of a positional text argument, which mangles a shell-quoted path. The
// POSIX escape for a single quote loses its backslash and leaves an
// unterminated string, and a \n becomes a real newline that runs the command.
func TestSendTextCommand(t *testing.T) {
	const text = `pi --session '/tmp/al'\''ex/a.jsonl'`
	cmd := sendTextCommand(context.Background(), "kitten", 42, text)
	want := []string{"kitten", "@", "send-text", "--match", "id:42", "--stdin"}
	if !slices.Equal(cmd.Args, want) {
		t.Fatalf("send-text args: got %v, want %v", cmd.Args, want)
	}
	if cmd.Stdin == nil {
		t.Fatal("no text on stdin")
	}
	got, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != text {
		t.Fatalf("stdin: got %q, want %q", got, text)
	}

	// A resume command starting with a dash reaches the window as text. It is
	// never on the command line, so kitty cannot read it as a flag.
	dashed := sendTextCommand(context.Background(), "kitten", 42, "-weird")
	if slices.Contains(dashed.Args, "-weird") {
		t.Fatalf("the text reached the command line: %v", dashed.Args)
	}
}

// `kitten @ kitten` has its own parser, so the path and each argument stay
// separate argv entries. One joined string arrives as a kitten name nobody
// installed.
func TestKittenCommand(t *testing.T) {
	args := []string{"register", "/tmp/cattery-sub-7.sock"}
	cmd := kittenCommand(context.Background(), "kitten", "/k/cattery_events.py", args)
	want := []string{"kitten", "@", "kitten", "/k/cattery_events.py", "register", "/tmp/cattery-sub-7.sock"}
	if !slices.Equal(cmd.Args, want) {
		t.Fatalf("kitten args: got %v, want %v", cmd.Args, want)
	}
}

// The kitten answers on stdout, and `cattery events` has nowhere to go without
// it: a registration that failed has to say why, so the caller can hear that
// setup has not run since the upgrade.
func TestKitten(t *testing.T) {
	t.Run("the answer comes back trimmed", func(t *testing.T) {
		client := &Client{kitten: fakeKitten(t, "echo registered /tmp/sub.sock")}

		got, err := client.Kitten(context.Background(), "/k/cattery_events.py", "register", "/tmp/sub.sock")

		if err != nil {
			t.Fatalf("kitten: %v", err)
		}
		if got != "registered /tmp/sub.sock" {
			t.Errorf("answer: got %q", got)
		}
	})

	cases := []struct {
		name   string
		kitten string
		want   []string
	}{
		{
			name:   "reports kitty's own reason",
			kitten: fakeKitten(t, "printf 'No such kitten\\navailable\\n' >&2; exit 1"),
			want:   []string{"/k/cattery_events.py", "No such kitten available"},
		},
		{
			name:   "falls back to the exit status when output is silent",
			kitten: fakeKitten(t, "exit 3"),
			want:   []string{"/k/cattery_events.py", "exit status 3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{kitten: tc.kitten}

			_, err := client.Kitten(context.Background(), "/k/cattery_events.py", "register", "/tmp/sub.sock")

			if err == nil {
				t.Fatal("expected an error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error missing %q, got %q", w, err.Error())
				}
			}
		})
	}
}

// Both new commands run through the same error path as the rest of the client.
// kitty's own reason survives on one line, so the picker's notice can show it.
func TestActionAndSendTextErrors(t *testing.T) {
	cases := []struct {
		name   string
		run    func(*Client) error
		kitten string
		want   []string
	}{
		{
			name:   "action reports kitty's reason",
			run:    func(c *Client) error { return c.Action(context.Background(), "goto_session /tmp/a") },
			kitten: fakeKitten(t, "printf 'no such file\\nor directory\\n' >&2; exit 1"),
			want:   []string{"goto_session /tmp/a", "no such file or directory"},
		},
		{
			name:   "action falls back to the exit status",
			run:    func(c *Client) error { return c.Action(context.Background(), "goto_session /tmp/a") },
			kitten: fakeKitten(t, "exit 3"),
			want:   []string{"goto_session /tmp/a", "exit status 3"},
		},
		{
			name:   "send-text reports kitty's reason",
			run:    func(c *Client) error { return c.SendText(context.Background(), 42, "pi") },
			kitten: fakeKitten(t, "printf 'no listening socket\\nfor id:42\\n' >&2; exit 1"),
			want:   []string{"window 42", "no listening socket for id:42"},
		},
		{
			name:   "send-text falls back to the exit status",
			run:    func(c *Client) error { return c.SendText(context.Background(), 42, "pi") },
			kitten: fakeKitten(t, "exit 3"),
			want:   []string{"window 42", "exit status 3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(&Client{kitten: tc.kitten})
			if err == nil {
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("error spans lines: %q", err.Error())
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error missing %q, got %q", w, err.Error())
				}
			}
		})
	}

	t.Run("success returns nil", func(t *testing.T) {
		client := &Client{kitten: fakeKitten(t, "exit 0")}
		if err := client.Action(context.Background(), "goto_session /tmp/a"); err != nil {
			t.Fatalf("action: got %v, want nil", err)
		}
		if err := client.SendText(context.Background(), 42, "pi"); err != nil {
			t.Fatalf("send-text: got %v, want nil", err)
		}
	})
}

func TestWindows(t *testing.T) {
	t.Run("parses kitty's inventory", func(t *testing.T) {
		client := &Client{kitten: fakeKitten(t, "cat <<'EOF'\n"+sessionLs+"\nEOF")}
		windows, err := client.Windows(context.Background())
		if err != nil {
			t.Fatalf("windows: %v", err)
		}
		if len(windows) != 4 {
			t.Fatalf("got %d windows, want 4", len(windows))
		}
	})

	t.Run("keeps kitty's reason on failure", func(t *testing.T) {
		client := &Client{kitten: fakeKitten(t, "printf 'no listening socket\\n' >&2; exit 1")}
		_, err := client.Windows(context.Background())
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "no listening socket") {
			t.Fatalf("error missing kitty's reason: %q", err.Error())
		}
	})
}

// fakeKitten writes a stub kitten script running body, and returns its path.
func fakeKitten(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kitten")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
