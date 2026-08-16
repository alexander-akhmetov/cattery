package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alexander-akhmetov/cattery"
)

const testBinary = "/opt/bin/cattery"

// harness is one setup run against temporary directories, with codex and pi
// faked so nothing launches.
type harness struct {
	t           *testing.T
	kittyDir    string
	claudeDir   string
	codexDir    string
	opencodeDir string
	opts        Options
	out         strings.Builder
	runs        [][]string

	// failRun makes chosen commands fail. The harness records every run either
	// way, so a test can check what came after the failure.
	failRun func(name string, args []string) error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	// Stale resolves opencode's directory from the environment rather than
	// taking it as an argument, so the whole suite points XDG_CONFIG_HOME at a
	// temporary directory. Without that a test would read, and report on, the
	// plugin installed on the machine running it.
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	h := &harness{
		t:           t,
		kittyDir:    t.TempDir(),
		claudeDir:   t.TempDir(),
		codexDir:    t.TempDir(),
		opencodeDir: filepath.Join(config, "opencode"),
	}
	h.opts = Options{
		KittyDir:    h.kittyDir,
		ClaudeDir:   h.claudeDir,
		CodexDir:    h.codexDir,
		OpencodeDir: h.opencodeDir,
		Binary:      testBinary,
		Yes:         true,
		LegacyPaths: []string{},
		LookPath:    func(file string) (string, error) { return "/usr/bin/" + file, nil },
		RunCommand: func(name string, args ...string) error {
			h.runs = append(h.runs, append([]string{name}, args...))
			if h.failRun == nil {
				return nil
			}
			return h.failRun(name, args)
		},
	}
	return h
}

// ran is what one binary was launched with, one entry per run.
func (h *harness) ran(name string) [][]string {
	var out [][]string
	for _, run := range h.runs {
		if filepath.Base(run[0]) == name {
			out = append(out, run)
		}
	}
	return out
}

func (h *harness) run() string {
	h.t.Helper()
	h.out.Reset()
	h.opts.Out = &h.out
	if err := Run(h.opts); err != nil {
		h.t.Fatalf("setup: %v", err)
	}
	return h.out.String()
}

func (h *harness) kittyPath(name string) string  { return filepath.Join(h.kittyDir, name) }
func (h *harness) claudePath(name string) string { return filepath.Join(h.claudeDir, name) }
func (h *harness) codexPath(name string) string  { return filepath.Join(h.codexDir, name) }

// opencodePluginPath is where setup writes the plugin: opencode scans
// "{plugin,plugins}/*.{ts,js}" under its configuration directory.
func (h *harness) opencodePluginPath() string {
	return filepath.Join(h.opencodeDir, "plugin", "cattery.ts")
}

func (h *harness) read(path string) string {
	h.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func (h *harness) write(path, content string) {
	h.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		h.t.Fatalf("write %s: %v", path, err)
	}
}

func (h *harness) perm(path string) fs.FileMode {
	h.t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		h.t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// --- kitty files ------------------------------------------------------------

func TestFreshInstall(t *testing.T) {
	h := newHarness(t)
	out := h.run()

	assets := cattery.KittyFiles()
	for _, name := range append(append([]string{}, cattery.ManagedFiles...), cattery.TabBarFile) {
		path := h.kittyPath(name)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			t.Errorf("%s is a symlink; installed files must be copies", name)
		}
		if got := info.Mode().Perm(); got != assetMode {
			t.Errorf("%s mode: got %o, want %o", name, got, assetMode)
		}
		want, err := fs.ReadFile(assets, name)
		if err != nil {
			t.Fatalf("embedded %s: %v", name, err)
		}
		if h.read(path) != string(want) {
			t.Errorf("%s does not match the embedded copy", name)
		}
	}

	conf := h.read(h.kittyPath("kitty.conf"))
	for _, want := range []string{
		blockStart, blockEnd,
		"watcher cattery_watcher.py",
		"allow_remote_control yes",
		"listen_on unix:/tmp/kitty-{kitty_pid}",
		"tab_bar_style powerline",
		`tab_title_template "{custom}"`,
		"map opt+a>opt+a launch --type=overlay --cwd=current --copy-colors " + testBinary,
		"env CATTERY_BIN=" + testBinary,
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("kitty.conf missing %q, got:\n%s", want, conf)
		}
	}
	if !strings.Contains(out, "Reload kitty to finish.") {
		t.Errorf("report missing the closing line, got:\n%s", out)
	}
}

// A second run leaves the same bytes behind. The kitty files match the embedded
// copies again, and the managed block is replaced in place instead of added a
// second time.
func TestRerunIsIdempotent(t *testing.T) {
	h := newHarness(t)
	// The first run takes the old hooks out. The second has nothing left to do
	// with the file, which is the case that must not rewrite it again.
	h.write(h.claudePath("settings.json"), oldHookSettings)
	h.run()

	before := map[string]string{}
	for _, name := range append(append([]string{}, cattery.ManagedFiles...), cattery.TabBarFile, "kitty.conf") {
		before[name] = h.read(h.kittyPath(name))
	}
	settingsBefore := h.read(h.claudePath("settings.json"))

	h.run()

	for name, want := range before {
		if got := h.read(h.kittyPath(name)); got != want {
			t.Errorf("%s changed on the second run", name)
		}
	}
	if got := h.read(h.claudePath("settings.json")); got != settingsBefore {
		t.Errorf("settings.json changed on the second run:\n%s", got)
	}
	if n := strings.Count(before["kitty.conf"], blockStart); n != 1 {
		t.Errorf("kitty.conf holds %d cattery blocks, want 1", n)
	}
	if n := len(h.ran("pi")); n != 2 {
		t.Errorf("pi runs: got %d, want one per setup run", n)
	}
	if n := len(h.ran("codex")); n != 2*len(codexArgs) {
		t.Errorf("codex runs: got %d, want %d per setup run", n, len(codexArgs))
	}
	if n := len(h.ran("claude")); n != 2*len(claudeArgs) {
		t.Errorf("claude runs: got %d, want %d per setup run", n, len(claudeArgs))
	}
}

// install.sh left symlinks into a checkout. Writing through one would edit the
// checkout, so setup removes the link first.
func TestSymlinkTargetIsReplacedByACopy(t *testing.T) {
	h := newHarness(t)
	checkout := t.TempDir()
	source := filepath.Join(checkout, "cattery_tab.py")
	h.write(source, "# the checkout's copy\n")
	if err := os.Symlink(source, h.kittyPath("cattery_tab.py")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	out := h.run()

	info, err := os.Lstat(h.kittyPath("cattery_tab.py"))
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		t.Fatal("cattery_tab.py is still a symlink")
	}
	if got := info.Mode().Perm(); got != assetMode {
		t.Errorf("mode: got %o, want %o", got, assetMode)
	}
	if got := h.read(source); got != "# the checkout's copy\n" {
		t.Errorf("the checkout was modified: %q", got)
	}
	if !strings.Contains(out, "was a symlink") {
		t.Errorf("report does not mention the symlink, got:\n%s", out)
	}
}

