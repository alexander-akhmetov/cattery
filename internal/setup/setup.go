// Package setup installs cattery. It writes the embedded kitty files into the
// kitty config directory, keeps a marked block in kitty.conf, and offers to
// wire up Claude Code, Codex, pi and opencode. It backs `cattery setup`.
//
// The installed kitty files are copies, so an install does not depend on where
// the source lives. A copy does not follow a binary upgrade, so the picker
// compares the two and warns.
package setup

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/alexander-akhmetov/cattery"
)

// piPackage is the cattery pi package. pi fetches it from git, so setup never
// parses pi's settings file.
const piPackage = "git:github.com/alexander-akhmetov/cattery"

// The Claude Code plugin, as its CLI names it. claudeSource is the repository
// the marketplace comes from, claudeMarketplace the name the manifest gives it,
// and claudePlugin the selector `claude plugin install` takes.
//
// The https URL rather than the `alexander-akhmetov/cattery` shorthand Codex
// takes: Claude clones that shorthand over SSH, and a user following the README
// has no reason to have a key loaded.
const (
	claudeSource      = "https://github.com/alexander-akhmetov/cattery.git"
	claudeMarketplace = "cattery"
	claudePlugin      = "cattery-claude@" + claudeMarketplace
)

// claudeArgs are what setup runs, in order, checked against claude 2.1.233.
//
// `marketplace add` on a source already configured exits 0 and says so, but it
// leaves the clone it fetched alone, and `plugin install` on an installed
// plugin exits 0 without refetching. So `marketplace update` between them is
// the only thing that carries a release to a machine that already has the
// plugin, and it is second because it fails on a marketplace nothing has added
// yet.
//
// `--scope user` because s.run gives the child no stdin, so the scope the
// interactive install would ask for has to be on the command line. No `--yes`:
// that flag is for a plugin installed by running a marketplace-declared
// command, which this one is not.
var claudeArgs = [][]string{
	{"plugin", "marketplace", "add", claudeSource},
	{"plugin", "marketplace", "update", claudeMarketplace},
	{"plugin", "install", claudePlugin, "--scope", "user"},
}

// The Codex plugin, as its CLI names it. codexSource is the repository the
// marketplace comes from, codexMarketplace the name the manifest gives it, and
// codexPlugin the selector `codex plugin add` takes.
const (
	codexSource      = "alexander-akhmetov/cattery"
	codexMarketplace = "cattery"
	codexPlugin      = "cattery-codex@" + codexMarketplace
)

// codexArgs are what setup runs, in order, checked against codex-cli 0.147.0.
//
// `marketplace add` on a source already configured exits 0 and says so, but it
// leaves the snapshot it fetched alone. The upgrade behind it is what a re-run
// after a cattery release needs, and it is second because it fails on a
// marketplace nothing has added yet. `plugin add` on an installed plugin
// re-installs it, so the whole sequence is safe to repeat.
var codexArgs = [][]string{
	{"plugin", "marketplace", "add", codexSource},
	{"plugin", "marketplace", "upgrade", codexMarketplace},
	{"plugin", "add", codexPlugin},
}

// assetMode makes the installed kitty files readable by kitty and writable only
// by their owner. None of them is executed.
const assetMode fs.FileMode = 0o644

// preserveMode tells writeFile to keep the mode a file already has. kitty.conf
// and Claude's settings.json belong to the user, and setup edits only its own
// part of them.
const preserveMode fs.FileMode = 0

