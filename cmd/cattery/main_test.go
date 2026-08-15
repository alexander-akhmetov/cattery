package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexander-akhmetov/cattery/internal/agent"
	"github.com/alexander-akhmetov/cattery/internal/kitty"
	"github.com/alexander-akhmetov/cattery/internal/tmux"
)

func TestRoute(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		want     string
		wantArgs []string
		wantErr  bool
	}{
		// The two forms that existed before subcommands.
		{name: "no arguments opens the picker", args: nil, want: cmdPicker},
		{name: "empty argument list", args: []string{}, want: cmdPicker},
		{name: "print", args: []string{"-print"}, want: cmdPrint},
		{name: "print, long form", args: []string{"--print"}, want: cmdPrint},
		{name: "print=false is still the picker", args: []string{"-print=false"}, want: cmdPicker},
		{name: "version", args: []string{"-version"}, want: cmdVersion},
		{name: "version, long form", args: []string{"--version"}, want: cmdVersion},
		{name: "version=false is still the picker", args: []string{"-version=false"}, want: cmdPicker},

		{name: "state working", args: []string{"state", "working"}, want: cmdState, wantArgs: []string{"working"}},
		{name: "state blocked", args: []string{"state", "blocked"}, want: cmdState, wantArgs: []string{"blocked"}},
		{name: "state idle", args: []string{"state", "idle"}, want: cmdState, wantArgs: []string{"idle"}},
		{name: "state clear", args: []string{"state", "clear"}, want: cmdState, wantArgs: []string{"clear"}},
		// The state writer ignores an unknown word; routing does not check it.
		{name: "state with an unknown word", args: []string{"state", "nonsense"}, want: cmdState, wantArgs: []string{"nonsense"}},
		{name: "state with no word", args: []string{"state"}, want: cmdState},

		{name: "setup", args: []string{"setup"}, want: cmdSetup},
		{
			name:     "setup keeps its own flags",
			args:     []string{"setup", "--dry-run", "--kitty-dir", "/tmp/k"},
			want:     cmdSetup,
			wantArgs: []string{"--dry-run", "--kitty-dir", "/tmp/k"},
		},

		{name: "attach", args: []string{"attach", "kontora:3.%17"}, want: cmdAttach, wantArgs: []string{"kontora:3.%17"}},
		// The target is checked where the attach runs, not here.
		{name: "attach with no target", args: []string{"attach"}, want: cmdAttach},

		{name: "events", args: []string{"events"}, want: cmdEvents},

		{name: "unknown subcommand", args: []string{"install"}, wantErr: true},
		{name: "unknown flag", args: []string{"-nope"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := route(tc.args, io.Discard)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("route(%v): got %+v, want an error", tc.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("route(%v): %v", tc.args, err)
			}
			if got.name != tc.want {
				t.Fatalf("route(%v): got %q, want %q", tc.args, got.name, tc.want)
			}
			if len(got.args) != len(tc.wantArgs) {
				t.Fatalf("route(%v) args: got %v, want %v", tc.args, got.args, tc.wantArgs)
			}
			for i := range tc.wantArgs {
				if got.args[i] != tc.wantArgs[i] {
					t.Fatalf("route(%v) arg %d: got %q, want %q", tc.args, i, got.args[i], tc.wantArgs[i])
				}
			}
		})
	}
}

// -h is not a routing failure. It asked for the usage text and got it.
func TestRouteHelp(t *testing.T) {
	var out strings.Builder
	if _, err := route([]string{"-h"}, &out); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("route(-h): got %v, want flag.ErrHelp", err)
	}
	if !strings.Contains(out.String(), "-print") {
		t.Fatalf("usage does not mention -print:\n%s", out.String())
	}
}

// A build that nobody stamped still has to print something. `go install` leaves
// the module version in the build info, and a plain `go build` leaves nothing.
func TestVersionString(t *testing.T) {
	previous := version
	t.Cleanup(func() { version = previous })

	t.Run("the stamped value wins", func(t *testing.T) {
		version = "v1.2.3"

		if got := versionString(); got != "v1.2.3" {
			t.Fatalf("got %q, want %q", got, "v1.2.3")
		}
	})

	t.Run("an unstamped build prints something", func(t *testing.T) {
		version = ""

		if got := versionString(); got == "" {
			t.Fatal("got an empty version")
		}
	})
}

// --- save and restore -----------------------------------------------------