// kitty.conf and Claude's settings.json belong to the user, and both are often
// symlinks into a dotfiles checkout. Setup edits the file the link points at.
// Replacing the link with a regular file would detach both from the checkout in
// silence, while `git status` there stayed clean.
func TestSymlinkedUserFilesAreEditedThroughTheLink(t *testing.T) {
	h := newHarness(t)
	dotfiles := t.TempDir()

	conf := filepath.Join(dotfiles, "kitty.conf")
	h.write(conf, "font_size 13\n")
	settings := filepath.Join(dotfiles, "settings.json")
	h.write(settings, oldHookSettings)
	if err := os.Chmod(settings, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	links := map[string]string{
		h.kittyPath("kitty.conf"):     conf,
		h.claudePath("settings.json"): settings,
	}
	for link, target := range links {
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}

	out := h.run()

	for link, target := range links {
		info, err := os.Lstat(link)
		if err != nil {
			t.Fatalf("lstat %s: %v", link, err)
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			t.Errorf("%s is no longer a symlink; the write replaced it", shorten(link))
		}
		if !strings.Contains(out, shorten(target)) {
			t.Errorf("the report does not name %s, got:\n%s", shorten(target), out)
		}
	}
	if got := h.read(conf); !strings.Contains(got, blockStart) {
		t.Errorf("the checkout's kitty.conf did not get the block:\n%s", got)
	}
	if got := h.read(settings); strings.Contains(got, testBinary+" state idle") {
		t.Errorf("the checkout's settings.json kept the hooks:\n%s", got)
	}
	if got := h.perm(settings); got != 0o600 {
		t.Errorf("settings.json mode: got %o, want %o", got, 0o600)
	}
	// The backup stays beside the link. The checkout is a git repository, and
	// an untracked copy of settings.json does not belong in it.
	if _, err := os.Stat(settings + ".cattery-bak"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a backup was written into the checkout: %v", err)
	}
	if got := h.read(h.claudePath("settings.json.cattery-bak")); got != oldHookSettings {
		t.Errorf("backup does not hold the original:\n%s", got)
	}
}

// --- tab_bar.py -------------------------------------------------------------

func TestTabBarBranches(t *testing.T) {
	cases := []struct {
		name     string
		existing string // empty means no tab_bar.py at all
		want     string // the content afterwards; empty means the embedded copy
		manual   bool   // setup prints the lines to add
	}{
		{
			name: "absent gets the embedded default",
			want: "",
		},
		{
			name:     "already calls cattery_tab",
			existing: "from cattery_tab import agent_prefix\n\ndef draw_title(data):\n    return \"\"\n",
			want:     "from cattery_tab import agent_prefix\n\ndef draw_title(data):\n    return \"\"\n",
		},
		{
			name:     "custom without cattery is left alone",
			existing: "def draw_title(data):\n    return data['title']\n",
			want:     "def draw_title(data):\n    return data['title']\n",
			manual:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			path := h.kittyPath(cattery.TabBarFile)
			if tc.existing != "" {
				h.write(path, tc.existing)
			}

			out := h.run()

			want := tc.want
			if want == "" {
				embedded, err := fs.ReadFile(cattery.KittyFiles(), cattery.TabBarFile)
				if err != nil {
					t.Fatalf("embedded tab_bar.py: %v", err)
				}
				want = string(embedded)
			}
			if got := h.read(path); got != want {
				t.Errorf("tab_bar.py:\n got %q\nwant %q", got, want)
			}
			if printed := strings.Contains(out, "from cattery_tab import agent_prefix"); printed != tc.manual {
				t.Errorf("manual instructions printed=%v, want %v; output:\n%s", printed, tc.manual, out)
			}
		})
	}
}

// --- kitty.conf -------------------------------------------------------------

func TestKittyConfKeepsEverythingOutsideTheBlock(t *testing.T) {
	h := newHarness(t)
	head := "font_size 13.0\nshell fish\n"
	// Two blank lines below the block, which belong to the user. A run that ate
	// one would change a file whose managed block did not change, and the next
	// run would do it again.
	below := "\n\ncursor_shape beam\n"
	h.write(h.kittyPath("kitty.conf"), head+blockStart+"\nstale line\n"+blockEnd+"\n"+below)

	h.run()

	conf := h.read(h.kittyPath("kitty.conf"))
	if want := head + renderBlock(testBinary) + below; conf != want {
		t.Errorf("kitty.conf:\n got %q\nwant %q", conf, want)
	}
	if n := strings.Count(conf, blockStart); n != 1 {
		t.Errorf("start markers: got %d, want 1", n)
	}
	if n := strings.Count(conf, blockEnd); n != 1 {
		t.Errorf("end markers: got %d, want 1", n)
	}

	if out := h.run(); strings.Contains(out, "would update") || strings.Contains(out, "updated") {
		t.Errorf("the second run changed something, got:\n%s", out)
	}
	if got := h.read(h.kittyPath("kitty.conf")); got != conf {
		t.Errorf("the second run rewrote kitty.conf:\n got %q\nwant %q", got, conf)
	}
}