// Options configures one `cattery setup` run.
type Options struct {
	// KittyDir is the --kitty-dir flag. Empty falls back to
	// $KITTY_CONFIG_DIRECTORY, then ~/.config/kitty.
	KittyDir string

	// DryRun reports every action and changes nothing. It writes no file, makes
	// no backup, and runs no external command.
	DryRun bool

	// Yes answers the Claude, Codex, pi and opencode questions without asking.
	Yes bool

	// Binary is the path setup writes into the kitty map. Empty asks the
	// running process where it lives.
	Binary string

	// ClaudeDir is where settings.json lives. Empty falls back to
	// $CLAUDE_CONFIG_DIR, then ~/.claude. Setup reads that file only to take
	// out the hooks an older release merged into it.
	ClaudeDir string

	// CodexDir is where Codex's own hooks.json lives. Empty falls back to
	// $CODEX_HOME, then ~/.codex. Setup reads that file and never writes it.
	CodexDir string

	// OpencodeDir is opencode's configuration directory, where the plugin goes.
	// Empty falls back to $XDG_CONFIG_HOME/opencode, then ~/.config/opencode.
	OpencodeDir string

	// LegacyPaths are the files the shell installer left behind. Nil uses the
	// real ones. Setup reports them and never touches them.
	LegacyPaths []string

	// In is the reader the yes/no questions are answered on. Nil means nobody
	// is there to answer, which is how a non-terminal stdin arrives. The agent
	// steps are then skipped unless Yes is set.
	In io.Reader

	// Out receives the report. Nil means os.Stdout.
	Out io.Writer

	// LookPath and RunCommand find and run claude, codex and pi. A test
	// replaces them to record the install commands without launching anything.
	LookPath   func(file string) (string, error)
	RunCommand func(name string, args ...string) error
}

// Run installs cattery and reports what it did.
//
// It fails only on the kitty side, which is the install itself. The Claude,
// Codex, pi and opencode steps belong to other tools. When one of them cannot
// be done, setup says so, prints the command to run by hand, and still reports
// success for its own part.
func Run(opts Options) error {
	s, err := newSession(opts)
	if err != nil {
		return err
	}
	if err := s.kitty(); err != nil {
		return err
	}
	s.claude()
	s.codex()
	s.pi()
	s.opencode()
	s.reportLegacy()
	s.out.line("")
	s.out.line("Reload kitty to finish.")
	return nil
}

// session is one run's resolved configuration.
type session struct {
	opts        Options
	out         *reporter
	answers     *bufio.Reader // nil when there is nobody to ask
	kittyDir    string
	claudeDir   string
	codexDir    string
	opencodeDir string
	binary      string
	legacy      []string
	lookPath    func(string) (string, error)
	run         func(string, ...string) error
}

func newSession(opts Options) (*session, error) {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	binary := opts.Binary
	if binary == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("cannot find this binary's own path: %w", err)
		}
		binary = exe
	}
	kittyDir, err := resolveDir(opts.KittyDir, "KITTY_CONFIG_DIRECTORY", ".config", "kitty")
	if err != nil {
		return nil, err
	}
	claudeDir, err := resolveDir(opts.ClaudeDir, "CLAUDE_CONFIG_DIR", ".claude")
	if err != nil {
		return nil, err
	}
	codexDir, err := resolveDir(opts.CodexDir, "CODEX_HOME", ".codex")
	if err != nil {
		return nil, err
	}
	opencodeDir, err := opencodeDirFor(opts.OpencodeDir)
	if err != nil {
		return nil, err
	}

	s := &session{
		opts:        opts,
		out:         &reporter{w: out, dry: opts.DryRun},
		kittyDir:    kittyDir,
		claudeDir:   claudeDir,
		codexDir:    codexDir,
		opencodeDir: opencodeDir,
		binary:      binary,
		legacy:      opts.LegacyPaths,
		lookPath:    opts.LookPath,
		run:         opts.RunCommand,
	}
	// One reader for every question. A fresh bufio.Reader per question would
	// buffer past the first answer and swallow the next.
	if opts.In != nil {
		s.answers = bufio.NewReader(opts.In)
	}
	if s.legacy == nil {
		s.legacy = legacyPaths(kittyDir)
	}
	if s.lookPath == nil {
		s.lookPath = exec.LookPath
	}
	if s.run == nil {
		s.run = func(name string, args ...string) error {
			cmd := exec.Command(name, args...)
			cmd.Stdout, cmd.Stderr = out, out
			return cmd.Run()
		}
	}
	return s, nil
}