func TestRouteSessionCommands(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		want     string
		wantArgs []string
	}{
		{name: "save", args: []string{"save"}, want: cmdSave},
		{name: "restore", args: []string{"restore"}, want: cmdRestore},
		{name: "save with a path", args: []string{"save", "/tmp/a.kitty-session"}, want: cmdSave, wantArgs: []string{"/tmp/a.kitty-session"}},
		{
			name:     "restore with a path and a flag after it",
			args:     []string{"restore", "/tmp/a.kitty-session", "-run"},
			want:     cmdRestore,
			wantArgs: []string{"/tmp/a.kitty-session", "-run"},
		},
		{
			name:     "restore with the flag first",
			args:     []string{"restore", "-run", "/tmp/a.kitty-session"},
			want:     cmdRestore,
			wantArgs: []string{"-run", "/tmp/a.kitty-session"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := route(tc.args, io.Discard)
			if err != nil {
				t.Fatalf("route(%v): %v", tc.args, err)
			}
			if got.name != tc.want {
				t.Fatalf("route(%v): got %q, want %q", tc.args, got.name, tc.want)
			}
			if !slices.Equal(got.args, tc.wantArgs) {
				t.Fatalf("route(%v) args: got %v, want %v", tc.args, got.args, tc.wantArgs)
			}
		})
	}
}

// The subcommands come before the picker's own flags, and both forms of the old
// command line still work.
func TestRouteStillPrefersTheExistingEntryPoints(t *testing.T) {
	for _, args := range [][]string{nil, {"-print"}} {
		got, err := route(args, io.Discard)
		if err != nil {
			t.Fatalf("route(%v): %v", args, err)
		}
		if got.name == cmdSave || got.name == cmdRestore {
			t.Fatalf("route(%v): got %q", args, got.name)
		}
	}
}

func TestParseSessionArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		env     string
		want    string
		wantRel string // expected path, relative to the test's own directory
		wantRun bool
		wantErr bool
	}{
		{
			name: "an explicit path wins over the environment",
			args: []string{"/tmp/explicit.kitty-session"},
			env:  "/tmp/default.kitty-session",
			want: "/tmp/explicit.kitty-session",
		},
		{
			name: "the environment wins over the default",
			env:  "/tmp/work.kitty-session",
			want: "/tmp/work.kitty-session",
		},
		{
			name:    "a flag after the path is still a flag",
			args:    []string{"/tmp/a.kitty-session", "-run"},
			want:    "/tmp/a.kitty-session",
			wantRun: true,
		},
		{
			name:    "a flag before the path",
			args:    []string{"-run", "/tmp/a.kitty-session"},
			want:    "/tmp/a.kitty-session",
			wantRun: true,
		},
		{
			name:    "a relative path is taken as a path, not looked up as a name",
			args:    []string{"work.kitty-session"},
			wantRel: "work.kitty-session",
		},
		{
			name:    "a relative CATTERY_SESSION_FILE is resolved too",
			env:     "work.kitty-session",
			wantRel: "work.kitty-session",
		},
		{name: "two paths", args: []string{"/tmp/a", "/tmp/b"}, wantErr: true},
		{name: "an unknown flag", args: []string{"-nope"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("CATTERY_SESSION_FILE", tc.env)
			}
			flags := flag.NewFlagSet("test", flag.ContinueOnError)
			flags.SetOutput(io.Discard)
			run := flags.Bool("run", false, "")

			got, err := parseSessionArgs(flags, tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSessionArgs(%v): %v", tc.args, err)
			}
			// Every path comes back absolute. kitty resolves a relative one
			// against directories of its own, so one string would name three
			// files: cattery's, kitty's working directory, and kitty's
			// configuration directory.
			want := tc.want
			if tc.wantRel != "" {
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				want = filepath.Join(cwd, tc.wantRel)
			}
			if got != want {
				t.Fatalf("path: got %q, want %q", got, want)
			}
			if *run != tc.wantRun {
				t.Fatalf("-run: got %v, want %v", *run, tc.wantRun)
			}
		})
	}
}

