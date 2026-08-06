package setup

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexander-akhmetov/cattery"
)

const testBinary = "/opt/bin/cattery"

// harness is one setup run against temporary directories, with pi faked so it
// launches nothing.
type harness struct {
	t         *testing.T
	kittyDir  string
	claudeDir string
	opts      Options
	out       strings.Builder
	piRuns    [][]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, kittyDir: t.TempDir(), claudeDir: t.TempDir()}
	h.opts = Options{
		KittyDir:    h.kittyDir,
		ClaudeDir:   h.claudeDir,
		Binary:      testBinary,
		Yes:         true,
		LegacyPaths: []string{},
		LookPath:    func(string) (string, error) { return "/usr/bin/pi", nil },
		RunCommand: func(name string, args ...string) error {
			h.piRuns = append(h.piRuns, append([]string{name}, args...))
			return nil
		},
	}
	return h
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
	if len(h.piRuns) != 2 {
		t.Errorf("pi runs: got %d, want one per setup run", len(h.piRuns))
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
	h.write(settings, unrelatedSettings)
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
	if got := h.read(settings); !strings.Contains(got, testBinary+" state idle") {
		t.Errorf("the checkout's settings.json did not get the hooks:\n%s", got)
	}
	if got := h.perm(settings); got != 0o600 {
		t.Errorf("settings.json mode: got %o, want %o", got, 0o600)
	}
	// The backup stays beside the link. The checkout is a git repository, and
	// an untracked copy of settings.json does not belong in it.
	if _, err := os.Stat(settings + ".cattery-bak"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a backup was written into the checkout: %v", err)
	}
	if got := h.read(h.claudePath("settings.json.cattery-bak")); got != unrelatedSettings {
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
	h.write(h.claudePath("settings.json"), `{"model":"opus"}`)

	out := h.run()

	if got := h.read(h.kittyPath("kitty.conf")); got != existing {
		t.Errorf("kitty.conf changed:\n%s", got)
	}
	if got := h.read(h.claudePath("settings.json")); got != `{"model":"opus"}` {
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
	if len(h.piRuns) != 0 {
		t.Errorf("pi was launched: %v", h.piRuns)
	}

	for _, want := range []string{
		"would write", "would update", "would run", "kitty.conf", "settings.json", "pi install " + piPackage,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run report missing %q, got:\n%s", want, out)
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

// unrelatedSettings mirrors a real file. Other tools' hooks sit in the same
// arrays cattery writes into, and the keys around them must not move.
const unrelatedSettings = `{
  "model": "opus",
  "hooks": {
    "Notification": [
      {"hooks": [{"type": "command", "command": "nono notify"}]}
    ],
    "PostToolUse": [
      {"matcher": "Edit", "hooks": [{"type": "command", "command": "agterm format"}]}
    ]
  },
  "env": {"FOO": "bar"}
}`

func TestClaudeMergeKeepsUnrelatedHooksAndKeyOrder(t *testing.T) {
	h := newHarness(t)
	path := h.claudePath("settings.json")
	h.write(path, unrelatedSettings)

	h.run()

	merged := h.read(path)
	for _, want := range []string{"nono notify", "agterm format", `"matcher": "Edit"`, `"FOO": "bar"`, `"model": "opus"`} {
		if !strings.Contains(merged, want) {
			t.Errorf("merged settings lost %q:\n%s", want, merged)
		}
	}
	for _, h := range claudeHooks {
		want := testBinary + " state " + h.State
		if !strings.Contains(merged, want) {
			t.Errorf("merged settings missing %q:\n%s", want, merged)
		}
	}
	if got, want := topLevelKeys(t, merged), []string{"model", "hooks", "env"}; !equal(got, want) {
		t.Errorf("top-level key order: got %v, want %v", got, want)
	}
	if got, want := eventKeys(t, merged), []string{"Notification", "PostToolUse", "UserPromptSubmit", "Stop", "SessionEnd"}; !equal(got, want) {
		t.Errorf("hook event order: got %v, want %v", got, want)
	}
	if backup := h.read(path + ".cattery-bak"); backup != unrelatedSettings {
		t.Errorf("backup does not hold the original:\n%s", backup)
	}
}

func TestClaudeMergeReplacesTheCatteryHook(t *testing.T) {
	cases := []struct {
		name       string
		existing   string
		gone       string   // a command the merge must rewrite away
		kept       []string // text the merge must leave exactly as it was
		wantGroups int      // Stop groups afterwards
	}{
		{
			name:       "the shell installer's command",
			existing:   `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"cattery-state idle"}]}]}}`,
			gone:       "cattery-state idle",
			wantGroups: 1,
		},
		{
			name:       "an older binary path",
			existing:   `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/old/bin/cattery state idle"}]}]}}`,
			gone:       "/old/bin/cattery state idle",
			wantGroups: 1,
		},
		{
			// What setup writes when the binary sits in a directory whose name
			// holds a space.
			name:       "a quoted binary path",
			existing:   `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"'/opt/my apps/cattery' state idle"}]}]}}`,
			gone:       "/opt/my apps/cattery",
			wantGroups: 1,
		},
		{
			// The group belongs to the user. It can carry a matcher and another
			// tool's command, and only cattery's own entry may change.
			name: "a group cattery shares with another tool",
			existing: `{"hooks":{"Stop":[{"matcher":"*","hooks":[` +
				`{"type":"command","command":"/old/bin/cattery state idle"},` +
				`{"type":"command","command":"terminal-notifier -title 'Claude Code'"}]}]}}`,
			gone:       "/old/bin/cattery state idle",
			kept:       []string{`"matcher": "*"`, "terminal-notifier -title 'Claude Code'"},
			wantGroups: 1,
		},
		{
			// A command that only mentions cattery belongs to somebody else. The
			// && is here too, because json.MarshalIndent would escape it.
			name:       "a command that names cattery without running it",
			existing:   `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"cd ~/projects/cattery && make lint"}]}]}}`,
			kept:       []string{"cd ~/projects/cattery && make lint"},
			wantGroups: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			path := h.claudePath("settings.json")
			h.write(path, tc.existing)

			h.run()

			merged := h.read(path)
			if tc.gone != "" && strings.Contains(merged, tc.gone) {
				t.Errorf("the old command survived:\n%s", merged)
			}
			for _, want := range tc.kept {
				if !strings.Contains(merged, want) {
					t.Errorf("the merge lost %q:\n%s", want, merged)
				}
			}
			if n := strings.Count(merged, testBinary+" state idle"); n != 1 {
				t.Errorf("Stop hooks running cattery: got %d, want 1:\n%s", n, merged)
			}
			if n := len(groupsFor(t, merged, "Stop")); n != tc.wantGroups {
				t.Errorf("Stop groups: got %d, want %d:\n%s", n, tc.wantGroups, merged)
			}
		})
	}
}

func TestClaudeBackupIsWrittenOnce(t *testing.T) {
	h := newHarness(t)
	path := h.claudePath("settings.json")
	backup := path + ".cattery-bak"
	h.write(path, unrelatedSettings)
	// settings.json can hold an API key, so the copy must not widen its mode.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	h.run()
	if got := h.read(backup); got != unrelatedSettings {
		t.Fatalf("first backup:\n%s", got)
	}
	if got := h.perm(backup); got != 0o600 {
		t.Errorf("backup mode: got %o, want %o", got, 0o600)
	}

	// A later run must not overwrite the only copy of the file from before
	// cattery.
	h.write(path, `{"model":"sonnet"}`)
	h.run()
	if got := h.read(backup); got != unrelatedSettings {
		t.Fatalf("the backup was overwritten:\n%s", got)
	}
}

func TestClaudeSettingsCreatedWhenAbsent(t *testing.T) {
	h := newHarness(t)
	h.opts.ClaudeDir = filepath.Join(h.claudeDir, "nested")
	h.claudeDir = h.opts.ClaudeDir

	h.run()

	merged := h.read(h.claudePath("settings.json"))
	for _, hook := range claudeHooks {
		if !strings.Contains(merged, testBinary+" state "+hook.State) {
			t.Errorf("settings.json missing the %s hook:\n%s", hook.Event, merged)
		}
	}
	if _, err := os.Stat(h.claudePath("settings.json.cattery-bak")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a backup was written for a file that did not exist")
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
	if !strings.Contains(out, "skipped") || !strings.Contains(out, "UserPromptSubmit") {
		t.Errorf("report does not skip and explain, got:\n%s", out)
	}
}

// --- consent ----------------------------------------------------------------

func TestConsent(t *testing.T) {
	cases := []struct {
		name       string
		answers    string // empty with stdin means an empty answer to both
		noStdin    bool
		yes        bool
		wantClaude bool
		wantPi     bool
	}{
		{name: "empty answers accept both", answers: "\n\n", wantClaude: true, wantPi: true},
		{name: "explicit yes", answers: "y\nyes\n", wantClaude: true, wantPi: true},
		{name: "explicit decline", answers: "n\nn\n", wantClaude: false, wantPi: false},
		{name: "declines Claude, accepts pi", answers: "no\n\n", wantClaude: false, wantPi: true},
		{name: "--yes skips the questions", noStdin: true, yes: true, wantClaude: true, wantPi: true},
		{name: "no terminal and no --yes skips both", noStdin: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.opts.Yes = tc.yes
			if !tc.noStdin {
				h.opts.In = strings.NewReader(tc.answers)
			}

			out := h.run()

			_, err := os.Stat(h.claudePath("settings.json"))
			if merged := err == nil; merged != tc.wantClaude {
				t.Errorf("settings.json written=%v, want %v; output:\n%s", merged, tc.wantClaude, out)
			}
			if ran := len(h.piRuns) > 0; ran != tc.wantPi {
				t.Errorf("pi install ran=%v, want %v; output:\n%s", ran, tc.wantPi, out)
			}
			// A skipped step says what to run by hand.
			if !tc.wantClaude {
				for _, hook := range claudeHooks {
					if !strings.Contains(out, hook.Event) || !strings.Contains(out, testBinary+" state "+hook.State) {
						t.Errorf("report missing the manual %s command, got:\n%s", hook.Event, out)
					}
				}
			}
			if !tc.wantPi && !strings.Contains(out, "pi install "+piPackage) {
				t.Errorf("report missing the manual pi command, got:\n%s", out)
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

	if len(h.piRuns) != 1 {
		t.Fatalf("pi runs: got %v, want one", h.piRuns)
	}
	want := []string{"/usr/bin/pi", "install", piPackage}
	if !equal(h.piRuns[0], want) {
		t.Fatalf("pi command: got %v, want %v", h.piRuns[0], want)
	}
}

func TestPiMissingFromPath(t *testing.T) {
	h := newHarness(t)
	h.opts.LookPath = func(string) (string, error) { return "", exec.ErrNotFound }

	out := h.run()

	if len(h.piRuns) != 0 {
		t.Fatalf("pi was launched: %v", h.piRuns)
	}
	if !strings.Contains(out, "pi is not on PATH") || !strings.Contains(out, "pi install "+piPackage) {
		t.Errorf("report does not explain the skip, got:\n%s", out)
	}
}

// A pi install that fails is reported. The kitty install before it still
// stands, so setup does not fail.
func TestPiInstallFailureIsReported(t *testing.T) {
	h := newHarness(t)
	h.opts.RunCommand = func(string, ...string) error { return errors.New("network unreachable") }

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