// resolveDir picks a directory: the explicit one, else the environment
// variable, else the home-relative default.
func resolveDir(explicit, env string, home ...string) (string, error) {
	if explicit != "" {
		return filepath.Clean(explicit), nil
	}
	if fromEnv := os.Getenv(env); fromEnv != "" {
		return filepath.Clean(fromEnv), nil
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find the home directory: %w", err)
	}
	return filepath.Join(append([]string{dir}, home...)...), nil
}

// KittyDir is where an install without --kitty-dir puts the kitty files.
func KittyDir() (string, error) {
	return resolveDir("", "KITTY_CONFIG_DIRECTORY", ".config", "kitty")
}

// opencodeDirFor picks opencode's configuration directory. It has its own
// resolver because XDG_CONFIG_HOME names the parent rather than the directory
// itself, which is not the shape resolveDir reads.
func opencodeDirFor(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Clean(explicit), nil
	}
	if config := os.Getenv("XDG_CONFIG_HOME"); config != "" {
		return filepath.Join(config, "opencode"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find the home directory: %w", err)
	}
	return filepath.Join(home, ".config", "opencode"), nil
}

// opencodePlugin is where the plugin file goes under an opencode config
// directory.
func opencodePlugin(dir string) string {
	return filepath.Join(dir, cattery.OpencodePluginDir, cattery.OpencodeFile)
}

// Stale reports whether the install in dir has fallen behind this binary: the
// copies of the kitty files, the managed kitty.conf block, which carries
// settings those files depend on, and the opencode plugin. That happens when an
// upgrade changes one of them and setup has not run since.
//
// A directory holding none of the files is not stale: cattery may not be
// installed there at all. A directory holding some of them is stale, which
// catches an install predating a file the binary now ships. A file that cannot
// be read counts as missing.
//
// The opencode plugin follows the same "absent means not installed" rule on its
// own: a user without opencode must not be told to run setup again. Its
// directory is resolved here rather than passed in, because the picker asks
// about the kitty install and knows nothing about opencode.
func Stale(dir string) bool {
	assets := cattery.KittyFiles()
	installed, missing := 0, 0
	for _, name := range cattery.ManagedFiles {
		want, err := fs.ReadFile(assets, name)
		if err != nil {
			continue
		}
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			missing++
			continue
		}
		installed++
		if string(got) != string(want) {
			return true
		}
	}
	if opencodeStale() {
		return true
	}
	if installed == 0 {
		return false
	}
	if missing > 0 {
		return true
	}
	conf, err := os.ReadFile(filepath.Join(dir, "kitty.conf"))
	if err != nil {
		return false
	}
	return blockStale(string(conf))
}

// opencodeStale reports whether an installed opencode plugin no longer matches
// the embedded one. No plugin, and a directory that cannot be resolved, both
// mean nothing to compare.
func opencodeStale() bool {
	dir, err := opencodeDirFor("")
	if err != nil {
		return false
	}
	want, err := fs.ReadFile(cattery.OpencodeFiles(), cattery.OpencodeFile)
	if err != nil {
		return false
	}
	got, err := os.ReadFile(opencodePlugin(dir))
	if err != nil {
		return false
	}
	return string(got) != string(want)
}

// legacyPaths are what install.sh linked into place.
func legacyPaths(kittyDir string) []string {
	paths := []string{filepath.Join(kittyDir, "cattery_overlay.sh")}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".local", "bin", "cattery-state"))
	}
	return paths
}

// --- kitty ------------------------------------------------------------------

func (s *session) kitty() error {
	assets := cattery.KittyFiles()
	if !s.opts.DryRun {
		if err := os.MkdirAll(s.kittyDir, 0o755); err != nil {
			return err
		}
	}

	for _, name := range cattery.ManagedFiles {
		content, err := fs.ReadFile(assets, name)
		if err != nil {
			return err
		}
		if err := s.install(filepath.Join(s.kittyDir, name), content); err != nil {
			return err
		}
	}
	if err := s.tabBar(assets); err != nil {
		return err
	}
	return s.kittyConf()
}

