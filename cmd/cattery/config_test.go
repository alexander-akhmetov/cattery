package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configFile(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	for _, name := range []string{"CATTERY_PREVIEW", "CATTERY_SESSION_FILE", "CATTERY_RESUME_PREFIX", "CATTERY_RESUME_PREFIX_CLAUDE", "CATTERY_RESUME_PREFIX_CODEX", "CATTERY_RESUME_PREFIX_OPENCODE", "CATTERY_RESUME_PREFIX_PI"} {
		t.Setenv(name, "")
	}
	path := filepath.Join(home, "config", "cattery", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunConfig(t *testing.T) {
	cases := []struct {
		name string
		args []string
		file string
		env  string
		want string
		code int
	}{
		{name: "pi default", args: []string{"resume-prefix", "pi"}, want: "pi"},
		{name: "claude", args: []string{"resume-prefix", "claude"}, file: "[resume]\nclaude = 'claude --profile personal'", want: "claude --profile personal"},
		{name: "codex", args: []string{"resume-prefix", "codex"}, file: "[resume]\ncodex = 'nono run codex'", want: "nono run codex"},
		{name: "opencode", args: []string{"resume-prefix", "opencode"}, file: "[resume]\nopencode = 'nono run opencode'", want: "nono run opencode"},
		{name: "pi file", args: []string{"resume-prefix", "pi"}, file: "[resume]\npi = '''env PROFILE='a b' pi'''", want: "env PROFILE='a b' pi"},
		{name: "environment wins", args: []string{"resume-prefix", "pi"}, file: "[resume]\npi = 'file pi'", env: "env pi", want: "env pi"},
		{name: "invalid configuration", args: []string{"resume-prefix", "pi"}, file: "[resume]\npi = 3", code: 1},
		{name: "no arguments", code: 2},
		{name: "unknown action", args: []string{"show", "pi"}, code: 2},
		{name: "missing kind", args: []string{"resume-prefix"}, code: 2},
		{name: "unknown kind", args: []string{"resume-prefix", "unknown"}, code: 2},
		{name: "extra argument", args: []string{"resume-prefix", "pi", "extra"}, code: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := configFile(t, tc.file)
			t.Setenv("CATTERY_RESUME_PREFIX", tc.env)
			t.Setenv("KITTY_WINDOW_ID", "")
			t.Setenv("KITTY_LISTEN_ON", "")
			var out, diagnostic strings.Builder
			if code := runConfig(&out, &diagnostic, tc.args); code != tc.code {
				t.Fatalf("exit %d, want %d: %s", code, tc.code, &diagnostic)
			}
			if tc.code != 0 {
				if out.Len() != 0 || diagnostic.Len() == 0 {
					t.Fatalf("stdout %q, diagnostic %q", &out, &diagnostic)
				}
				if tc.code == 1 && !strings.Contains(diagnostic.String(), path) {
					t.Fatalf("diagnostic omits config path: %s", &diagnostic)
				}
				return
			}
			var got string
			decoder := json.NewDecoder(strings.NewReader(out.String()))
			if err := decoder.Decode(&got); err != nil || got != tc.want {
				t.Fatalf("output %q, want JSON string %q: %v", &out, tc.want, err)
			}
			if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) || diagnostic.Len() != 0 {
				t.Fatalf("extra output: %s / %s", &out, &diagnostic)
			}
		})
	}
}

func TestPickerOptions(t *testing.T) {
	cases := []struct {
		name string
		file string
		env  string
		want bool
		err  bool
	}{
		{name: "default"},
		{name: "file enabled", file: "[picker]\npreview = true", want: true},
		{name: "explicit false", file: "[picker]\npreview = false"},
		{name: "environment opens", file: "[picker]\npreview = false", env: "1", want: true},
		{name: "environment closes", file: "[picker]\npreview = true", env: "0"},
		{name: "only one enables", file: "[picker]\npreview = true", env: "true"},
		{name: "malformed", file: "[picker", err: true},
		{name: "invalid type", file: "[picker]\npreview = 'yes'", err: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := configFile(t, tc.file)
			t.Setenv("CATTERY_PREVIEW", tc.env)
			options, err := pickerOptions()
			if tc.err {
				if err == nil || !strings.Contains(err.Error(), path) {
					t.Fatalf("configuration error: %v", err)
				}
				if code := run(nil); code != 1 {
					t.Fatalf("picker exit %d, want 1 before starting the terminal", code)
				}
				return
			}
			if err != nil || options.Preview != tc.want {
				t.Fatalf("options %+v, error %v", options, err)
			}
		})
	}
}
