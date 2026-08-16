package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexander-akhmetov/cattery/internal/kitty"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "mixed.kitty-session"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The fixture is a trimmed capture from a real kitty 0.48.1 instance, so the
// rewrite is tested against the quoting kitty actually produces.
func TestRewriteFixture(t *testing.T) {
	got, stats := rewrite(fixture(t))
	out := string(got)
	lines := strings.Split(out, "\n")

	t.Run("live agent state never survives", func(t *testing.T) {
		// A stale AGENT_STATE or AGENT_DISPLAY would come back as a tab marker
		// for an agent that is not running.
		for _, name := range []string{"AGENT_STATE", "AGENT_DISPLAY", "AGENT_SINCE", "AGENT_MSG", "AGENT_KIND"} {
			if strings.Contains(out, name) {
				t.Errorf("%s survived the rewrite", name)
			}
		}
	})

	t.Run("the picker's own overlay window is gone", func(t *testing.T) {
		if strings.Contains(out, "--type=overlay") {
			t.Error("an overlay window survived the rewrite")
		}
		if strings.Contains(out, "sleep 25") {
			t.Error("the overlay's command survived the rewrite")
		}
	})

	t.Run("resume commands survive", func(t *testing.T) {
		want := []string{
			`'--var=AGENT_RESUME=claude --resume abc-123'`,
			`'--var=AGENT_RESUME=pi --session /Users/x/.pi/agent/sessions/x/2026-08-04.jsonl'`,
		}
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("missing %s", w)
			}
		}
	})

	t.Run("an agent's recorded command is dropped", func(t *testing.T) {
		// pi --fork would fork a session that ended. Restore types the resume
		// command into a plain shell instead.
		if strings.Contains(out, "--fork") {
			t.Error("a pi --fork command survived the rewrite")
		}
		if strings.Contains(out, "mise/installs/node") {
			t.Error("the node interpreter path survived the rewrite")
		}
	})

	t.Run("an agent with no resume command still loses its fork", func(t *testing.T) {
		// The third tab's window published no AGENT_RESUME, so nothing types
		// into it. Rerunning its fork would reopen a dead session.
		if strings.Contains(out, "stale.jsonl") {
			t.Error("a stale session path survived the rewrite")
		}
	})

	t.Run("everything that is not a launch line is untouched", func(t *testing.T) {
		for _, want := range []string{
			// An unquoted path with spaces, which the tokenizer must not touch.
			"cd /Users/x/Documents/My Notes/otel",
			"cd /Users/x/projects/myapp",
			"new_tab pi fork",
			"layout fat",
			"layout tall",
			"enabled_layouts fat,grid,horizontal,splits,stack,tall,vertical",
			`set_layout_state {"main_bias": [0.5, 0.5], "biased_map": {}, "opts": {"full_size": 1, "bias": 50, "mirrored": "n"}, "class": "Fat", "all_windows": {"active_group_idx": 0, "active_group_history": [618], "window_groups": [{"id": 618, "window_ids": [649]}]}}`,
			"focus",
			"focus_tab 4",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("missing untouched line: %q", want)
			}
		}
	})

	t.Run("a non-agent launch keeps its command", func(t *testing.T) {
		// This window is not an agent, so it comes back running what the user
		// asked kitty to run.
		if !strings.Contains(out, "launch --title=logs tail -f /var/log/system.log") {
			t.Error("a non-agent launch line lost its command")
		}
	})

	t.Run("a plain shell stays a plain shell", func(t *testing.T) {
		if !strings.Contains(out, `launch 'kitty-unserialize-data={"id": 693}'`) {
			t.Error("the plain shell line changed")
		}
	})

	t.Run("kitty-unserialize-data is not mistaken for the command", func(t *testing.T) {
		// The blob is the one token before the command that does not start
		// with a dash. Reading it as the command would strip a resume
		// variable's line back to "launch" alone.
		for _, line := range lines {
			if isLaunch(line) && strings.Contains(line, "AGENT_RESUME") && !strings.Contains(line, "kitty-unserialize-data") {
				t.Errorf("line lost its unserialize data: %q", line)
			}
		}
	})

	t.Run("statistics", func(t *testing.T) {
		if stats.Tabs != 4 {
			t.Errorf("tabs: got %d, want 4", stats.Tabs)
		}
		if stats.Resumable != 2 {
			t.Errorf("resumable: got %d, want 2", stats.Resumable)
		}
	})
}