// install writes one embedded file as a regular file. It replaces a symlink
// instead of following it: the shell installer left links into a checkout, and
// writing through one would edit the checkout.
func (s *session) install(path string, content []byte) error {
	info, err := os.Lstat(path)
	if err == nil && info.Mode()&fs.ModeSymlink != 0 {
		s.out.act("replaced", "would replace", shorten(path)+" (was a symlink)")
		if !s.opts.DryRun {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	} else if err == nil {
		if current, err := os.ReadFile(path); err == nil && string(current) == string(content) {
			s.out.plain("ok", shorten(path))
			return nil
		}
	}
	s.out.act("wrote", "would write", shorten(path))
	if s.opts.DryRun {
		return nil
	}
	return writeFile(path, content, assetMode)
}

// tabBar handles kitty's tab title renderer. An existing one belongs to the
// user, so setup prints the changes it needs instead of editing it.
func (s *session) tabBar(assets fs.FS) error {
	path := filepath.Join(s.kittyDir, cattery.TabBarFile)
	existing, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		content, err := fs.ReadFile(assets, cattery.TabBarFile)
		if err != nil {
			return err
		}
		return s.install(path, content)
	case err != nil:
		return err
	case strings.Contains(string(existing), "cattery_tab"):
		s.out.plain("ok", shorten(path)+" (already calls cattery_tab)")
		return nil
	default:
		s.out.plain("kept", shorten(path)+" (yours; add the agent glyph by hand)")
		s.out.block(tabBarInstructions)
		return nil
	}
}

// tabBarInstructions is what an existing tab_bar.py needs to draw the marker.
// kitty loads that file with runpy.run_path, which does not extend sys.path, so
// the file has to add its own directory first and guard the import. An
// ImportError at module scope runs before draw_title is defined and disables
// the whole tab bar.
const tabBarInstructions = `    import os
    import sys

    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
    try:
        from cattery_tab import agent_prefix
    except Exception:
        def agent_prefix(data):
            return ""

    # then put agent_prefix(data) in front of the title inside draw_title:
    #     return f" {index}: {agent_prefix(data)}{fmt.fg.tab}{title}"`

func (s *session) kittyConf() error {
	path := filepath.Join(s.kittyDir, "kitty.conf")
	current, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	block := renderBlock(s.binary)
	merged, mergeErr := mergeBlock(string(current), block)
	target := resolveLink(path)
	switch {
	case mergeErr != nil:
		// Markers that do not pair up are reported and left alone. Repairing
		// them means deciding which half of the file the user meant to keep. The
		// install still stands, so this is a report and not a failure.
		s.out.plain("kept", shorten(path)+": "+mergeErr.Error())
		s.out.block(block)
	case merged == string(current):
		s.out.plain("ok", shorten(path)+" (cattery block)")
	default:
		s.out.act("updated", "would update", label(path, target)+" (cattery block)")
		if !s.opts.DryRun {
			return writeFile(target, []byte(merged), preserveMode)
		}
	}
	return nil
}

// --- agents -----------------------------------------------------------------

