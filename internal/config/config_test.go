package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolateEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{
		"XDG_CONFIG_HOME", "CATTERY_PREVIEW", "CATTERY_SESSION_FILE",
		"CATTERY_RESUME_PREFIX", "CATTERY_RESUME_PREFIX_CLAUDE",
		"CATTERY_RESUME_PREFIX_CODEX", "CATTERY_RESUME_PREFIX_OPENCODE",
		"CATTERY_RESUME_PREFIX_PI",
	} {
		t.Setenv(name, "")
	}
	return home
}

func writeConfig(t *testing.T, dir, text string) string {
	t.Helper()
	path := filepath.Join(dir, "cattery", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadText(t *testing.T, text string) Config {
	t.Helper()
	writeConfig(t, filepath.Join(os.Getenv("HOME"), ".config"), text)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLoadDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name string
		xdg  string
		want string
	}{
		{"unset XDG", "unset", "home"},
		{"empty XDG", "", "home"},
		{"relative XDG", "relative", "home"},
		{"dot XDG", ".", "home"},
		{"tilde XDG is not absolute", "~/config", "home"},
		{"absolute XDG", "absolute", "xdg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolateEnv(t)
			cwd := t.TempDir()
			t.Chdir(cwd)
			xdg := t.TempDir()
			homeDir := filepath.Join(home, ".config")
			writeConfig(t, homeDir, "[resume]\nprefix = 'home'\n[session]\nfile = 'snapshot'")
			writeConfig(t, xdg, "[resume]\nprefix = 'xdg'\n[session]\nfile = 'snapshot'")
			writeConfig(t, filepath.Join(cwd, "relative"), "[resume]\nprefix = 'wrong'")
			switch tc.xdg {
			case "unset":
				if err := os.Unsetenv("XDG_CONFIG_HOME"); err != nil {
					t.Fatal(err)
				}
			case "absolute":
				t.Setenv("XDG_CONFIG_HOME", xdg)
			default:
				t.Setenv("XDG_CONFIG_HOME", tc.xdg)
			}
			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if c.Resume.Prefix != tc.want {
				t.Fatalf("prefix = %q, want %q", c.Resume.Prefix, tc.want)
			}
			dir := homeDir
			if tc.want == "xdg" {
				dir = xdg
			}
			want := filepath.Join(dir, "cattery", "snapshot")
			got, err := c.SessionPath()
			if err != nil || got != want {
				t.Fatalf("SessionPath() = %q, %v; want %q", got, err, want)
			}
		})
	}
}

func TestLoadMissing(t *testing.T) {
	for _, useXDG := range []bool{false, true} {
		t.Run(fmt.Sprintf("XDG=%t", useXDG), func(t *testing.T) {
			home := isolateEnv(t)
			missing := filepath.Join(home, ".config")
			if useXDG {
				missing = filepath.Join(t.TempDir(), "missing")
				t.Setenv("XDG_CONFIG_HOME", missing)
				writeConfig(t, filepath.Join(home, ".config"), "invalid TOML: no fallback")
			}
			c, err := Load()
			if err != nil || c != (Config{}) {
				t.Fatalf("Load() = %+v, %v; want zero config and no error", c, err)
			}
			path, err := c.SessionPath()
			if err != nil {
				t.Fatal(err)
			}
			for _, absent := range []string{missing, filepath.Dir(path)} {
				if _, err := os.Stat(absent); !os.IsNotExist(err) {
					t.Fatalf("%q: stat error = %v, want not exist", absent, err)
				}
			}
		})
	}
}