// With no path and no environment variable, the snapshot goes under kitty's own
// sessions directory, where `goto_session <dir>` looks for one.
func TestDefaultSnapshotPath(t *testing.T) {
	t.Setenv("CATTERY_SESSION_FILE", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	flags := flag.NewFlagSet("test", flag.ContinueOnError)

	got, err := parseSessionArgs(flags, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".local", "share", "kitty", "sessions", "agents.kitty-session")
	if got != want {
		t.Fatalf("default path: got %q, want %q", got, want)
	}
}

// cliClient is a kitty that writes a two-tab session with one resumable agent.
type cliClient struct {
	actions   []string
	actionErr error
	sent      []string
	restored  bool
}

const cliNative = "\nnew_tab\ncd /tmp\nlaunch '--var=AGENT_RESUME=pi --session /tmp/a.jsonl'\n" +
	"\nnew_tab\ncd /tmp\nlaunch\n"

func (c *cliClient) Action(_ context.Context, arg string) error {
	c.actions = append(c.actions, arg)
	if c.actionErr != nil {
		return c.actionErr
	}
	if path, ok := strings.CutPrefix(arg, "save_as_session --save-only "); ok {
		return os.WriteFile(strings.Trim(path, "'"), []byte(cliNative), 0o600)
	}
	if strings.HasPrefix(arg, "goto_session ") {
		c.restored = true
	}
	return nil
}

func (c *cliClient) SendText(_ context.Context, _ int, text string) error {
	c.sent = append(c.sent, text)
	return nil
}

func (c *cliClient) Windows(context.Context) ([]kitty.Window, error) {
	if !c.restored {
		return nil, nil
	}
	return []kitty.Window{
		{ID: 9, SessionName: "agents", AtPrompt: true, UserVars: map[string]string{"AGENT_RESUME": "pi --session /tmp/a.jsonl"}},
	}, nil
}

func TestRunSave(t *testing.T) {
	t.Run("writes the snapshot and prints one summary line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "agents.kitty-session")
		client := &cliClient{}
		var out strings.Builder

		if code := runSave(client, &out, []string{path}); code != 0 {
			t.Fatalf("exit code %d", code)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("no snapshot: %v", err)
		}
		lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
		if len(lines) != 1 {
			t.Fatalf("printed %d lines, want 1: %q", len(lines), out.String())
		}
		for _, want := range []string{"2 tabs", "1 resumable agent", path} {
			if !strings.Contains(lines[0], want) {
				t.Errorf("summary %q is missing %q", lines[0], want)
			}
		}
	})

	t.Run("creates the directory the snapshot goes in", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nested", "deeper", "agents.kitty-session")
		var out strings.Builder

		if code := runSave(&cliClient{}, &out, []string{path}); code != 0 {
			t.Fatalf("exit code %d: %s", code, out.String())
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("no snapshot: %v", err)
		}
	})

	t.Run("uses the environment path when none is given", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "work.kitty-session")
		t.Setenv("CATTERY_SESSION_FILE", path)
		var out strings.Builder

		if code := runSave(&cliClient{}, &out, nil); code != 0 {
			t.Fatalf("exit code %d: %s", code, out.String())
		}
		if !strings.Contains(out.String(), path) {
			t.Errorf("summary %q does not name %q", out.String(), path)
		}
	})

	t.Run("a kitty that refuses exits non-zero and prints no summary", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "agents.kitty-session")
		client := &cliClient{actionErr: errors.New("no listening socket")}
		var out strings.Builder

		if code := runSave(client, &out, []string{path}); code != 1 {
			t.Fatalf("exit code: got %d, want 1", code)
		}
		if out.String() != "" {
			t.Errorf("printed a summary for a failed save: %q", out.String())
		}
	})
}

func TestRunRestore(t *testing.T) {
	// snapshot writes a file runRestore can read back.
	snapshot := func(t *testing.T) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "agents.kitty-session")
		if err := os.WriteFile(path, []byte(cliNative), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("types the resume command and prints one summary line", func(t *testing.T) {
		client := &cliClient{}
		var out strings.Builder

		if code := runRestore(client, &out, []string{snapshot(t)}); code != 0 {
			t.Fatalf("exit code %d: %s", code, out.String())
		}
		lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
		if len(lines) != 1 {
			t.Fatalf("printed %d lines, want 1: %q", len(lines), out.String())
		}
		for _, want := range []string{"2 tabs", "typed 1 of 1"} {
			if !strings.Contains(lines[0], want) {
				t.Errorf("summary %q is missing %q", lines[0], want)
			}
		}
		if len(client.sent) != 1 || strings.HasSuffix(client.sent[0], "\r") {
			t.Errorf("sent %q, want one command with no carriage return", client.sent)
		}
	})

	t.Run("-run after the path presses return", func(t *testing.T) {
		client := &cliClient{}
		var out strings.Builder

		if code := runRestore(client, &out, []string{snapshot(t), "-run"}); code != 0 {
			t.Fatalf("exit code %d: %s", code, out.String())
		}
		if len(client.sent) != 1 || !strings.HasSuffix(client.sent[0], "\r") {
			t.Fatalf("sent %q, want a trailing carriage return", client.sent)
		}
		if !strings.Contains(out.String(), "ran 1 of 1") {
			t.Errorf("summary %q does not say the commands ran", out.String())
		}
	})

	t.Run("a missing snapshot exits non-zero", func(t *testing.T) {
		var out strings.Builder
		code := runRestore(&cliClient{}, &out, []string{filepath.Join(t.TempDir(), "absent")})
		if code != 1 {
			t.Fatalf("exit code: got %d, want 1", code)
		}
		if out.String() != "" {
			t.Errorf("printed a summary for a failed restore: %q", out.String())
		}
	})

	t.Run("an unknown flag is a usage error", func(t *testing.T) {
		var out strings.Builder
		if code := runRestore(&cliClient{}, &out, []string{"-nope"}); code != 2 {
			t.Fatalf("exit code: got %d, want 2", code)
		}
	})
}