// claude installs the Claude Code plugin through Claude's own CLI, then takes
// the hooks an older release merged into settings.json back out. The plugin
// comes from the published repository rather than from this checkout, so a run
// here fetches whatever that repository holds now.
//
// Nothing notices that plugin going stale: Stale compares the installed kitty
// files and the opencode plugin, neither of which this step touches. Claude
// trusts a user-scope plugin's hooks on install, so there is no gate to explain
// the way Codex needs one.
func (s *session) claude() {
	s.out.line("")
	path, err := s.lookPath("claude")
	if err != nil {
		s.out.plain("skipped", "claude is not on PATH")
		s.out.block(claudeInstructions())
		return
	}
	if !s.consent("Install the cattery Claude Code plugin?") {
		// One line per command, so a decline still leaves the three to run by
		// hand. settings.json is left alone on this path: hooks an older release
		// merged in there are the only thing publishing the state, and taking
		// them out without the plugin to replace them would leave the user with
		// neither.
		for _, args := range claudeArgs {
			s.out.plain("skipped", claudeCommand(args))
		}
		s.out.block(claudeStaleInstructions)
		return
	}
	// The install is the last step, and the only one whose success says the
	// plugin is in place: either marketplace step can fail on a machine that
	// already has it. A dry run runs nothing and still reports the removal.
	installed := s.opts.DryRun
	for i, args := range claudeArgs {
		command := claudeCommand(args)
		s.out.act("ran", "would run", command)
		if s.opts.DryRun {
			continue
		}
		err := s.run(path, args...)
		if err != nil {
			s.out.plain("failed", command+": "+err.Error())
		}
		if i == len(claudeArgs)-1 {
			installed = err == nil
		}
	}
	s.out.block(claudeStaleInstructions)
	if installed {
		s.claudeSettings()
	}
}

// claudeSettings takes cattery's own hooks out of settings.json, which is where
// releases before the plugin put them. Both sets fire otherwise, and every state
// is published twice.
//
// It runs only behind an install that worked. A user who declined, who has no
// claude on PATH, or whose install failed keeps the hooks, because those are
// then still the only thing publishing their state. Silent when the file holds
// nothing of cattery's.
func (s *session) claudeSettings() {
	path := filepath.Join(s.claudeDir, "settings.json")
	current, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.out.plain("kept", shorten(path)+": "+err.Error())
		}
		return
	}
	cleaned, removed, err := removeCatteryHooks(current)
	if err != nil {
		s.out.plain("kept", shorten(path)+": "+err.Error())
		return
	}
	if removed == 0 {
		return
	}

	// One backup, kept forever. A second run would otherwise overwrite the only
	// copy of the settings from before cattery.
	backup := path + ".cattery-bak"
	target := resolveLink(path)
	detail := fmt.Sprintf("%s (%d hooks; the plugin runs them now)", label(path, target), removed)
	_, backupErr := os.Stat(backup)
	writeBackup := errors.Is(backupErr, os.ErrNotExist)
	if writeBackup {
		detail += ", backup at " + filepath.Base(backup)
	}

	s.out.act("removed", "would remove", detail)
	if s.opts.DryRun {
		return
	}
	if writeBackup {
		// The backup goes beside the link, not beside the file it points at, so
		// a settings.json in a dotfiles checkout gets no untracked copy dropped
		// into that repository. Its mode comes from the file being copied, since
		// the backup path does not exist yet: settings.json can hold an API key,
		// and a 0644 copy of a 0600 file hands it to everyone on the machine.
		if err := writeFile(backup, current, modeOf(path)); err != nil {
			s.out.plain("failed", shorten(backup)+": "+err.Error())
			return
		}
	}
	if err := writeFile(target, cleaned, preserveMode); err != nil {
		s.out.plain("failed", shorten(target)+": "+err.Error())
	}
}

// claudeCommand renders one step as the line a user would type.
func claudeCommand(args []string) string { return "claude " + strings.Join(args, " ") }

// claudeInstructions lists the steps for a user without claude on PATH.
func claudeInstructions() string {
	lines := make([]string, 0, 1+len(claudeArgs))
	lines = append(lines, "    Once claude is installed, run:")
	for _, args := range claudeArgs {
		lines = append(lines, "      "+claudeCommand(args))
	}
	return strings.Join(lines, "\n") + "\n\n" + claudeStaleInstructions
}

// claudeStaleInstructions is what setup cannot do for the user.
const claudeStaleInstructions = `    The plugin is fetched from the published repository, so nothing notices it
    going stale. Run these again after every cattery upgrade.`