func TestLoadStrict(t *testing.T) {
	for _, tc := range []struct{ name, text, detail string }{
		{"unknown root", "extra = true", `unknown key "extra" at line 2, column 1`},
		{"unknown table", "[extra]\nvalue = true", `unknown key "extra" at line 2, column 2`},
		{"unknown empty table", "[extra]", `unknown key "extra" at line 2, column 2`},
		{"unknown empty nested table", "[resume.extra]", `unknown key "resume.extra" at line 2, column 2`},
		{"picker table type", "picker = false", ""},
		{"session table type", "session = []", ""},
		{"resume table type", "resume = 'pi'", ""},
		{"unknown picker key", "[picker]\nprevew = true", `unknown key "picker.prevew" at line 3, column 1`},
		{"unknown session key", "[session]\npath = 'file'", `unknown key "session.path" at line 3, column 1`},
		{"unknown resume key", "[resume]\ncluade = 'wrapper'", `unknown key "resume.cluade" at line 3, column 1`},
		{"unknown inline key", "picker = { preview = true, extra = false }", `unknown key "extra" at line 2, column 28`},
		{"unknown indented key", "[picker]\n  prevew = true", `unknown key "picker.prevew" at line 3, column 3`},
		{"unknown dotted key", "picker.prevew = true", `unknown key "picker.prevew" at line 2, column 1`},
		{"multiple unknowns", "[picker]\nprevew = true\n[resume]\ncluade = 'wrapper'", `unknown key "picker.prevew" at line 3, column 1; unknown key "resume.cluade" at line 5, column 1`},
		{"preview type", "[picker]\npreview = 'false'", ""},
		{"session type", "[session]\nfile = 42", ""},
		{"resume type", "[resume]\npi = false", ""},
		{"resume integer", "[resume]\npi = 42", ""},
		{"resume float", "[resume]\npi = 1.5", ""},
		{"resume date", "[resume]\npi = 2026-01-01", ""},
		{"resume array", "[resume]\npi = ['wrapper']", ""},
		{"resume inline table", "[resume]\npi = { value = 'wrapper' }", ""},
		{"resume nested key", "[resume]\npi.extra = 'wrapper'", ""},
		{"resume nested table", "[resume.pi]\nextra = 'wrapper'", ""},
		{"syntax", "[resume", ""},
		{"duplicate key", "[resume]\npi = 'one'\npi = 'two'", ""},
		{"duplicate table", "[resume]\n[resume]", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolateEnv(t)
			partial := "session = { file = 'partial' }\n"
			if strings.Contains(tc.text, "[session]") {
				partial = "picker = { preview = true }\n"
			}
			path := writeConfig(t, filepath.Join(home, ".config"), partial+tc.text)
			// Environment overrides must not hide an invalid config file.
			t.Setenv("CATTERY_PREVIEW", "1")
			t.Setenv("CATTERY_SESSION_FILE", "/override")
			t.Setenv("CATTERY_RESUME_PREFIX", "override")
			c, err := Load()
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("error = %v, want selected path %q", err, path)
			}
			if tc.detail != "" {
				want := fmt.Sprintf("decode config %q: %s", path, tc.detail)
				if err.Error() != want {
					t.Fatalf("error = %q, want %q", err, want)
				}
			}
			if c != (Config{}) {
				t.Fatalf("invalid config leaked partial data: %+v", c)
			}
		})
	}
}

func TestLoadReadError(t *testing.T) {
	for _, useXDG := range []bool{false, true} {
		t.Run(fmt.Sprintf("XDG=%t", useXDG), func(t *testing.T) {
			home := isolateEnv(t)
			dir := filepath.Join(home, ".config")
			if useXDG {
				dir = t.TempDir()
				t.Setenv("XDG_CONFIG_HOME", dir)
				writeConfig(t, filepath.Join(home, ".config"), "[resume]\nprefix = 'no fallback'")
			}
			path := filepath.Join(dir, "cattery", "config.toml")
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
			c, err := Load()
			if err == nil || !strings.Contains(err.Error(), path) || c != (Config{}) {
				t.Fatalf("Load() = %+v, %v; want zero config and error naming %q", c, err, path)
			}
		})
	}
}

func TestLoadHomeUnavailable(t *testing.T) {
	isolateEnv(t)
	t.Setenv("HOME", "")
	if c, err := Load(); err == nil || c != (Config{}) {
		t.Fatalf("Load() = %+v, %v; want zero config and error", c, err)
	}
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	writeConfig(t, xdg, "[picker]\npreview = false")
	if _, err := Load(); err != nil {
		t.Fatalf("absolute XDG must not need HOME: %v", err)
	}
}

