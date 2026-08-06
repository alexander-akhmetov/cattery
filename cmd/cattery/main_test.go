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

	"github.com/alexander-akhmetov/cattery/internal/kitty"
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