// --- print ------------------------------------------------------------------

// printLister stands in for the merged inventory.
type printLister struct {
	agents []agent.Agent
	err    error
}

func (p printLister) ListAgents(context.Context) ([]agent.Agent, error) { return p.agents, p.err }

// A printed row says which host the agent runs in, and a tmux row carries the
// target `cattery attach` takes. `cattery -print` is how the contract is
// checked by hand when the picker shows nothing.
func TestPrintAgents(t *testing.T) {
	var out strings.Builder
	err := printAgents(printLister{agents: []agent.Agent{
		{
			ID: 17, Host: agent.HostTmux, Kind: "claude", Display: "working",
			Project: "astra-l", Branch: "kontora/al-67je",
			CWD:    "/Users/x/.kontora/worktrees/astra-l/al-67je",
			Target: "kontora:3.%17",
		},
		{
			ID: 12, Host: agent.HostKitty, Kind: "pi", Display: "idle",
			Project: "dotfiles", Branch: "main", CWD: "/Users/x/projects/dotfiles",
		},
	}}, &out)
	if err != nil {
		t.Fatalf("print: %v", err)
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), out.String())
	}
	for _, want := range []string{"host=tmux", "id=17", "target=kontora:3.%17", "astra-l", "working"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("tmux row %q is missing %q", lines[0], want)
		}
	}
	if !strings.Contains(lines[1], "host=kitty") || strings.Contains(lines[1], "target=") {
		t.Errorf("kitty row %q should name its host and no target", lines[1])
	}
}

// One host can fail while the other answers. Its rows are the whole inventory
// on a machine with no kitty running, so they print and the failure still comes
// back for the exit code.
func TestPrintAgentsPartialFailure(t *testing.T) {
	var out strings.Builder
	err := printAgents(printLister{
		agents: []agent.Agent{{ID: 17, Host: agent.HostTmux, Display: "working", Target: "kontora:3.%17"}},
		err:    errors.New("kitty: no listening socket"),
	}, &out)

	if err == nil {
		t.Fatal("expected the lister failure to come back")
	}
	if !strings.Contains(out.String(), "host=tmux") {
		t.Errorf("dropped the rows the working host returned: %q", out.String())
	}
}

// --- events -------------------------------------------------------------------

// eventsClient stands in for the running kitty. Registration is the first thing
// the subscriber does, so refusing it is what ends the command.
type eventsClient struct{ err error }

func (c eventsClient) Kitten(context.Context, string, ...string) (string, error) {
	return "", c.err
}

func TestRunEvents(t *testing.T) {
	t.Run("an unknown flag is a usage error", func(t *testing.T) {
		if got := runEvents(eventsClient{}, io.Discard, []string{"-nope"}); got != 2 {
			t.Fatalf("exit code: got %d, want 2", got)
		}
	})

	t.Run("an argument is a usage error", func(t *testing.T) {
		// It takes none, and a typed word is more likely a subcommand somebody
		// expected than something to ignore.
		if got := runEvents(eventsClient{}, io.Discard, []string{"register"}); got != 2 {
			t.Fatalf("exit code: got %d, want 2", got)
		}
	})

	t.Run("a kitty that refuses the registration exits non-zero", func(t *testing.T) {
		client := eventsClient{err: errors.New("no listening socket")}

		if got := runEvents(client, io.Discard, nil); got != 1 {
			t.Fatalf("exit code: got %d, want 1", got)
		}
	})
}

// --- attach -----------------------------------------------------------------

// A target is required, and one that is not <session>:<window>.<pane id>
// never reaches tmux.
func TestRunAttachArguments(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{name: "no target", args: nil, want: 2},
		{name: "an empty target", args: []string{""}, want: 2},
		{name: "two targets", args: []string{"kontora:3.%17", "kontora:4.%18"}, want: 2},
		{name: "a target with no window index", args: []string{"kontora"}, want: 1},
		{name: "a target with no pane id", args: []string{"kontora:3"}, want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runAttach(tmux.NewClient(), tc.args); got != tc.want {
				t.Fatalf("exit code: got %d, want %d", got, tc.want)
			}
		})
	}
}