// codex installs the Codex plugin through Codex's own CLI. The plugin comes
// from the published repository rather than from this checkout, so a run here
// fetches whatever that repository holds now.
//
// Two things it cannot do, which is why both are printed rather than done.
// Codex hashes a plugin's hooks and skips them without a word until the user
// trusts them in /hooks. And nothing notices the published plugin going stale:
// Stale compares the installed kitty files, which this step does not touch.
func (s *session) codex() {
	s.out.line("")
	path, err := s.lookPath("codex")
	if err != nil {
		s.out.plain("skipped", "codex is not on PATH")
		s.out.block(codexInstructions())
		return
	}
	if !s.consent("Install the cattery Codex plugin?") {
		// One line per command, so a decline still leaves the three to run by
		// hand. Only the trust step needs the block behind them.
		for _, args := range codexArgs {
			s.out.plain("skipped", codexCommand(args))
		}
		s.out.block(codexTrustInstructions)
		return
	}
	for _, args := range codexArgs {
		command := codexCommand(args)
		s.out.act("ran", "would run", command)
		if s.opts.DryRun {
			continue
		}
		if err := s.run(path, args...); err != nil {
			s.out.plain("failed", command+": "+err.Error())
		}
	}
	s.out.block(codexTrustInstructions)
	s.reportCodexHooks()
}

// codexCommand renders one step as the line a user would type.
func codexCommand(args []string) string { return "codex " + strings.Join(args, " ") }

// codexInstructions lists the steps for a user without codex on PATH.
func codexInstructions() string {
	lines := make([]string, 0, 1+len(codexArgs))
	lines = append(lines, "    Once codex is installed, run:")
	for _, args := range codexArgs {
		lines = append(lines, "      "+codexCommand(args))
	}
	return strings.Join(lines, "\n") + "\n\n" + codexTrustInstructions
}

// codexTrustInstructions is what setup cannot do for the user.
const codexTrustInstructions = `    Then open /hooks in Codex and trust each cattery-codex@cattery entry.
    Codex skips an untrusted hook without a word.

    The plugin is fetched from the published repository, so nothing notices it
    going stale. Run these again after every cattery upgrade.`

// reportCodexHooks names hand-written cattery entries in Codex's own
// hooks.json. Those fire beside the plugin's and publish every state twice,
// and a hand-written one predating this release carries no --kind, so it tags
// the session claude and publishes `claude --resume <codex id>`, which reopens
// nothing. Setup does not own that file, so it says so and leaves it alone.
func (s *session) reportCodexHooks() {
	path := filepath.Join(s.codexDir, "hooks.json")
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(data, []byte("cattery state")) {
		return
	}
	s.out.plain("note", shorten(path)+" also runs `cattery state`; those entries double up with the plugin, and one without --kind tags a Codex session as Claude. Remove them.")
}

func (s *session) pi() {
	s.out.line("")
	command := "pi install " + piPackage
	path, err := s.lookPath("pi")
	if err != nil {
		s.out.plain("skipped", "pi is not on PATH")
		s.out.block("    Once pi is installed, run:\n      " + command)
		return
	}
	if !s.consent("Install the cattery pi package?") {
		s.out.plain("skipped", command)
		return
	}
	s.out.act("ran", "would run", command)
	if s.opts.DryRun {
		return
	}
	if err := s.run(path, "install", piPackage); err != nil {
		s.out.plain("failed", command+": "+err.Error())
	}
}

// opencode installs the plugin into opencode's own configuration directory,
// where it is auto-loaded: opencode scans "{plugin,plugins}/*.{ts,js}" there and
// needs nothing written into opencode.json.
//
// It is the one agent extension that ships inside the binary rather than being
// fetched from the published repository, which is what lets Stale check it.
func (s *session) opencode() {
	s.out.line("")
	path := opencodePlugin(s.opencodeDir)
	if _, err := s.lookPath("opencode"); err != nil {
		s.out.plain("skipped", "opencode is not on PATH")
		s.out.block("    Once opencode is installed, run cattery setup again to write:\n      " + shorten(path))
		return
	}
	if !s.consent("Install the cattery opencode plugin into " + shorten(path) + "?") {
		s.out.plain("skipped", shorten(path))
		return
	}
	content, err := fs.ReadFile(cattery.OpencodeFiles(), cattery.OpencodeFile)
	if err != nil {
		s.out.plain("failed", shorten(path)+": "+err.Error())
		return
	}
	if !s.opts.DryRun {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			s.out.plain("failed", shorten(path)+": "+err.Error())
			return
		}
	}
	if err := s.install(path, content); err != nil {
		s.out.plain("failed", shorten(path)+": "+err.Error())
	}
}