func TestMergeBlock(t *testing.T) {
	block := "# >>> cattery >>>\nwatcher cattery_watcher.py\n# <<< cattery <<<\n"

	cases := []struct {
		name    string
		conf    string
		want    string
		wantErr bool
	}{
		{name: "empty file", conf: "", want: block},
		{name: "whitespace only", conf: "\n\n", want: block},
		{name: "appends after settings", conf: "font_size 13\n", want: "font_size 13\n\n" + block},
		{
			name: "appends once past a trailing blank line",
			conf: "font_size 13\n\n\n",
			want: "font_size 13\n\n" + block,
		},
		{
			name: "replaces in place",
			conf: "a 1\n" + blockStart + "\nold\n" + blockEnd + "\nb 2\n",
			want: "a 1\n" + block + "b 2\n",
		},
		{
			name: "replaces a block at the top",
			conf: blockStart + "\nold\n" + blockEnd + "\nb 2\n",
			want: block + "b 2\n",
		},
		{
			// The blank lines sit outside the block, so they are the user's.
			name: "keeps blank lines below the block",
			conf: "a 1\n" + blockStart + "\nold\n" + blockEnd + "\n\n\nb 2\n",
			want: "a 1\n" + block + "\n\nb 2\n",
		},
		{
			name: "keeps blank lines above the block",
			conf: "a 1\n\n\n" + blockStart + "\nold\n" + blockEnd + "\n",
			want: "a 1\n\n\n" + block,
		},
		{name: "start without end", conf: blockStart + "\nold\n", wantErr: true},
		{name: "end without start", conf: "old\n" + blockEnd + "\n", wantErr: true},
		{
			name:    "two blocks",
			conf:    blockStart + "\n" + blockEnd + "\n" + blockStart + "\n" + blockEnd + "\n",
			wantErr: true,
		},
		{name: "markers in the wrong order", conf: blockEnd + "\nold\n" + blockStart + "\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mergeBlock(tc.conf, block)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("mergeBlock: got %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("mergeBlock: %v", err)
			}
			if got != tc.want {
				t.Fatalf("mergeBlock:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// Unpaired markers are reported and left alone. Repairing them means choosing
// which half of the file the user meant to keep.
func TestKittyConfWithBrokenMarkersIsLeftAlone(t *testing.T) {
	h := newHarness(t)
	conf := "font_size 13\n" + blockStart + "\nwatcher cattery_watcher.py\n"
	h.write(h.kittyPath("kitty.conf"), conf)

	out := h.run()

	if got := h.read(h.kittyPath("kitty.conf")); got != conf {
		t.Errorf("kitty.conf was changed:\n%s", got)
	}
	if !strings.Contains(out, "kept") || !strings.Contains(out, blockStart) {
		t.Errorf("report does not explain the problem or print the block, got:\n%s", out)
	}
}

// The two lines naming the binary quote it differently, and a path with a space
// is where that shows. `launch` splits its command shell-style, so the picker
// binding needs the quotes; kitty's `env` takes the rest of the line as the
// value, so the same quotes would end up in what the watcher reads.
func TestRenderBlockQuotesOnlyTheBinding(t *testing.T) {
	block := renderBlock("/opt/my apps/cattery")

	for _, want := range []string{
		"--copy-colors '/opt/my apps/cattery'",
		envBinary + "/opt/my apps/cattery\n",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q, got:\n%s", want, block)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{in: "/opt/bin/cattery", want: "/opt/bin/cattery"},
		{in: "/Users/a.b/.local/bin/cattery", want: "/Users/a.b/.local/bin/cattery"},
		{in: "/opt/my apps/cattery", want: "'/opt/my apps/cattery'"},
		{in: "/opt/it's/cattery", want: `'/opt/it'\''s/cattery'`},
		{in: "/opt/$HOME/cattery", want: "'/opt/$HOME/cattery'"},
		{in: "", want: "''"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := shellQuote(tc.in); got != tc.want {
				t.Fatalf("shellQuote(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// --- directory resolution ---------------------------------------------------

func TestKittyDirResolution(t *testing.T) {
	cases := []struct {
		name    string
		useFlag bool
	}{
		{name: "--kitty-dir wins over the environment", useFlag: true},
		{name: "no flag falls back to the environment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flagDir := t.TempDir()
			envDir := t.TempDir()
			t.Setenv("KITTY_CONFIG_DIRECTORY", envDir)

			h := newHarness(t)
			used, unused := envDir, flagDir
			if tc.useFlag {
				used, unused = flagDir, envDir
				h.opts.KittyDir = flagDir
			} else {
				h.opts.KittyDir = ""
			}
			h.kittyDir = used
			h.run()

			if _, err := os.Stat(filepath.Join(used, "cattery_tab.py")); err != nil {
				t.Fatalf("setup did not write into %s: %v", used, err)
			}
			entries, err := os.ReadDir(unused)
			if err != nil {
				t.Fatalf("read %s: %v", unused, err)
			}
			if len(entries) != 0 {
				t.Fatalf("setup wrote into %s as well: %v", unused, entries)
			}
		})
	}
}

// --- dry run ----------------------------------------------------------------

func TestDryRunWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.opts.DryRun = true
	existing := "font_size 13\n"
	h.write(h.kittyPath("kitty.conf"), existing)
	h.write(h.claudePath("settings.json"), oldHookSettings)

	out := h.run()

	if got := h.read(h.kittyPath("kitty.conf")); got != existing {
		t.Errorf("kitty.conf changed:\n%s", got)
	}
	if got := h.read(h.claudePath("settings.json")); got != oldHookSettings {
		t.Errorf("settings.json changed:\n%s", got)
	}
	for _, name := range cattery.ManagedFiles {
		if _, err := os.Stat(h.kittyPath(name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was created", name)
		}
	}
	if _, err := os.Stat(h.claudePath("settings.json.cattery-bak")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a backup was created")
	}
	if _, err := os.Stat(h.opencodePluginPath()); !errors.Is(err, os.ErrNotExist) {
		t.Error("the opencode plugin was created")
	}
	if len(h.runs) != 0 {
		t.Errorf("a command was launched: %v", h.runs)
	}

	want := make([]string, 0, 8+len(codexArgs)+len(claudeArgs))
	want = append(want,
		"would write", "would update", "would run", "would remove", "kitty.conf", "settings.json",
		"pi install "+piPackage, shorten(h.opencodePluginPath()),
	)
	for _, args := range codexArgs {
		want = append(want, codexCommand(args))
	}
	for _, args := range claudeArgs {
		want = append(want, claudeCommand(args))
	}
	for _, phrase := range want {
		if !strings.Contains(out, phrase) {
			t.Errorf("dry-run report missing %q, got:\n%s", phrase, out)
		}
	}
}

// --- legacy leftovers -------------------------------------------------------

func TestLegacyLeftoversAreReportedNotDeleted(t *testing.T) {
	h := newHarness(t)
	overlay := h.kittyPath("cattery_overlay.sh")
	stateWriter := filepath.Join(t.TempDir(), "cattery-state")
	h.write(overlay, "#!/usr/bin/env bash\n")
	h.write(stateWriter, "#!/bin/bash\n")
	h.opts.LegacyPaths = []string{overlay, stateWriter, filepath.Join(h.kittyDir, "gone.sh")}

	out := h.run()

	for _, path := range []string{overlay, stateWriter} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was removed: %v", path, err)
		}
		if !strings.Contains(out, shorten(path)) {
			t.Errorf("report does not mention %s, got:\n%s", path, out)
		}
	}
	if strings.Contains(out, "gone.sh") {
		t.Errorf("report names a file that does not exist, got:\n%s", out)
	}
}

// --- Claude -----------------------------------------------------------------

// The marketplace has to be added before the plugin can be installed from it,
// and updated in between, because `marketplace add` on a source already
// configured leaves its snapshot alone.
func TestClaudePluginCommand(t *testing.T) {
	h := newHarness(t)
	out := h.run()

	runs := h.ran("claude")
	want := [][]string{
		{"/usr/bin/claude", "plugin", "marketplace", "add", claudeSource},
		{"/usr/bin/claude", "plugin", "marketplace", "update", claudeMarketplace},
		{"/usr/bin/claude", "plugin", "install", claudePlugin, "--scope", "user"},
	}
	if len(runs) != len(want) {
		t.Fatalf("claude runs: got %v, want %v", runs, want)
	}
	for i := range want {
		if !equal(runs[i], want[i]) {
			t.Errorf("claude run %d: got %v, want %v", i, runs[i], want[i])
		}
	}
	// The plugin comes from the published repository, so nothing here notices it
	// going stale. That has to be said.
	if !strings.Contains(out, "stale") {
		t.Errorf("report missing the stale note, got:\n%s", out)
	}
}

func TestClaudeMissingFromPath(t *testing.T) {
	h := newHarness(t)
	h.opts.LookPath = func(file string) (string, error) {
		if file == "claude" {
			return "", exec.ErrNotFound
		}
		return "/usr/bin/" + file, nil
	}
	// The hooks an older release merged in are the only thing publishing state
	// for a user who cannot install the plugin.
	path := h.claudePath("settings.json")
	h.write(path, oldHookSettings)

	out := h.run()

	if runs := h.ran("claude"); len(runs) != 0 {
		t.Fatalf("claude was launched: %v", runs)
	}
	if !strings.Contains(out, "claude is not on PATH") {
		t.Errorf("report does not name the skip, got:\n%s", out)
	}
	for _, args := range claudeArgs {
		if !strings.Contains(out, claudeCommand(args)) {
			t.Errorf("report missing %q, got:\n%s", claudeCommand(args), out)
		}
	}
	if got := h.read(path); got != oldHookSettings {
		t.Errorf("settings.json was changed:\n%s", got)
	}
}

// A failing step is reported and the ones behind it still run. `plugin install`
// works off the clone an earlier run left, so a marketplace fetch that fails
// offline does not have to take the install with it. A failing install does
// take the removal with it: without a plugin, the old hooks are all the user
// has.
func TestClaudeFailureIsReported(t *testing.T) {
	cases := []struct {
		name        string
		fails       []string // the claudeArgs entry that fails
		wantRemoval bool
	}{
		{name: "the marketplace fetch", fails: claudeArgs[0], wantRemoval: true},
		{name: "the install itself", fails: claudeArgs[len(claudeArgs)-1]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.failRun = func(name string, args []string) error {
				if filepath.Base(name) == "claude" && slices.Equal(args, tc.fails) {
					return errors.New("network unreachable")
				}
				return nil
			}
			path := h.claudePath("settings.json")
			h.write(path, oldHookSettings)

			out := h.run()

			if runs := h.ran("claude"); len(runs) != len(claudeArgs) {
				t.Fatalf("claude runs: got %v, want %d", runs, len(claudeArgs))
			}
			if !strings.Contains(out, "failed") || !strings.Contains(out, "network unreachable") {
				t.Errorf("report does not name the failure, got:\n%s", out)
			}
			if removed := h.read(path) != oldHookSettings; removed != tc.wantRemoval {
				t.Errorf("hooks removed=%v, want %v; output:\n%s", removed, tc.wantRemoval, out)
			}
		})
	}
}

// --- the hooks an older release merged into settings.json --------------------

// oldHookSettings is what a machine installed before the plugin holds: cattery's
// five hooks beside another tool's, and keys around them that must not move.
const oldHookSettings = `{
  "model": "opus",
  "hooks": {
    "Notification": [
      {"hooks": [{"type": "command", "command": "nono notify"}]},
      {"hooks": [{"type": "command", "command": "/opt/bin/cattery state blocked"}]}
    ],
    "PostToolUse": [
      {"matcher": "Edit", "hooks": [{"type": "command", "command": "agterm format 2>/dev/null && echo ok"}]}
    ],
    "SessionStart": [
      {"matcher": "startup|resume|clear", "hooks": [{"type": "command", "command": "/opt/bin/cattery state idle"}]}
    ],
    "UserPromptSubmit": [
      {"hooks": [{"type": "command", "command": "/opt/bin/cattery state working"}]}
    ],
    "Stop": [
      {"hooks": [{"type": "command", "command": "/opt/bin/cattery state idle"}]}
    ],
    "SessionEnd": [
      {"hooks": [{"type": "command", "command": "/opt/bin/cattery state clear"}]}
    ]
  },
  "env": {"FOO": "bar"}
}`

func TestClaudeOldHooksAreRemoved(t *testing.T) {
	h := newHarness(t)
	path := h.claudePath("settings.json")
	h.write(path, oldHookSettings)

	out := h.run()

	cleaned := h.read(path)
	if strings.Contains(cleaned, "cattery state") {
		t.Errorf("a cattery hook survived:\n%s", cleaned)
	}
	// The other tool's hooks, and the keys around them, belong to the user. The
	// && and the 2>/dev/null are here because json.Marshal escapes both unless
	// every step goes through encode.
	for _, want := range []string{
		"nono notify", "agterm format 2>/dev/null && echo ok", `"matcher": "Edit"`,
		`"FOO": "bar"`, `"model": "opus"`,
	} {
		if !strings.Contains(cleaned, want) {
			t.Errorf("the removal lost %q:\n%s", want, cleaned)
		}
	}
	if got, want := topLevelKeys(t, cleaned), []string{"model", "hooks", "env"}; !equal(got, want) {
		t.Errorf("top-level key order: got %v, want %v", got, want)
	}
	// Notification keeps its first group, because the other tool's command is in
	// it. The four events cattery had to itself go entirely.
	wantEvents := []string{"Notification", "PostToolUse"}
	if got := eventKeys(t, cleaned); !equal(got, wantEvents) {
		t.Errorf("hook event order: got %v, want %v", got, wantEvents)
	}
	if groups := groupsFor(t, cleaned, "Notification"); len(groups) != 1 {
		t.Errorf("Notification groups: got %d, want 1:\n%s", len(groups), cleaned)
	}
	if backup := h.read(path + ".cattery-bak"); backup != oldHookSettings {
		t.Errorf("backup does not hold the original:\n%s", backup)
	}
	if !strings.Contains(out, "removed") || !strings.Contains(out, "5 hooks") {
		t.Errorf("report does not name the removal, got:\n%s", out)
	}
}

func TestClaudeHookRemovalCases(t *testing.T) {
	cases := []struct {
		name     string
		event    string // the hook event the case is about, "" for Stop
		existing string
		kept     []string // text the removal must leave exactly as it was
		// wantGroups is how many groups the event holds afterwards, and
		// wantEvents which events survive at all. A nil wantEvents means no
		// "hooks" key is left.
		wantGroups int
		wantEvents []string
		wantCount  int
		// wantGroupKeys is the first surviving group's keys, in order. A group
		// this rewrites goes back through object, which is what keeps the
		// matcher where the user put it.
		wantGroupKeys []string
		wantMatcher   string
	}{
		{
			name:     "the shell installer's command",
			existing: `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"cattery-state idle"}]}]}}`,
			// The last event empties, so "hooks" goes with it.
			wantCount: 1,
		},
		{
			name:      "an older binary path",
			existing:  `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/old/bin/cattery state idle"}]}]}}`,
			wantCount: 1,
		},
		{
			// What an older setup wrote when the binary sat in a directory whose
			// name holds a space.
			name:      "a quoted binary path",
			existing:  `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"'/opt/my apps/cattery' state idle"}]}]}}`,
			wantCount: 1,
		},
		{
			// The group belongs to the user. Only cattery's own entry goes, and
			// the matcher and the other command around it stay.
			name: "a group cattery shares with another tool",
			existing: `{"hooks":{"Stop":[{"matcher":"*","hooks":[` +
				`{"type":"command","command":"/old/bin/cattery state idle"},` +
				`{"type":"command","command":"terminal-notifier -title 'Claude Code'"}]}]}}`,
			kept:          []string{"terminal-notifier -title 'Claude Code'"},
			wantGroups:    1,
			wantEvents:    []string{"Stop"},
			wantCount:     1,
			wantGroupKeys: []string{"matcher", "hooks"},
			wantMatcher:   "*",
		},
		{
			// A command that only mentions cattery belongs to somebody else, so
			// the whole file comes back untouched.
			name:       "a command that names cattery without running it",
			existing:   `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"cd ~/projects/cattery && make lint"}]}]}}`,
			kept:       []string{"cd ~/projects/cattery && make lint"},
			wantGroups: 1,
			wantEvents: []string{"Stop"},
		},
		{
			// The plugin's own command carries --kind, so the state is not the
			// last word and catteryCommand does not claim it. A user who copied
			// it into settings.json by hand keeps it.
			name:       "the plugin's command copied in by hand",
			existing:   `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"cattery state idle --kind claude"}]}]}}`,
			kept:       []string{"cattery state idle --kind claude"},
			wantGroups: 1,
			wantEvents: []string{"Stop"},
		},
		{
			name:      "a SessionStart group the user stripped the matcher from",
			event:     "SessionStart",
			existing:  `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/old/bin/cattery state idle"}]}]}}`,
			wantCount: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			path := h.claudePath("settings.json")
			h.write(path, tc.existing)

			out := h.run()

			cleaned := h.read(path)
			for _, want := range tc.kept {
				if !strings.Contains(cleaned, want) {
					t.Errorf("the removal lost %q:\n%s", want, cleaned)
				}
			}
			// A file with nothing of cattery's in it is not rewritten at all, so
			// it comes back byte for byte and gets no backup.
			if tc.wantCount == 0 {
				if cleaned != tc.existing {
					t.Errorf("settings.json was rewritten:\n%s", cleaned)
				}
				if _, err := os.Stat(path + ".cattery-bak"); !errors.Is(err, os.ErrNotExist) {
					t.Error("a backup was written for a file that needed no edit")
				}
				return
			}
			if want := fmt.Sprintf("%d hooks", tc.wantCount); !strings.Contains(out, want) {
				t.Errorf("report does not say %q, got:\n%s", want, out)
			}
			if strings.Contains(cleaned, "cattery state") || strings.Contains(cleaned, "cattery-state") {
				t.Errorf("a cattery hook survived:\n%s", cleaned)
			}
			if got := eventKeys(t, cleaned); !equal(got, tc.wantEvents) {
				t.Errorf("hook events: got %v, want %v:\n%s", got, tc.wantEvents, cleaned)
			}
			if tc.wantEvents == nil {
				if got := topLevelKeys(t, cleaned); slices.Contains(got, "hooks") {
					t.Errorf("an empty hooks key was left behind: %v\n%s", got, cleaned)
				}
				return
			}
			event := tc.event
			if event == "" {
				event = "Stop"
			}
			groups := groupsFor(t, cleaned, event)
			if len(groups) != tc.wantGroups {
				t.Fatalf("%s groups: got %d, want %d:\n%s", event, len(groups), tc.wantGroups, cleaned)
			}
			if tc.wantGroupKeys != nil {
				if got := hookGroup(t, groups[0]).keys; !equal(got, tc.wantGroupKeys) {
					t.Errorf("%s group keys: got %v, want %v:\n%s", event, got, tc.wantGroupKeys, cleaned)
				}
				if got := matcherOf(t, groups[0]); got != tc.wantMatcher {
					t.Errorf("%s matcher: got %q, want %q:\n%s", event, got, tc.wantMatcher, cleaned)
				}
			}
		})
	}
}

// The hooks stay when there is no plugin to replace them: a user who declines
// keeps the only thing publishing their state.
func TestClaudeDeclinedInstallKeepsTheHooks(t *testing.T) {
	h := newHarness(t)
	h.opts.Yes = false
	h.opts.In = strings.NewReader("n\n\n\n\n")
	path := h.claudePath("settings.json")
	h.write(path, oldHookSettings)

	out := h.run()

	if got := h.read(path); got != oldHookSettings {
		t.Errorf("settings.json was changed:\n%s", got)
	}
	if _, err := os.Stat(path + ".cattery-bak"); !errors.Is(err, os.ErrNotExist) {
		t.Error("a backup was written on a declined install")
	}
	for _, args := range claudeArgs {
		if !strings.Contains(out, claudeCommand(args)) {
			t.Errorf("report missing the manual %q, got:\n%s", claudeCommand(args), out)
		}
	}
}

func TestClaudeBackupIsWrittenOnce(t *testing.T) {
	h := newHarness(t)
	path := h.claudePath("settings.json")
	backup := path + ".cattery-bak"
	h.write(path, oldHookSettings)
	// settings.json can hold an API key, so the copy must not widen its mode.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	h.run()
	if got := h.read(backup); got != oldHookSettings {
		t.Fatalf("first backup:\n%s", got)
	}
	if got := h.perm(backup); got != 0o600 {
		t.Errorf("backup mode: got %o, want %o", got, 0o600)
	}

	// A later run must not overwrite the only copy of the file from before
	// cattery. This one has a hook left to remove, or nothing would be written
	// at all.
	h.write(path, `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"cattery-state idle"}]}]}}`)
	h.run()
	if got := h.read(backup); got != oldHookSettings {
		t.Fatalf("the backup was overwritten:\n%s", got)
	}
}

// Nothing of cattery's to take out means nothing to write, whether the file is
// missing, empty, or somebody else's entirely.
func TestClaudeSettingsLeftAlone(t *testing.T) {
	cases := []struct {
		name     string
		existing string
		absent   bool // no settings.json at all
	}{
		{name: "no settings.json", absent: true},
		{name: "an empty file", existing: ""},
		{name: "no hooks key", existing: `{"model":"opus"}`},
		{
			name:     "somebody else's hooks",
			existing: `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"notify-send done"}]}]}}`,
		},
		// A hooks key that is not an object is somebody's typo, not cattery's
		// to repair. Reported and left alone.
		{name: "a hooks key holding null", existing: `{"hooks":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			path := h.claudePath("settings.json")
			if !tc.absent {
				h.write(path, tc.existing)
			}

			out := h.run()

			if tc.absent {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Error("settings.json was created")
				}
			} else if got := h.read(path); got != tc.existing {
				t.Errorf("settings.json was changed:\n%s", got)
			}
			if _, err := os.Stat(path + ".cattery-bak"); !errors.Is(err, os.ErrNotExist) {
				t.Error("a backup was written")
			}
			if strings.Contains(out, "removed") {
				t.Errorf("report claims a removal, got:\n%s", out)
			}
		})
	}
}

func TestClaudeInvalidJSONIsLeftAlone(t *testing.T) {
	h := newHarness(t)
	path := h.claudePath("settings.json")
	broken := "{ not json"
	h.write(path, broken)

	out := h.run()

	if got := h.read(path); got != broken {
		t.Errorf("settings.json was changed:\n%s", got)
	}
	if !strings.Contains(out, "kept") || !strings.Contains(out, "settings.json") {
		t.Errorf("report does not keep and explain, got:\n%s", out)
	}
}

// --- consent ----------------------------------------------------------------

func TestConsent(t *testing.T) {
	// The questions come in one order: Claude, Codex, pi, opencode.
	type want struct{ claude, codex, pi, opencode bool }
	every := want{claude: true, codex: true, pi: true, opencode: true}
	cases := []struct {
		name    string
		answers string // empty with stdin means an empty answer to all four
		noStdin bool
		yes     bool
		want    want
	}{
		{name: "empty answers accept all four", answers: "\n\n\n\n", want: every},
		{name: "explicit yes", answers: "y\ny\nyes\ny\n", want: every},
		{name: "explicit decline", answers: "n\nn\nn\nn\n"},
		{
			name:    "declines Claude, accepts the rest",
			answers: "no\n\n\n\n",
			want:    want{codex: true, pi: true, opencode: true},
		},
		{name: "declines Codex only", answers: "\nn\n\n\n", want: want{claude: true, pi: true, opencode: true}},
		{name: "declines opencode only", answers: "\n\n\nn\n", want: want{claude: true, codex: true, pi: true}},
		{name: "--yes skips the questions", noStdin: true, yes: true, want: every},
		{name: "no terminal and no --yes skips all four", noStdin: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.opts.Yes = tc.yes
			if !tc.noStdin {
				h.opts.In = strings.NewReader(tc.answers)
			}

			out := h.run()

			if ran := len(h.ran("claude")) > 0; ran != tc.want.claude {
				t.Errorf("claude plugin install ran=%v, want %v; output:\n%s", ran, tc.want.claude, out)
			}
			if ran := len(h.ran("pi")) > 0; ran != tc.want.pi {
				t.Errorf("pi install ran=%v, want %v; output:\n%s", ran, tc.want.pi, out)
			}
			if ran := len(h.ran("codex")) > 0; ran != tc.want.codex {
				t.Errorf("codex plugin install ran=%v, want %v; output:\n%s", ran, tc.want.codex, out)
			}
			_, err := os.Stat(h.opencodePluginPath())
			if written := err == nil; written != tc.want.opencode {
				t.Errorf("opencode plugin written=%v, want %v; output:\n%s", written, tc.want.opencode, out)
			}
			// A skipped step says what to run by hand.
			if !tc.want.claude {
				for _, args := range claudeArgs {
					if !strings.Contains(out, claudeCommand(args)) {
						t.Errorf("report missing the manual %q, got:\n%s", claudeCommand(args), out)
					}
				}
			}
			if !tc.want.pi && !strings.Contains(out, "pi install "+piPackage) {
				t.Errorf("report missing the manual pi command, got:\n%s", out)
			}
			if !tc.want.codex {
				for _, args := range codexArgs {
					if !strings.Contains(out, codexCommand(args)) {
						t.Errorf("report missing the manual %q, got:\n%s", codexCommand(args), out)
					}
				}
			}
			// The kitty side never asks, because it is the install itself.
			if _, err := os.Stat(h.kittyPath("cattery_tab.py")); err != nil {
				t.Errorf("kitty files were skipped: %v", err)
			}
		})
	}
}

// --- pi ---------------------------------------------------------------------

func TestPiInstallCommand(t *testing.T) {
	h := newHarness(t)
	h.run()

	runs := h.ran("pi")
	if len(runs) != 1 {
		t.Fatalf("pi runs: got %v, want one", runs)
	}
	want := []string{"/usr/bin/pi", "install", piPackage}
	if !equal(runs[0], want) {
		t.Fatalf("pi command: got %v, want %v", runs[0], want)
	}
}

func TestPiMissingFromPath(t *testing.T) {
	h := newHarness(t)
	h.opts.LookPath = func(file string) (string, error) {
		if file == "pi" {
			return "", exec.ErrNotFound
		}
		return "/usr/bin/" + file, nil
	}

	out := h.run()

	if runs := h.ran("pi"); len(runs) != 0 {
		t.Fatalf("pi was launched: %v", runs)
	}
	if !strings.Contains(out, "pi is not on PATH") || !strings.Contains(out, "pi install "+piPackage) {
		t.Errorf("report does not explain the skip, got:\n%s", out)
	}
}

// --- opencode ---------------------------------------------------------------

// The plugin ships inside the binary, unlike the pi package and the Codex
// plugin, so setup writes the file itself and opencode auto-loads it.
func TestOpencodePluginIsWritten(t *testing.T) {
	h := newHarness(t)

	h.run()

	want, err := fs.ReadFile(cattery.OpencodeFiles(), cattery.OpencodeFile)
	if err != nil {
		t.Fatalf("read the embedded plugin: %v", err)
	}
	if got := h.read(h.opencodePluginPath()); got != string(want) {
		t.Errorf("installed plugin does not match the embedded one")
	}
	if got := h.perm(h.opencodePluginPath()); got != assetMode {
		t.Errorf("mode: got %v, want %v", got, assetMode)
	}
	if runs := h.ran("opencode"); len(runs) != 0 {
		t.Fatalf("opencode was launched: %v", runs)
	}
}

// A machine without opencode must still get a clean install: this step belongs
// to another tool.
func TestOpencodeMissingFromPath(t *testing.T) {
	h := newHarness(t)
	h.opts.LookPath = func(file string) (string, error) {
		if file == "opencode" {
			return "", exec.ErrNotFound
		}
		return "/usr/bin/" + file, nil
	}

	out := h.run()

	if _, err := os.Stat(h.opencodePluginPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("wrote the plugin for a machine with no opencode: %v", err)
	}
	if !strings.Contains(out, "opencode is not on PATH") {
		t.Errorf("report does not explain the skip, got:\n%s", out)
	}
}

// --- codex ------------------------------------------------------------------

// The marketplace has to be added before the plugin can be installed from it,
// and upgraded in between, because `marketplace add` on a source already
// configured leaves its snapshot alone.
func TestCodexPluginCommand(t *testing.T) {
	h := newHarness(t)
	out := h.run()

	runs := h.ran("codex")
	want := [][]string{
		{"/usr/bin/codex", "plugin", "marketplace", "add", codexSource},
		{"/usr/bin/codex", "plugin", "marketplace", "upgrade", codexMarketplace},
		{"/usr/bin/codex", "plugin", "add", codexPlugin},
	}
	if len(runs) != len(want) {
		t.Fatalf("codex runs: got %v, want %v", runs, want)
	}
	for i := range want {
		if !equal(runs[i], want[i]) {
			t.Errorf("codex run %d: got %v, want %v", i, runs[i], want[i])
		}
	}
	// The trust gate and the stale plugin: neither can be automated, so both
	// have to be said.
	for _, phrase := range []string{"/hooks", "trust", "stale"} {
		if !strings.Contains(out, phrase) {
			t.Errorf("report missing %q, got:\n%s", phrase, out)
		}
	}
}

func TestCodexMissingFromPath(t *testing.T) {
	h := newHarness(t)
	h.opts.LookPath = func(file string) (string, error) {
		if file == "codex" {
			return "", exec.ErrNotFound
		}
		return "/usr/bin/" + file, nil
	}

	out := h.run()

	if runs := h.ran("codex"); len(runs) != 0 {
		t.Fatalf("codex was launched: %v", runs)
	}
	if !strings.Contains(out, "codex is not on PATH") {
		t.Errorf("report does not name the skip, got:\n%s", out)
	}
	for _, args := range codexArgs {
		if !strings.Contains(out, codexCommand(args)) {
			t.Errorf("report missing %q, got:\n%s", codexCommand(args), out)
		}
	}
}

// A failing step is reported and the ones behind it still run. `plugin add`
// works off the snapshot an earlier run left, so a marketplace fetch that
// fails offline does not have to take the install with it.
func TestCodexFailureIsReported(t *testing.T) {
	h := newHarness(t)
	h.failRun = func(name string, args []string) error {
		if filepath.Base(name) == "codex" && slices.Equal(args, codexArgs[0]) {
			return errors.New("network unreachable")
		}
		return nil
	}

	out := h.run()

	if runs := h.ran("codex"); len(runs) != len(codexArgs) {
		t.Fatalf("codex runs: got %v, want %d", runs, len(codexArgs))
	}
	if !strings.Contains(out, "failed") || !strings.Contains(out, "network unreachable") {
		t.Errorf("report does not name the failure, got:\n%s", out)
	}
}

// Codex's own hooks.json is the user's. Hand-written cattery entries there
// double up with the plugin, so setup names them and changes nothing.
func TestCodexHandWrittenHooksAreReported(t *testing.T) {
	cases := []struct {
		name     string
		hooks    string // "" writes no file at all
		wantNote bool
	}{
		{
			name:     "an entry cattery would double",
			hooks:    `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"cattery state idle"}]}]}}`,
			wantNote: true,
		},
		{
			name:  "somebody else's hooks",
			hooks: `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"notify-send done"}]}]}}`,
		},
		{name: "no hooks.json at all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			path := h.codexPath("hooks.json")
			if tc.hooks != "" {
				h.write(path, tc.hooks)
			}

			out := h.run()

			if got := strings.Contains(out, "double up with the plugin"); got != tc.wantNote {
				t.Errorf("reported=%v, want %v; output:\n%s", got, tc.wantNote, out)
			}
			if tc.hooks != "" && h.read(path) != tc.hooks {
				t.Errorf("hooks.json was changed:\n%s", h.read(path))
			}
		})
	}
}

// Both plugins are fetched from the published repository, so the manifests in
// this checkout are the release. A drift between them and what the writer
// accepts is silent: the host would run a command that publishes nothing.
func TestPluginManifests(t *testing.T) {
	root := filepath.Join("..", "..")
	cases := []struct {
		name string
		// dir is the plugin's directory under the repository root, and
		// manifestDir the directory inside it that holds plugin.json.
		dir, manifestDir string
		marketplace      []string // the marketplace file, as path elements
		selector         string   // the plugin@marketplace the CLI takes
		market           string   // the marketplace name that selector's half names
		kind             string   // the --kind word the hooks publish
		// wantSource is the marketplace entry's source. Codex writes an object,
		// Claude a plain string.
		wantSource string
		// wantHooksField is plugin.json's "hooks". Codex needs the path;
		// Claude loads hooks/hooks.json by itself and refuses to load a plugin
		// that names it a second time, so there it has to be absent.
		wantHooksField string
		events         []struct{ Event, State, Matcher string }
	}{
		{
			name:        "claude",
			dir:         "claude",
			manifestDir: ".claude-plugin",
			marketplace: []string{".claude-plugin", "marketplace.json"},
			selector:    claudePlugin,
			market:      claudeMarketplace,
			kind:        "claude",
			wantSource:  `"./claude"`,
			events:      claudeHooks,
		},
		{
			name:           "codex",
			dir:            "codex",
			manifestDir:    ".codex-plugin",
			marketplace:    []string{".agents", "plugins", "marketplace.json"},
			selector:       codexPlugin,
			market:         codexMarketplace,
			kind:           "codex",
			wantSource:     `{"source":"local","path":"./codex"}`,
			wantHooksField: "./hooks/hooks.json",
			// The same five states Claude's hooks publish, on Codex's own
			// events. SessionStart takes a matcher for the same reason Claude's
			// does: an idle published for a compaction marks a running agent
			// finished.
			events: []struct{ Event, State, Matcher string }{
				{Event: "SessionStart", State: "idle", Matcher: "startup|resume|clear"},
				{Event: "UserPromptSubmit", State: "working"},
				{Event: "PermissionRequest", State: "blocked", Matcher: "*"},
				{Event: "Stop", State: "idle"},
				{Event: "SessionEnd", State: "clear"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var manifest struct {
				Hooks map[string][]struct {
					Matcher string `json:"matcher"`
					Hooks   []struct {
						Type    string `json:"type"`
						Command string `json:"command"`
						Timeout int    `json:"timeout"`
					} `json:"hooks"`
				} `json:"hooks"`
			}
			read(t, filepath.Join(root, tc.dir, "hooks", "hooks.json"), &manifest)

			if len(manifest.Hooks) != len(tc.events) {
				t.Fatalf("events: got %v, want %d", slices.Sorted(maps.Keys(manifest.Hooks)), len(tc.events))
			}
			for _, w := range tc.events {
				groups, ok := manifest.Hooks[w.Event]
				if !ok || len(groups) != 1 || len(groups[0].Hooks) != 1 {
					t.Fatalf("hooks.%s: got %v, want one command", w.Event, groups)
				}
				entry := groups[0].Hooks[0]
				// The bare name off PATH: a static manifest cannot know where
				// the binary is, and both hosts run a hook as a child of a
				// process a shell started.
				if command := "cattery state " + w.State + " --kind " + tc.kind; entry.Command != command {
					t.Errorf("hooks.%s command: got %q, want %q", w.Event, entry.Command, command)
				}
				if groups[0].Matcher != w.Matcher {
					t.Errorf("hooks.%s matcher: got %q, want %q", w.Event, groups[0].Matcher, w.Matcher)
				}
				// A hook that hangs holds up the turn, and the writer's own
				// publish gives up after two seconds.
				if entry.Timeout != 5 {
					t.Errorf("hooks.%s timeout: got %d, want 5", w.Event, entry.Timeout)
				}
				if entry.Type != "command" {
					t.Errorf("hooks.%s type: got %q, want %q", w.Event, entry.Type, "command")
				}
			}

			// The two manifests name the plugin and reach the hooks file. A
			// rename leaves the install command naming something the
			// marketplace does not hold.
			var plugin struct {
				Name  string `json:"name"`
				Hooks string `json:"hooks"`
			}
			read(t, filepath.Join(root, tc.dir, tc.manifestDir, "plugin.json"), &plugin)
			wantName, _, _ := strings.Cut(tc.selector, "@")
			if plugin.Name != wantName {
				t.Errorf("plugin name: got %q, want %q", plugin.Name, wantName)
			}
			if plugin.Hooks != tc.wantHooksField {
				t.Errorf("plugin hooks field: got %q, want %q", plugin.Hooks, tc.wantHooksField)
			}

			var market struct {
				Name    string `json:"name"`
				Plugins []struct {
					Name string `json:"name"`
					// Raw, because Codex names a source with an object and
					// Claude with a path.
					Source json.RawMessage `json:"source"`
				} `json:"plugins"`
			}
			read(t, filepath.Join(append([]string{root}, tc.marketplace...)...), &market)
			if market.Name != tc.market {
				t.Errorf("marketplace name: got %q, want %q", market.Name, tc.market)
			}
			if len(market.Plugins) != 1 || market.Plugins[0].Name != plugin.Name {
				t.Fatalf("marketplace plugins: got %+v, want one named %q", market.Plugins, plugin.Name)
			}
			var source bytes.Buffer
			if err := json.Compact(&source, market.Plugins[0].Source); err != nil {
				t.Fatalf("marketplace source: %v", err)
			}
			if got := source.String(); got != tc.wantSource {
				t.Errorf("marketplace source: got %s, want %s", got, tc.wantSource)
			}
		})
	}
}

// read decodes a JSON file of this repository into v.
func read(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

// A pi install that fails is reported. The kitty install before it still
// stands, so setup does not fail.
func TestPiInstallFailureIsReported(t *testing.T) {
	h := newHarness(t)
	h.failRun = func(name string, _ []string) error {
		if filepath.Base(name) == "pi" {
			return errors.New("network unreachable")
		}
		return nil
	}

	out := h.run()

	if !strings.Contains(out, "failed") || !strings.Contains(out, "network unreachable") {
		t.Errorf("report does not name the failure, got:\n%s", out)
	}
}

// --- stale copies -----------------------------------------------------------

func TestStale(t *testing.T) {
	cases := []struct {
		name string
		// prepare runs after a fresh install and changes what is on disk.
		prepare func(t *testing.T, h *harness)
		install bool
		want    bool
	}{
		{name: "a fresh install matches", install: true},
		{
			name:    "one edited file is stale",
			install: true,
			prepare: func(_ *testing.T, h *harness) {
				h.write(h.kittyPath("cattery_tab.py"), "# hand-edited\n")
			},
			want: true,
		},
		{
			name:    "an appended byte is stale",
			install: true,
			prepare: func(t *testing.T, h *harness) {
				t.Helper()
				h.write(h.kittyPath("cattery_watcher.py"), h.read(h.kittyPath("cattery_watcher.py"))+"\n")
			},
			want: true,
		},
		{
			// An install that predates a file the binary now ships.
			name:    "a missing file beside installed ones is stale",
			install: true,
			prepare: func(t *testing.T, h *harness) {
				t.Helper()
				if err := os.Remove(h.kittyPath("cattery_jump.py")); err != nil {
					t.Fatalf("remove: %v", err)
				}
			},
			want: true,
		},
		{
			// The block carries settings the installed files depend on, so a
			// release touching only renderBlock has to warn as well.
			name:    "an edited block is stale",
			install: true,
			prepare: func(t *testing.T, h *harness) {
				t.Helper()
				conf := h.read(h.kittyPath("kitty.conf"))
				h.write(h.kittyPath("kitty.conf"), strings.Replace(conf, envBinary+testBinary+"\n", "", 1))
			},
			want: true,
		},
		{
			// The install names its own binary. Where that binary is says
			// nothing about which release wrote the block.
			name:    "another binary path is not stale",
			install: true,
			prepare: func(t *testing.T, h *harness) {
				t.Helper()
				conf := h.read(h.kittyPath("kitty.conf"))
				h.write(h.kittyPath("kitty.conf"), strings.ReplaceAll(conf, testBinary, "/usr/local/bin/cattery"))
			},
			want: false,
		},
		{
			// What setup itself leaves behind when it cannot merge. It said so
			// while it ran, and the picker has nothing to add.
			name:    "a conf with no block is not stale",
			install: true,
			prepare: func(t *testing.T, h *harness) {
				t.Helper()
				h.write(h.kittyPath("kitty.conf"), "font_size 13\n")
			},
			want: false,
		},
		{
			// The plugin is the one agent extension the binary carries, so it
			// is the one an upgrade can leave behind.
			name:    "an edited opencode plugin is stale",
			install: true,
			prepare: func(_ *testing.T, h *harness) {
				h.write(h.opencodePluginPath(), "// hand-edited\n")
			},
			want: true,
		},
		{
			// A user without opencode has no plugin and must not be told to run
			// setup again.
			name:    "no opencode plugin at all is not stale",
			install: true,
			prepare: func(t *testing.T, h *harness) {
				t.Helper()
				if err := os.Remove(h.opencodePluginPath()); err != nil {
					t.Fatalf("remove: %v", err)
				}
			},
			want: false,
		},
		{
			// Nothing was installed here, so nothing can be behind.
			name: "an empty directory is not stale",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.install {
				h.run()
			}
			if tc.prepare != nil {
				tc.prepare(t, h)
			}
			if got := Stale(h.kittyDir); got != tc.want {
				t.Fatalf("Stale: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestKittyDirFallsBackToTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KITTY_CONFIG_DIRECTORY", dir)
	got, err := KittyDir()
	if err != nil {
		t.Fatalf("KittyDir: %v", err)
	}
	if got != dir {
		t.Fatalf("KittyDir: got %q, want %q", got, dir)
	}
}

// --- helpers ----------------------------------------------------------------

func topLevelKeys(t *testing.T, data string) []string {
	t.Helper()
	var root object
	if err := json.Unmarshal([]byte(data), &root); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	return root.keys
}

func eventKeys(t *testing.T, data string) []string {
	t.Helper()
	var root object
	if err := json.Unmarshal([]byte(data), &root); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	hooks, err := root.child("hooks")
	if err != nil {
		t.Fatalf("parse hooks: %v", err)
	}
	return hooks.keys
}

func groupsFor(t *testing.T, data, event string) []json.RawMessage {
	t.Helper()
	var root object
	if err := json.Unmarshal([]byte(data), &root); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	hooks, err := root.child("hooks")
	if err != nil {
		t.Fatalf("parse hooks: %v", err)
	}
	var groups []json.RawMessage
	if raw, ok := hooks.values[event]; ok {
		if err := json.Unmarshal(raw, &groups); err != nil {
			t.Fatalf("parse %s: %v", event, err)
		}
	}
	return groups
}

// hookGroup parses one hook group, which remembers its key order.
func hookGroup(t *testing.T, group json.RawMessage) object {
	t.Helper()
	var out object
	if err := json.Unmarshal(group, &out); err != nil {
		t.Fatalf("parse a hook group: %v", err)
	}
	return out
}

// matcherOf is the matcher one hook group carries, or "" when it has none.
func matcherOf(t *testing.T, group json.RawMessage) string {
	t.Helper()
	raw, ok := hookGroup(t, group).values["matcher"]
	if !ok {
		return ""
	}
	var matcher string
	if err := json.Unmarshal(raw, &matcher); err != nil {
		t.Fatalf("parse the matcher: %v", err)
	}
	return matcher
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