func TestLoadControls(t *testing.T) {
	for _, key := range []string{"prefix", "claude", "codex", "opencode", "pi"} {
		t.Run(key, func(t *testing.T) {
			home := isolateEnv(t)
			for r := rune(0); r <= 0x9f; r++ {
				if r > 0x1f && r < 0x7f {
					continue
				}
				for _, overwrite := range []bool{false, true} {
					t.Run(fmt.Sprintf("U+%04X/overwrite=%t", r, overwrite), func(t *testing.T) {
						text := fmt.Sprintf("[picker]\npreview = true\n[resume]\n%s = \"wrap\\u%04Xend\"", key, r)
						if overwrite {
							text += fmt.Sprintf("\n%s = 'valid'", strings.ToUpper(key))
						}
						path := writeConfig(t, filepath.Join(home, ".config"), text)
						t.Setenv("CATTERY_RESUME_PREFIX", "override")
						t.Setenv("CATTERY_RESUME_PREFIX_"+strings.ToUpper(key), "override")
						c, err := Load()
						if err == nil || !strings.Contains(err.Error(), path) || c != (Config{}) {
							t.Fatalf("Load() = %+v, %v; want zero config and error naming %q", c, err, path)
						}
					})
				}
			}
		})
	}
	for _, text := range []string{
		"[resume]\nprefix = '''first\nsecond'''",
		"[resume]\npi = 'literal\ttab'",
		"[resume]\nclaude = 'literal\u0085control'",
		"[resume]\npi = \"bad\\ncommand\"\nPI = 'valid pi'",
		"[resume]\nPI = 'valid pi'\npi = \"bad\\ncommand\"",
		"[resume]\npi = \"bad\\ncommand\"\n[RESUME]\npi = 'valid pi'",
		"resume = { pi = \"bad\\ncommand\", PI = 'valid pi' }",
		"resume.pi = \"bad\\ncommand\"\nRESUME.PI = 'valid pi'",
		"[resume]\npi = \"bad\\ncommand\"\n[RESUME]\nprefix = 'valid shared'",
	} {
		t.Run(text, func(t *testing.T) {
			isolateEnv(t)
			writeConfig(t, filepath.Join(os.Getenv("HOME"), ".config"), text)
			if c, err := Load(); err == nil || c != (Config{}) {
				t.Fatalf("Load() = %+v, %v; want zero config and error", c, err)
			}
		})
	}
}

func TestPreview(t *testing.T) {
	for _, file := range []string{"", "[picker]\npreview = false", "[picker]\npreview = true"} {
		for _, env := range []string{"unset", "", "1", "0", "false", "true", "01", " 1", "1 "} {
			t.Run(fmt.Sprintf("file=%q/env=%q", file, env), func(t *testing.T) {
				isolateEnv(t)
				c := loadText(t, file)
				if env == "unset" {
					if err := os.Unsetenv("CATTERY_PREVIEW"); err != nil {
						t.Fatal(err)
					}
				} else {
					t.Setenv("CATTERY_PREVIEW", env)
				}
				want := env == "1" || ((env == "" || env == "unset") && strings.HasSuffix(file, "true"))
				if got := c.Preview(); got != want {
					t.Fatalf("Preview() = %t, want %t", got, want)
				}
				if (c.Picker.Preview == nil) != (file == "") {
					t.Fatalf("preview pointer = %v for %q", c.Picker.Preview, file)
				}
			})
		}
	}
}

func TestResumePrefix(t *testing.T) {
	for _, kind := range []string{"claude", "codex", "opencode", "pi"} {
		for _, tc := range []struct {
			name, specificEnv, sharedEnv, specificFile, sharedFile, want string
		}{
			{"specific env", "env-specific", "env-shared", "file-specific", "file-shared", "env-specific"},
			{"shared env over specific file", "", "env-shared", "file-specific", "file-shared", "env-shared"},
			{"specific file", "", "", "file-specific", "file-shared", "file-specific"},
			{"shared file", "", "", "", "file-shared", "file-shared"},
			{"kind", "", "", "", "", kind},
			{"whitespace env is nonempty", " ", "env-shared", "file-specific", "file-shared", " "},
			{"whitespace file is nonempty", "", "", " ", "file-shared", " "},
			{"env controls are not file validation", "wrapper\targ", "", "", "", "wrapper\targ"},
			{"valid aliases", "", "", "first", "shared", "last"},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				isolateEnv(t)
				text := fmt.Sprintf("[resume]\nprefix = '%s'\n%s = '%s'", tc.sharedFile, kind, tc.specificFile)
				if tc.name == "valid aliases" {
					text += fmt.Sprintf("\n%s = 'last'", strings.ToUpper(kind))
				}
				c := loadText(t, text)
				for _, other := range []string{"claude", "codex", "opencode", "pi"} {
					t.Setenv("CATTERY_RESUME_PREFIX_"+strings.ToUpper(other), "wrong kind")
				}
				t.Setenv("CATTERY_RESUME_PREFIX_"+strings.ToUpper(kind), tc.specificEnv)
				t.Setenv("CATTERY_RESUME_PREFIX", tc.sharedEnv)
				if got := c.ResumePrefix(kind); got != tc.want {
					t.Fatalf("ResumePrefix(%q) = %q, want %q", kind, got, tc.want)
				}
			})
		}
	}
}