// legacyLeftovers reports what the shell installer left behind. Setup never
// deletes them, because it did not write them.
func (s *session) legacyLeftovers() []string {
	var found []string
	for _, path := range s.legacy {
		if _, err := os.Lstat(path); err == nil {
			found = append(found, shorten(path))
		}
	}
	return found
}

func (s *session) reportLegacy() {
	found := s.legacyLeftovers()
	if len(found) == 0 {
		return
	}
	s.out.line("")
	s.out.plain("note", "install.sh left these behind; nothing needs them now:")
	for _, path := range found {
		s.out.line("        " + path)
	}
}

// consent asks a yes/no question with yes as the default.
//
// --yes answers every question. A nil reader means nobody is there, which is
// what a piped stdin gives, so setup skips the step and prints the command to
// run by hand. That check comes before the dry-run check, so a dry run with
// nobody there reports the skip a real run would take. A dry run with somebody
// there asks nothing, because it changes nothing.
func (s *session) consent(question string) bool {
	if s.opts.Yes {
		return true
	}
	if s.answers == nil {
		s.out.plain("note", "stdin is not a terminal; re-run with --yes to accept")
		return false
	}
	if s.opts.DryRun {
		return true
	}
	s.out.line(question + " [Y/n] ")
	line, err := s.answers.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	default:
		return false
	}
}

// --- output and files -------------------------------------------------------

// reporter prints one line per action, in the order the actions happen. A dry
// run says "would", so no line reads as something that happened.
type reporter struct {
	w   io.Writer
	dry bool
}

func (r *reporter) act(did, would, detail string) {
	if r.dry {
		r.plain(would, detail)
		return
	}
	r.plain(did, detail)
}

func (r *reporter) plain(verb, detail string) {
	width := 8
	if r.dry {
		width = 13
	}
	fmt.Fprintf(r.w, "%-*s%s\n", width, verb, detail)
}

func (r *reporter) line(s string) { fmt.Fprintln(r.w, s) }

// block prints an indented snippet the user has to act on by hand.
func (r *reporter) block(s string) {
	fmt.Fprintf(r.w, "\n%s\n\n", s)
}

// writeFile replaces path atomically, so an interrupted run cannot leave a
// half-written kitty.conf or settings.json behind. The temporary file shares
// the directory, because a rename across filesystems fails.
//
// The rename replaces whatever path is, symlink included. A caller holding a
// link passes the file it points at; see resolveLink.
func writeFile(path string, content []byte, mode fs.FileMode) error {
	if mode == preserveMode {
		mode = modeOf(path)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".cattery-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// modeOf is a file's permission bits, or assetMode when there is no file to
// read them from.
func modeOf(path string) fs.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return assetMode
}

// resolveLink follows a symlink to the file it points at, so an edit reaches
// that file instead of replacing the link with a regular file. kitty.conf and
// Claude's settings.json are often links into a dotfiles checkout. Detaching
// one would stop later edits in the checkout from reaching kitty and Claude,
// while `git status` there stayed clean. A path that is not a link, and a link
// pointing nowhere, come back unchanged.
func resolveLink(path string) string {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&fs.ModeSymlink == 0 {
		return path
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}

// label names the file a write reaches. It names both ends of a symlink,
// because a report saying only kitty.conf would hide which file changed.
func label(path, target string) string {
	if target == path {
		return shorten(path)
	}
	return shorten(path) + " -> " + shorten(target)
}

// shorten replaces the home directory with ~, to keep the report readable on a
// narrow terminal.
func shorten(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rest
	}
	return path
}