// The exact output of the two lines that change most, so a regression shows as
// a diff instead of a missing substring.
func TestRewriteLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
		drop bool
	}{
		{
			name: "an agent line keeps only its resume command",
			line: `launch 'kitty-unserialize-data={"id": 1}' --var=AGENT_KIND=pi '--var=AGENT_MSG=hi there' --var=AGENT_STATE=idle '--var=AGENT_RESUME=pi --session /tmp/a.jsonl' /opt/pi --fork /tmp/a.jsonl`,
			want: `launch 'kitty-unserialize-data={"id": 1}' '--var=AGENT_RESUME=pi --session /tmp/a.jsonl'`,
		},
		{
			name: "an agent line with no command tail",
			line: `launch 'kitty-unserialize-data={"id": 2}' --var=AGENT_KIND=claude --var=AGENT_STATE=blocked '--var=AGENT_RESUME=claude --resume abc'`,
			want: `launch 'kitty-unserialize-data={"id": 2}' '--var=AGENT_RESUME=claude --resume abc'`,
		},
		{
			name: "an agent line with no resume command loses its state and its fork",
			line: `launch 'kitty-unserialize-data={"id": 3}' --var=AGENT_KIND=pi --var=AGENT_STATE=idle /opt/pi --fork /tmp/stale.jsonl`,
			want: `launch 'kitty-unserialize-data={"id": 3}'`,
		},
		{
			name: "an overlay line goes",
			line: `launch 'kitty-unserialize-data={"id": 4}' --cwd=/Users/x --type=overlay sh -c 'sleep 25'`,
			drop: true,
		},
		{
			name: "an overlay line goes even when it is an agent",
			line: `launch --type=overlay '--var=AGENT_RESUME=pi --session /tmp/a.jsonl'`,
			drop: true,
		},
		{
			// kitty writes --type=overlay-main for an overlay opened as the
			// tab's main window. It carries no AGENT_* variable, so keeping the
			// line would keep its command and restore would run it.
			name: "an overlay-main line goes too",
			line: `launch 'kitty-unserialize-data={"id": 7}' --cwd=/Users/x --type=overlay-main btop`,
			drop: true,
		},
		{
			// Every case below comes back as the same string instead of being
			// rebuilt, so spacing and quoting cannot drift.
			name: "a plain shell line is untouched",
			line: `launch 'kitty-unserialize-data={"id": 5}'`,
			want: `launch 'kitty-unserialize-data={"id": 5}'`,
		},
		{
			name: "a non-agent launch with a command is untouched",
			line: `launch --title=logs tail -f /var/log/system.log`,
			want: `launch --title=logs tail -f /var/log/system.log`,
		},
		{
			name: "a bare launch is untouched",
			line: `launch`,
			want: `launch`,
		},
		{
			name: "a cd line is not a launch line",
			line: `cd /Users/x/My Documents/some dir`,
			want: `cd /Users/x/My Documents/some dir`,
		},
		{
			name: "a line that only mentions launch is untouched",
			line: `cd /Users/x/launch pad`,
			want: `cd /Users/x/launch pad`,
		},
		{
			name: "unbalanced quotes are left alone rather than guessed at",
			line: `launch 'kitty-unserialize-data={"id": 6}' --var=AGENT_STATE='oops`,
			want: `launch 'kitty-unserialize-data={"id": 6}' --var=AGENT_STATE='oops`,
		},
		{
			name: "a quoted single quote inside a message survives being dropped",
			line: `launch --var=AGENT_KIND=pi '--var=AGENT_MSG=it'"'"'s here' '--var=AGENT_RESUME=pi --session /tmp/a.jsonl'`,
			want: `launch '--var=AGENT_RESUME=pi --session /tmp/a.jsonl'`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, drop := rewriteLine(tc.line)
			if drop != tc.drop {
				t.Fatalf("dropped: got %v, want %v", drop, tc.drop)
			}
			if drop {
				return
			}
			if got != tc.want {
				t.Fatalf("rewriteLine:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// A line the rewrite has no reason to change comes back identical, so
// tokenizing can never reformat it.
func TestRewriteKeepsUntouchedLinesIdentical(t *testing.T) {
	for line := range strings.SplitSeq(string(fixture(t)), "\n") {
		if strings.Contains(line, varPrefix) || strings.Contains(line, "--type=overlay") {
			continue
		}
		got, drop := rewriteLine(line)
		if drop {
			t.Errorf("dropped a line with no agent state: %q", line)
		}
		if got != line {
			t.Errorf("changed a line it had no reason to change:\n got %q\nwant %q", got, line)
		}
	}
}

func TestSplitTokens(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []string // decoded values
		bad  bool
	}{
		{name: "plain words", line: `launch --title=x tail -f`, want: []string{"launch", "--title=x", "tail", "-f"}},
		{
			name: "a single-quoted token holding spaces",
			line: `launch '--var=AGENT_MSG=fix the bar'`,
			want: []string{"launch", "--var=AGENT_MSG=fix the bar"},
		},
		{
			name: "kitty's escaped single quote",
			line: `launch '--var=AGENT_MSG=it'"'"'s here'`,
			want: []string{"launch", "--var=AGENT_MSG=it's here"},
		},
		{
			name: "the unserialize blob, which holds a space and braces",
			line: `launch 'kitty-unserialize-data={"id": 649}'`,
			want: []string{"launch", `kitty-unserialize-data={"id": 649}`},
		},
		{name: "runs of spaces", line: `launch    --a     --b`, want: []string{"launch", "--a", "--b"}},
		{name: "a tab separator", line: "launch\t--a", want: []string{"launch", "--a"}},
		{name: "double quotes", line: `launch "a b" c`, want: []string{"launch", "a b", "c"}},
		{name: "a backslash escape", line: `launch a\ b`, want: []string{"launch", "a b"}},
		{name: "empty", line: ``, want: nil},
		{name: "unterminated single quote", line: `launch 'a`, bad: true},
		{name: "unterminated double quote", line: `launch "a`, bad: true},
		{name: "a trailing backslash", line: `launch a\`, bad: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tokens, ok := splitTokens(tc.line)
			if ok == tc.bad {
				t.Fatalf("ok: got %v, want %v", ok, !tc.bad)
			}
			if tc.bad {
				return
			}
			if len(tokens) != len(tc.want) {
				t.Fatalf("got %d tokens, want %d: %+v", len(tokens), len(tc.want), tokens)
			}
			for i, w := range tc.want {
				if tokens[i].value != w {
					t.Errorf("token %d value: got %q, want %q", i, tokens[i].value, w)
				}
			}
			// Every raw span must come straight out of the line, or the rebuilt
			// line differs from what kitty wrote.
			for _, tok := range tokens {
				if !strings.Contains(tc.line, tok.raw) {
					t.Errorf("raw token %q is not a span of the line", tok.raw)
				}
			}
		})
	}
}

func TestCommandStart(t *testing.T) {
	cases := []struct {
		name string
		line string
		want int
	}{
		{name: "no command", line: `launch 'kitty-unserialize-data={"id": 1}' --var=AGENT_KIND=pi`, want: -1},
		{name: "bare launch", line: `launch`, want: -1},
		{name: "flags only", line: `launch --title=x --cwd=/tmp`, want: -1},
		{name: "command after the blob", line: `launch 'kitty-unserialize-data={"id": 1}' sh -c x`, want: 2},
		{name: "command after flags", line: `launch --title=x tail -f`, want: 2},
		{name: "command first", line: `launch tail -f`, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tokens, ok := splitTokens(tc.line)
			if !ok {
				t.Fatalf("splitTokens rejected %q", tc.line)
			}
			if got := commandStart(tokens); got != tc.want {
				t.Fatalf("commandStart: got %d, want %d", got, tc.want)
			}
		})
	}
}

// Every case here was checked against kitty 0.48.1's session_arg_to_name.
// Getting the name wrong is silent: restore then recognises none of the windows
// it made, types no resume commands, and never fires the duplicate guard.
func TestSessionName(t *testing.T) {
	cases := []struct{ path, want string }{
		{path: "/tmp/agents-test.kitty-session", want: "agents-test"},
		{path: "/tmp/probe.kitty-session", want: "probe"},
		{path: "agents.kitty-session", want: "agents"},
		{path: "/tmp/no-extension", want: "no-extension"},
		{path: "/tmp/two.dots.kitty-session", want: "two.dots"},

		// kitty drops three suffixes and no others.
		{path: "/tmp/agents.session", want: "agents"},
		{path: "/tmp/agents.kitty_session", want: "agents"},
		{path: "/tmp/agents.snapshot", want: "agents.snapshot"},
		{path: "/tmp/agents.txt", want: "agents.txt"},
		{path: "/tmp/.session", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := sessionName(tc.path); got != tc.want {
				t.Fatalf("sessionName(%q): got %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// --- a kitty that is not there ------------------------------------------------

// fakeClient stands in for kitty remote control. Like the real kitty, its
// save_as_session writes whatever `native` holds to the path in the action.
type fakeClient struct {
	mu sync.Mutex

	native     string // the session file kitty "writes"
	writeNever bool   // report success without writing anything
	actions    []string
	actionErr  map[string]error // keyed by the action's first word

	// pre is the inventory before goto_session runs, which the duplicate guard
	// reads.
	pre []kitty.Window

	// windows is the inventory each Windows call returns once goto_session has
	// run. The last entry repeats once the calls run past the end, so a test can
	// say "not ready, not ready, then ready".
	windows    [][]kitty.Window
	windowsErr error
	windowCall int
	restored   bool

	sent    []sentText
	sendErr error
}

type sentText struct {
	id   int
	text string
}

func (c *fakeClient) Action(_ context.Context, arg string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.actions = append(c.actions, arg)
	verb, rest, _ := strings.Cut(arg, " ")
	if err := c.actionErr[verb]; err != nil {
		return err
	}
	switch verb {
	case "save_as_session":
		if c.writeNever {
			return nil
		}
		path := strings.TrimPrefix(rest, "--save-only ")
		return os.WriteFile(strings.Trim(path, "'"), []byte(c.native), 0o600)
	case "goto_session":
		c.restored = true
	}
	return nil
}

func (c *fakeClient) SendText(_ context.Context, id int, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendErr != nil {
		return c.sendErr
	}
	c.sent = append(c.sent, sentText{id: id, text: text})
	return nil
}

func (c *fakeClient) Windows(context.Context) ([]kitty.Window, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.windowsErr != nil {
		return nil, c.windowsErr
	}
	// Before goto_session the session's tabs do not exist. Whatever a test puts
	// in `windows` is what the restore produced.
	if !c.restored {
		return c.pre, nil
	}
	if len(c.windows) == 0 {
		return nil, nil
	}
	i := min(c.windowCall, len(c.windows)-1)
	c.windowCall++
	return c.windows[i], nil
}

// fastPolling shrinks the readiness wait so a timeout test takes milliseconds.
func fastPolling(t *testing.T) {
	t.Helper()
	deadline, poll := readyDeadline, readyPoll
	readyDeadline, readyPoll = 60*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { readyDeadline, readyPoll = deadline, poll })
}

// --- Save ---------------------------------------------------------------------

func TestSave(t *testing.T) {
	t.Run("writes a rewritten snapshot and reports what it holds", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "agents.kitty-session")
		client := &fakeClient{native: string(fixture(t))}

		stats, err := Save(context.Background(), client, path)
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if stats.Tabs != 4 || stats.Resumable != 2 {
			t.Fatalf("stats: got %+v, want 4 tabs and 2 resumable", stats)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "AGENT_STATE") {
			t.Error("the installed snapshot still holds live agent state")
		}
		if !strings.Contains(string(data), "AGENT_RESUME") {
			t.Error("the installed snapshot lost its resume commands")
		}

		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != snapshotMode {
			t.Errorf("mode: got %o, want %o", info.Mode().Perm(), snapshotMode)
		}
	})

	t.Run("asks kitty for its own session file, as one string", func(t *testing.T) {
		dir := t.TempDir()
		client := &fakeClient{native: string(fixture(t))}

		if _, err := Save(context.Background(), client, filepath.Join(dir, "a.kitty-session")); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if len(client.actions) != 1 {
			t.Fatalf("actions: got %v, want one", client.actions)
		}
		action := client.actions[0]
		if !strings.HasPrefix(action, "save_as_session --save-only ") {
			t.Fatalf("action: got %q", action)
		}
		// Recording the running process makes a plain shell rerun its last
		// command on restore.
		if strings.Contains(action, "--use-foreground-process") {
			t.Fatal("save asked kitty to record the foreground process")
		}
	})

	t.Run("no temporary files survive a success", func(t *testing.T) {
		dir := t.TempDir()
		client := &fakeClient{native: string(fixture(t))}
		if _, err := Save(context.Background(), client, filepath.Join(dir, "a.kitty-session")); err != nil {
			t.Fatal(err)
		}
		if entries, _ := filepath.Glob(filepath.Join(dir, ".cattery-*")); len(entries) != 0 {
			t.Errorf("temporary files left behind: %v", entries)
		}
	})
}

func TestSaveKeepsPreviousSnapshot(t *testing.T) {
	t.Run("keeps the previous snapshot", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "agents.kitty-session")
		if err := os.WriteFile(path, []byte("old snapshot\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		client := &fakeClient{native: string(fixture(t))}

		if _, err := Save(context.Background(), client, path); err != nil {
			t.Fatalf("Save: %v", err)
		}

		prev, err := os.ReadFile(path + ".prev")
		if err != nil {
			t.Fatalf("no previous snapshot: %v", err)
		}
		if string(prev) != "old snapshot\n" {
			t.Fatalf("previous snapshot: got %q", prev)
		}
		if data, _ := os.ReadFile(path); string(data) == "old snapshot\n" {
			t.Fatal("the snapshot was not replaced")
		}
	})

	t.Run("a second save replaces the previous snapshot", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "agents.kitty-session")
		client := &fakeClient{native: "\nnew_tab\nlaunch\n"}

		for _, native := range []string{"\nnew_tab\nlaunch one\n", "\nnew_tab\nlaunch two\n"} {
			client.native = native
			if _, err := Save(context.Background(), client, path); err != nil {
				t.Fatalf("Save: %v", err)
			}
		}

		prev, err := os.ReadFile(path + ".prev")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(prev), "launch one") {
			t.Fatalf("previous snapshot: got %q, want the first save", prev)
		}
	})
}

// Every way a save can fail must leave the last good snapshot in place. The
// file exists to be there after a reboot.
func TestSaveFailures(t *testing.T) {
	t.Run("a kitty that refuses leaves the old snapshot alone", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "agents.kitty-session")
		if err := os.WriteFile(path, []byte("old snapshot\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		client := &fakeClient{actionErr: map[string]error{"save_as_session": errors.New("no listening socket")}}

		if _, err := Save(context.Background(), client, path); err == nil {
			t.Fatal("expected an error")
		} else if !strings.Contains(err.Error(), "no listening socket") {
			t.Fatalf("error lost kitty's reason: %v", err)
		}

		data, err := os.ReadFile(path)
		if err != nil || string(data) != "old snapshot\n" {
			t.Fatalf("the old snapshot changed: %q, %v", data, err)
		}
		if entries, _ := filepath.Glob(filepath.Join(dir, ".cattery-*")); len(entries) != 0 {
			t.Errorf("temporary files left behind: %v", entries)
		}
	})

	t.Run("a kitty that writes nothing leaves the old snapshot alone", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "agents.kitty-session")
		if err := os.WriteFile(path, []byte("old snapshot\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// kitty reports success and produces no file, so the rewrite has
		// nothing to read and the install must not start.
		client := &fakeClient{writeNever: true}

		if _, err := Save(context.Background(), client, path); err == nil {
			t.Fatal("expected an error")
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "old snapshot\n" {
			t.Fatalf("the old snapshot changed: %q, %v", data, err)
		}
		if _, err := os.Stat(path + ".prev"); err == nil {
			t.Error("rotated the old snapshot away for a save that failed")
		}
	})

	// Renaming would move the directory aside and leave a session file where it
	// used to be, so a save that names a directory must stop.
	t.Run("a directory is never replaced", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "agents.kitty-session")
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		client := &fakeClient{native: string(fixture(t))}

		if _, err := Save(context.Background(), client, path); err == nil {
			t.Fatal("expected an error")
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("the directory changed: %v, %v", info, err)
		}
		if _, err := os.Stat(path + ".prev"); err == nil {
			t.Error("moved the directory to .prev")
		}
	})
}

// --- Restore --------------------------------------------------------------------

// snapshot writes a session file holding two resumable windows.
func snapshot(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agents.kitty-session")
	body := "\nnew_tab\ncd /tmp\n" +
		"launch '--var=AGENT_RESUME=pi --session /tmp/a.jsonl'\n" +
		"\nnew_tab\ncd /tmp\n" +
		"launch '--var=AGENT_RESUME=claude --resume abc'\n" +
		"\nnew_tab\ncd /tmp\nlaunch\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// restored is the inventory after goto_session: two resumable windows, a plain
// window this restore created, and one the user opened earlier.
func restored(atPrompt bool) []kitty.Window {
	return []kitty.Window{
		{ID: 5, SessionName: "", UserVars: map[string]string{"AGENT_RESUME": "pi --session /other.jsonl"}},
		{ID: 11, SessionName: "agents", AtPrompt: atPrompt, UserVars: map[string]string{"AGENT_RESUME": "pi --session /tmp/a.jsonl"}},
		{ID: 12, SessionName: "agents", AtPrompt: false, UserVars: map[string]string{}},
		{ID: 13, SessionName: "agents", AtPrompt: atPrompt, UserVars: map[string]string{"AGENT_RESUME": "claude --resume abc"}},
	}
}

func TestRestore(t *testing.T) {
	t.Run("types each resume command without running it", func(t *testing.T) {
		path := snapshot(t)
		client := &fakeClient{windows: [][]kitty.Window{restored(true)}}

		stats, err := Restore(context.Background(), client, path, false)
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}

		want := []sentText{
			{id: 11, text: "pi --session /tmp/a.jsonl"},
			{id: 13, text: "claude --resume abc"},
		}
		if len(client.sent) != len(want) {
			t.Fatalf("sent %v, want %v", client.sent, want)
		}
		for i, w := range want {
			if client.sent[i] != w {
				t.Errorf("sent %d: got %+v, want %+v", i, client.sent[i], w)
			}
			if strings.HasSuffix(client.sent[i].text, "\r") {
				t.Errorf("sent %d ends in a carriage return, which runs it", i)
			}
		}
		if stats.Typed != 2 || stats.Resumable != 2 || stats.Tabs != 3 {
			t.Errorf("stats: got %+v, want 3 tabs, 2 resumable, 2 typed", stats)
		}
	})

	t.Run("-run presses return", func(t *testing.T) {
		path := snapshot(t)
		client := &fakeClient{windows: [][]kitty.Window{restored(true)}}

		if _, err := Restore(context.Background(), client, path, true); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		for _, s := range client.sent {
			if !strings.HasSuffix(s.text, "\r") {
				t.Errorf("window %d got %q with no carriage return", s.id, s.text)
			}
		}
	})

	t.Run("only the target session's windows are typed into", func(t *testing.T) {
		path := snapshot(t)
		client := &fakeClient{windows: [][]kitty.Window{restored(true)}}

		if _, err := Restore(context.Background(), client, path, false); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		for _, s := range client.sent {
			if s.id == 5 {
				t.Error("typed into a window this restore did not create")
			}
			if s.id == 12 {
				t.Error("typed into a window with no resume command")
			}
		}
	})

	t.Run("runs kitty's own action", func(t *testing.T) {
		path := snapshot(t)
		client := &fakeClient{windows: [][]kitty.Window{restored(true)}}

		if _, err := Restore(context.Background(), client, path, false); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if len(client.actions) != 1 || !strings.HasPrefix(client.actions[0], "goto_session ") {
			t.Fatalf("actions: got %v, want one goto_session", client.actions)
		}
	})
}

// A restored window is typed into once it draws a prompt, and again once the
// deadline passes whether it drew one or not.
func TestRestoreReadiness(t *testing.T) {
	t.Run("waits for a window to reach its prompt", func(t *testing.T) {
		fastPolling(t)
		path := snapshot(t)
		// Not ready, not ready, then ready.
		client := &fakeClient{windows: [][]kitty.Window{restored(false), restored(false), restored(true)}}

		if _, err := Restore(context.Background(), client, path, false); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if len(client.sent) != 2 {
			t.Fatalf("sent %v, want two commands", client.sent)
		}
		if client.windowCall < 3 {
			t.Errorf("polled %d times, want at least 3", client.windowCall)
		}
	})

	t.Run("a window that never reaches its prompt is typed into anyway", func(t *testing.T) {
		fastPolling(t)
		path := snapshot(t)
		client := &fakeClient{windows: [][]kitty.Window{restored(false)}}

		start := time.Now()
		stats, err := Restore(context.Background(), client, path, false)
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if stats.Typed != 2 {
			t.Fatalf("typed %d, want 2", stats.Typed)
		}
		if elapsed := time.Since(start); elapsed < readyDeadline {
			t.Errorf("gave up after %v, before the %v deadline", elapsed, readyDeadline)
		}
		for _, s := range client.sent {
			if strings.HasSuffix(s.text, "\r") {
				t.Error("the timeout path ran the command")
			}
		}
	})

	t.Run("a snapshot with nothing to resume waits for nobody", func(t *testing.T) {
		fastPolling(t)
		path := filepath.Join(t.TempDir(), "agents.kitty-session")
		if err := os.WriteFile(path, []byte("\nnew_tab\ncd /tmp\nlaunch\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		client := &fakeClient{windows: [][]kitty.Window{{{ID: 1, SessionName: "agents"}}}}

		start := time.Now()
		stats, err := Restore(context.Background(), client, path, false)
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if stats.Typed != 0 || len(client.sent) != 0 {
			t.Fatalf("typed into something: %+v", client.sent)
		}
		// Nothing to type means nothing to wait for. Without the snapshot's
		// count to stop on, the loop finds no window to declare ready and runs
		// the full deadline: three seconds of nothing on every such restore.
		if elapsed := time.Since(start); elapsed >= readyDeadline {
			t.Errorf("waited %v, the whole %v deadline, for a snapshot with no agents", elapsed, readyDeadline)
		}
	})

	// kitty builds the windows as it works through the session file, so a poll
	// can catch one agent ready before the next exists. Stopping there would
	// type into the windows that happened to exist and leave the rest at a bare
	// prompt.
	t.Run("a window that has not appeared yet is still waited for", func(t *testing.T) {
		fastPolling(t)
		path := snapshot(t) // two resumable windows
		half := []kitty.Window{
			{ID: 11, SessionName: "agents", AtPrompt: true, UserVars: map[string]string{"AGENT_RESUME": "pi --session /tmp/a.jsonl"}},
		}
		client := &fakeClient{windows: [][]kitty.Window{half, half, restored(true)}}

		stats, err := Restore(context.Background(), client, path, false)
		if err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if stats.Typed != 2 {
			t.Fatalf("typed %d of the snapshot's 2 resume commands", stats.Typed)
		}
		if len(client.sent) != 2 {
			t.Fatalf("sent %v, want both commands", client.sent)
		}
	})
}

func TestRestoreFailures(t *testing.T) {
	t.Run("restoring twice is refused", func(t *testing.T) {
		path := snapshot(t)
		client := &fakeClient{pre: []kitty.Window{{ID: 11, SessionName: "agents"}}}

		_, err := Restore(context.Background(), client, path, false)
		if err == nil {
			t.Fatal("expected a refusal")
		}
		// goto_session would build a second copy of every tab, so the message
		// must name the way out.
		if !strings.Contains(err.Error(), "close_session") {
			t.Errorf("error does not name close_session: %v", err)
		}
		if len(client.actions) != 0 {
			t.Errorf("changed tabs anyway: %v", client.actions)
		}
		if len(client.sent) != 0 {
			t.Errorf("typed anyway: %v", client.sent)
		}
	})

	t.Run("a missing snapshot fails before touching kitty", func(t *testing.T) {
		client := &fakeClient{}
		_, err := Restore(context.Background(), client, filepath.Join(t.TempDir(), "absent"), false)
		if err == nil {
			t.Fatal("expected an error")
		}
		if len(client.actions) != 0 {
			t.Errorf("ran an action anyway: %v", client.actions)
		}
	})

	t.Run("a kitty that refuses goto_session reports why", func(t *testing.T) {
		path := snapshot(t)
		client := &fakeClient{actionErr: map[string]error{"goto_session": errors.New("no listening socket")}}

		_, err := Restore(context.Background(), client, path, false)
		if err == nil || !strings.Contains(err.Error(), "no listening socket") {
			t.Fatalf("error: got %v, want kitty's reason", err)
		}
	})

	t.Run("one window that refuses text does not strand the others", func(t *testing.T) {
		path := snapshot(t)
		client := &fakeClient{windows: [][]kitty.Window{restored(true)}, sendErr: errors.New("no matching window")}

		stats, err := Restore(context.Background(), client, path, false)
		if err == nil || !strings.Contains(err.Error(), "no matching window") {
			t.Fatalf("error: got %v, want the send failure", err)
		}
		if stats.Typed != 0 {
			t.Errorf("typed: got %d, want 0", stats.Typed)
		}
	})
}

// --- summaries ----------------------------------------------------------------

// The CLI and the picker print the same sentence, so a snapshot reads the same
// whichever one took it.
func TestStatsSummaries(t *testing.T) {
	t.Run("saved", func(t *testing.T) {
		cases := []struct {
			stats Stats
			want  string
		}{
			{Stats{Tabs: 11, Resumable: 7}, "saved 11 tabs, 7 resumable agents -> /tmp/a.kitty-session"},
			{Stats{Tabs: 1, Resumable: 1}, "saved 1 tab, 1 resumable agent -> /tmp/a.kitty-session"},
			{Stats{}, "saved 0 tabs, 0 resumable agents -> /tmp/a.kitty-session"},
		}
		for _, tc := range cases {
			if got := tc.stats.Saved("/tmp/a.kitty-session"); got != tc.want {
				t.Errorf("Saved: got %q, want %q", got, tc.want)
			}
		}
	})

	t.Run("restored", func(t *testing.T) {
		cases := []struct {
			stats Stats
			run   bool
			want  string
		}{
			{Stats{Tabs: 11, Resumable: 7, Typed: 7}, false, "restored 11 tabs, typed 7 of 7 resume commands"},
			{Stats{Tabs: 11, Resumable: 7, Typed: 7}, true, "restored 11 tabs, ran 7 of 7 resume commands"},
			{Stats{Tabs: 1, Resumable: 1, Typed: 1}, false, "restored 1 tab, typed 1 of 1 resume commands"},
		}
		for _, tc := range cases {
			if got := tc.stats.Restored(tc.run); got != tc.want {
				t.Errorf("Restored: got %q, want %q", got, tc.want)
			}
		}
	})

	// A restore that passed its readiness deadline types fewer commands than
	// the snapshot holds and returns no error, so the summary has to show the
	// shortfall and the picker has to be able to report it.
	t.Run("a shortfall names both counts", func(t *testing.T) {
		short := Stats{Tabs: 11, Resumable: 7, Typed: 3}
		if got, want := short.Restored(false), "restored 11 tabs, typed 3 of 7 resume commands"; got != want {
			t.Errorf("Restored: got %q, want %q", got, want)
		}
		if !short.Incomplete() {
			t.Error("Incomplete: got false for 3 of 7")
		}
		if (Stats{Resumable: 7, Typed: 7}).Incomplete() {
			t.Error("Incomplete: got true for 7 of 7")
		}
	})
}

// A relative snapshot path would name three files. kitty's save_as_session
// resolves it against the kitty process's directory and goto_session against
// kitty's configuration directory, while cattery reads it next to itself.
func TestAbs(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Abs("work.kitty-session")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cwd, "work.kitty-session"); got != want {
		t.Fatalf("Abs: got %q, want %q", got, want)
	}

	if got, err := Abs("/tmp/a.kitty-session"); err != nil || got != "/tmp/a.kitty-session" {
		t.Fatalf("Abs of an absolute path: got %q, %v", got, err)
	}
}

// The default path is absolute whichever way it was chosen, because kitty sees
// it either way.
func TestDefaultPathIsAbsolute(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(envSessionFile, "work.kitty-session")
	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cwd, "work.kitty-session"); got != want {
		t.Fatalf("DefaultPath: got %q, want %q", got, want)
	}
}