func TestResumePrefixUnknownKind(t *testing.T) {
	isolateEnv(t)
	t.Setenv("CATTERY_RESUME_PREFIX_UNKNOWN", "not a supported override")
	var c Config
	for _, kind := range []string{"unknown", "", "CLAUDE"} {
		if got := c.ResumePrefix(kind); got != kind {
			t.Fatalf("ResumePrefix(%q) = %q", kind, got)
		}
	}
	c.Resume.Prefix = "file-shared"
	if got := c.ResumePrefix("unknown"); got != "file-shared" {
		t.Fatal(got)
	}
	t.Setenv("CATTERY_RESUME_PREFIX", "env-shared")
	if got := c.ResumePrefix("unknown"); got != "env-shared" {
		t.Fatal(got)
	}
}

func TestLoadShellFragments(t *testing.T) {
	isolateEnv(t)
	fragment := `env NAME="$HOME" nono run --profile 'a b' -- wrapper; echo $(whoami) | cat && next > "$OUTPUT"`
	c := loadText(t, "[resume]\nprefix = '''"+fragment+"'''")
	if got := c.ResumePrefix("pi"); got != fragment {
		t.Fatalf("ResumePrefix() = %q, want %q", got, fragment)
	}
	t.Setenv("CATTERY_RESUME_PREFIX_PI", fragment)
	if got := c.ResumePrefix("pi"); got != fragment {
		t.Fatalf("env ResumePrefix() = %q, want %q", got, fragment)
	}
}

func TestSessionPath(t *testing.T) {
	for _, tc := range []struct{ name, env, file, base, suffix string }{
		{"env absolute", "/env/snapshot", "/file/snapshot", "", "/env/snapshot"},
		{"env relative", "env/snapshot", "/file/snapshot", "cwd", "env/snapshot"},
		{"env dotdot", "../snapshot", "~/file", "cwd", "../snapshot"},
		{"env tilde is literal", "~/snapshot", "~/file", "cwd", "~/snapshot"},
		{"env dollars are literal", "$HOME/${USER}/snapshot", "/file", "cwd", "$HOME/${USER}/snapshot"},
		{"empty env uses file", "", "/file/snapshot", "", "/file/snapshot"},
		{"file relative", "", "snapshots/agents", "config", "snapshots/agents"},
		{"file dotdot", "", "../agents", "config", "../agents"},
		{"file home", "", "~/snapshots/agents", "home", "snapshots/agents"},
		{"file dollars are literal", "", "$HOME/${USER}/snapshot", "config", "$HOME/${USER}/snapshot"},
		{"file bare tilde is literal", "", "~", "config", "~"},
		{"file other user is literal", "", "~someone/snapshot", "config", "~someone/snapshot"},
		{"file spaces are nonempty", "", " ", "config", " "},
		{"env spaces are nonempty", " ", "/file", "cwd", " "},
		{"default", "", "", "home", ".local/share/kitty/sessions/agents.kitty-session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := isolateEnv(t)
			cwd := t.TempDir()
			t.Chdir(cwd)
			xdg := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", xdg)
			writeConfig(t, xdg, "[session]\nfile = '"+tc.file+"'")
			c, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("CATTERY_SESSION_FILE", tc.env)
			base := map[string]string{"cwd": cwd, "config": filepath.Join(xdg, "cattery"), "home": home}[tc.base]
			want := filepath.Join(base, tc.suffix)
			got, err := c.SessionPath()
			if err != nil || got != want || !filepath.IsAbs(got) {
				t.Fatalf("SessionPath() = %q, %v; want %q", got, err, want)
			}
		})
	}
}

func TestSessionPathWithoutHome(t *testing.T) {
	for _, tc := range []struct {
		name, env, file string
		wantError       bool
	}{
		{"default needs home", "", "", true},
		{"tilde file needs home", "", "~/snapshot", true},
		{"absolute file", "", "/snapshot", false},
		{"relative file", "", "snapshot", false},
		{"env bypasses tilde file", "snapshot", "~/snapshot", false},
		{"env tilde is literal", "~/snapshot", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			t.Setenv("HOME", "")
			t.Setenv("CATTERY_SESSION_FILE", tc.env)
			var c Config
			c.Session.File = tc.file
			got, err := c.SessionPath()
			if (err != nil) != tc.wantError {
				t.Fatalf("SessionPath() = %q, %v; want error %t", got, err, tc.wantError)
			}
			if tc.wantError && got != "" {
				t.Fatalf("error returned a path: %q", got)
			}
			if !tc.wantError && !filepath.IsAbs(got) {
				t.Fatalf("path is not absolute: %q", got)
			}
		})
	}
}
