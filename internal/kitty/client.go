// Package kitty talks to a running kitty instance through its remote-control
// CLI (`kitten @ ...`). It enumerates windows that publish the agent-state
// user variables and focuses a chosen window.
package kitty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Agent is a single kitty window carrying AGENT_DISPLAY, which the kitty
// watcher derives from the agent's own AGENT_STATE.
type Agent struct {
	ID      int
	Kind    string // AGENT_KIND, e.g. "pi" or "claude"
	Display string // AGENT_DISPLAY: blocked, done, working, idle
	Title   string
	CWD     string
	Since   time.Time // from AGENT_SINCE; zero when unknown
	Msg     string    // AGENT_MSG: the latest user prompt, when published

	// CreatedAt is when kitty opened the window, a stable session age. Since
	// resets on every display transition.
	CreatedAt time.Time

	// Worktrees of one repository share Project and ProjectKey but differ in
	// Root and Branch, so the picker groups them together and still tells
	// them apart.
	Project    string // repo label, e.g. "dotfiles"
	ProjectKey string // stable project identity: the git common dir, else the cwd
	Root       string // worktree top level; empty outside git
	Branch     string // empty on a detached HEAD or outside git
}

// repo is what one git lookup says about a working directory.
type repo struct {
	project    string
	projectKey string
	root       string
	branch     string
}

func (a *Agent) setRepo(r repo) {
	a.Project, a.ProjectKey, a.Root, a.Branch = r.project, r.projectKey, r.root, r.branch
}

const (
	defaultRepoTTL  = 5 * time.Second
	maxRepoWorkers  = 4
	gitLookupBudget = 500 * time.Millisecond
)

type repoCacheEntry struct {
	repo    repo
	expires time.Time
}

// Client runs kitty remote-control commands.
type Client struct {
	kitten string

	// lookup resolves one directory's git facts. A field, so tests can count
	// lookups without spawning git. nil means gitRepo.
	lookup func(ctx context.Context, cwd string) (found repo, answered bool)

	mu      sync.Mutex
	repo    map[string]repoCacheEntry // cwd -> recently observed repo facts
	repoTTL time.Duration
}

// NewClient resolves the kitten binary and returns a ready client.
func NewClient() *Client {
	return &Client{
		kitten:  kittenPath(),
		lookup:  gitRepo,
		repo:    map[string]repoCacheEntry{},
		repoTTL: defaultRepoTTL,
	}
}

func kittenPath() string {
	if p, err := exec.LookPath("kitten"); err == nil {
		return p
	}
	// macOS .app bundle layout fallback, for a PATH without the kitty bundle.
	const fallback = "/Applications/kitty.app/Contents/MacOS/kitten"
	if _, err := os.Stat(fallback); err == nil {
		return fallback
	}
	return "kitten"
}

// ListAgents returns the agent windows known to kitty, grouped by project.
func (c *Client) ListAgents(ctx context.Context) ([]Agent, error) {
	out, err := c.ls(ctx)
	if err != nil {
		return nil, err
	}
	agents, err := parseAgents(out)
	if err != nil {
		return nil, err
	}
	c.populateRepos(ctx, agents)
	sortAgents(agents)
	return agents, nil
}

// FocusWindow focuses the kitty window with the given id, switching OS window
// if needed. kitty explains a rejection on stderr ("no matching window"), so a
// failure carries that reason and the target id.
func (c *Client) FocusWindow(ctx context.Context, id int) error {
	return run(focusCommand(ctx, c.kitten, id), window(id))
}

// run executes one remote-control command and keeps kitty's explanation for a
// failure. CombinedOutput carries it: kitty writes the reason on stderr, and
// the error alone says "exit status 1". what names the target, because a picker
// banner shows the message with nothing around it.
func run(cmd *exec.Cmd, what string) error {
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if reason := condense(string(out)); reason != "" {
		return fmt.Errorf("%s: %s", what, reason)
	}
	return fmt.Errorf("%s: %w", what, err)
}

func window(id int) string { return "window " + strconv.Itoa(id) }

// condense flattens command output to one line. The picker renders errors in a
// single-line banner, and kitty's tracebacks span many lines.
func condense(out string) string {
	return strings.Join(strings.Fields(out), " ")
}

// commandError keeps kitty's explanation for a failed command. Output() stashes
// stderr on the ExitError but leaves Error() as "exit status 1", which tells
// the user nothing in the picker's error banner.
func commandError(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if reason := condense(string(exitErr.Stderr)); reason != "" {
			return errors.New(reason)
		}
	}
	return err
}

func focusCommand(ctx context.Context, kitten string, id int) *exec.Cmd {
	return exec.CommandContext(ctx, kitten, "@", "focus-window", "--match", "id:"+strconv.Itoa(id))
}

// SetUserVars publishes user variables on one kitty window, in the order given.
// Each entry is "NAME=value" to set a variable and a bare "NAME" to delete it,
// the form `kitten @ set-user-vars` takes.
//
// The state writer uses this when its OSC escape cannot reach a terminal, which
// is every Claude command hook. It matches by window id, so the value reaches
// the window the agent runs in even when another window is active.
func (c *Client) SetUserVars(ctx context.Context, id int, vars []string) error {
	if len(vars) == 0 {
		return nil
	}
	return run(setUserVarsCommand(ctx, c.kitten, id, vars), window(id))
}

func setUserVarsCommand(ctx context.Context, kitten string, id int, vars []string) *exec.Cmd {
	args := append([]string{"@", "set-user-vars", "--match", "id:" + strconv.Itoa(id)}, vars...)
	return exec.CommandContext(ctx, kitten, args...)
}

// Action runs one kitty action over remote control, as `kitten @ action <arg>`.
//
// The action name and its arguments go in a single string, which is what
// kitty's parser wants. Separate argv entries make kitty open a window titled
// "Invalid <action> command line". Quote any path inside arg with
// internal/shellquote.
func (c *Client) Action(ctx context.Context, arg string) error {
	return run(actionCommand(ctx, c.kitten, arg), fmt.Sprintf("action %q", arg))
}

func actionCommand(ctx context.Context, kitten, arg string) *exec.Cmd {
	return exec.CommandContext(ctx, kitten, "@", "action", arg)
}

// SendText types text into one kitty window, as if the user had typed it. It
// adds no carriage return; the caller appends one to run what it sent.
//
// kitty reports no error when a match finds nothing, so the id must come from
// Windows.
func (c *Client) SendText(ctx context.Context, id int, text string) error {
	return run(sendTextCommand(ctx, c.kitten, id, text), window(id))
}

func sendTextCommand(ctx context.Context, kitten string, id int, text string) *exec.Cmd {
	// The text goes in on stdin, which kitty documents as "sent as is, not
	// interpreted for escapes". kitty reads Python escapes out of a positional
	// text argument: the POSIX '\'' idiom shellquote emits would arrive as ''',
	// and a \n in a path would become a real newline that runs the command.
	cmd := exec.CommandContext(ctx, kitten, "@", "send-text", "--match", "id:"+strconv.Itoa(id), "--stdin")
	cmd.Stdin = strings.NewReader(text)
	return cmd
}

// Windows returns every kitty window, unfiltered. Session restore needs this
// rather than ListAgents: a freshly restored window has published no agent
// state yet, and carries only the AGENT_RESUME from its snapshot.
func (c *Client) Windows(ctx context.Context) ([]Window, error) {
	out, err := c.ls(ctx)
	if err != nil {
		return nil, err
	}
	return parseWindows(out)
}

// ls is the window inventory as JSON. It calls Output() instead of run(),
// because kitty's answer is the point; commandError digs the explanation out of
// the ExitError instead of mixing stderr into the JSON.
func (c *Client) ls(ctx context.Context) ([]byte, error) {
	out, err := exec.CommandContext(ctx, c.kitten, "@", "ls").Output()
	if err != nil {
		return nil, commandError(err)
	}
	return out, nil
}

// populateRepos resolves each unique cwd once, concurrently, then fills every
// agent that shares it. The worker limit keeps a large inventory from spawning
// an unbounded number of git processes.
func (c *Client) populateRepos(ctx context.Context, agents []Agent) {
	indices := make(map[string][]int)
	for i, agent := range agents {
		if agent.CWD != "" {
			indices[agent.CWD] = append(indices[agent.CWD], i)
		}
	}
	if len(indices) == 0 {
		return
	}

	cwds := make([]string, 0, len(indices))
	for cwd := range indices {
		cwds = append(cwds, cwd)
	}
	repos := make([]repo, len(cwds))
	workers := make(chan struct{}, maxRepoWorkers)
	var wg sync.WaitGroup
	for i, cwd := range cwds {
		workers <- struct{}{}
		wg.Go(func() {
			defer func() { <-workers }()
			repos[i] = c.repoFor(ctx, cwd)
		})
	}
	wg.Wait()

	for i, cwd := range cwds {
		for _, j := range indices[cwd] {
			agents[j].setRepo(repos[i])
		}
	}
}

// repoFor returns the recent git facts for cwd. An answer from git is cached
// for the TTL, including "this is not a repository". Without that, one agent
// outside git would start a git process on every reload.
//
// A lookup that ran out of time is not an answer, and is not cached. Caching it
// would hold the folder fallback for the whole TTL, which drops the branch from
// the row and splits worktrees of one repository into separate headings.
func (c *Client) repoFor(ctx context.Context, cwd string) repo {
	if cwd == "" {
		return repo{}
	}
	now := time.Now()
	c.mu.Lock()
	if cached, ok := c.repo[cwd]; ok && now.Before(cached.expires) {
		c.mu.Unlock()
		return cached.repo
	}
	c.mu.Unlock()

	lookup := c.lookup
	if lookup == nil {
		lookup = gitRepo
	}
	found, answered := lookup(ctx, cwd)

	if c.repoTTL > 0 && answered {
		c.mu.Lock()
		if c.repo == nil {
			c.repo = make(map[string]repoCacheEntry)
		}
		c.repo[cwd] = repoCacheEntry{repo: found, expires: now.Add(c.repoTTL)}
		c.mu.Unlock()
	}
	return found
}

// gitRepo asks git about one directory, under its own time budget. It reports
// whether git answered. A lookup killed by the budget or by a cancelled reload
// returns the folder fallback with answered false, which the caller must not
// cache.
func gitRepo(ctx context.Context, cwd string) (repo, bool) {
	cctx, cancel := context.WithTimeout(ctx, gitLookupBudget)
	defer cancel()
	out, _ := exec.CommandContext(cctx, "git", "-C", cwd, "rev-parse",
		"--path-format=absolute", "--git-common-dir", "--show-toplevel", "--abbrev-ref", "HEAD").Output()
	return parseRepo(cwd, out), cctx.Err() == nil
}

// fallbackRepo is what an agent's directory looks like with no git answer:
// grouped by the folder itself.
func fallbackRepo(cwd string) repo {
	if cwd == "" {
		return repo{}
	}
	return repo{project: filepath.Base(cwd), projectKey: cwd}
}

// parseRepo reads the three-line rev-parse output: git common dir, worktree top
// level, branch. It reads line by line instead of all-or-nothing, because git
// prints the paths it resolved before failing on a later argument. A repository
// with no commits prints both paths, then exits 128.
func parseRepo(cwd string, out []byte) repo {
	found := fallbackRepo(cwd)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 0 && strings.HasPrefix(lines[0], "/") {
		found.projectKey = filepath.Clean(lines[0])
		found.project = projectName(found.projectKey)
	}
	if len(lines) > 1 && strings.HasPrefix(lines[1], "/") {
		found.root = filepath.Clean(lines[1])
	}
	if len(lines) > 2 {
		if branch := strings.TrimSpace(lines[2]); branch != "HEAD" {
			found.branch = branch // "HEAD" is a detached or unborn head, not a name
		}
	}
	return found
}

// projectName labels a git common dir: the directory holding ".git" for a
// normal checkout, or the bare repository's own name.
func projectName(commonDir string) string {
	base := filepath.Base(commonDir)
	if base == ".git" {
		return filepath.Base(filepath.Dir(commonDir))
	}
	return strings.TrimSuffix(base, ".git")
}

// --- kitten @ ls JSON shape (only the fields we use) ------------------------

type rawWindow struct {
	ID          int               `json:"id"`
	Title       string            `json:"title"`
	CWD         string            `json:"cwd"`
	CreatedAt   int64             `json:"created_at"` // unix nanoseconds
	UserVars    map[string]string `json:"user_vars"`
	AtPrompt    bool              `json:"at_prompt"`
	SessionName string            `json:"session_name"`
}

type rawTab struct {
	Windows []rawWindow `json:"windows"`
}

type rawOSWindow struct {
	Tabs []rawTab `json:"tabs"`
}

// Window is one kitty window as `kitten @ ls` reports it, with no filtering.
type Window struct {
	ID       int
	Title    string
	CWD      string
	UserVars map[string]string

	// CreatedAt is when kitty opened the window; zero when kitty did not say.
	CreatedAt time.Time

	// SessionName is the session file that created this window, named after the
	// file's basename without its extension. Empty for a window the user
	// opened. kitty records it per window; its tab dictionaries have no such
	// key.
	SessionName string

	// AtPrompt is true once the shell has drawn a prompt and is waiting. Restore
	// uses it to decide when a restored window can be typed into.
	AtPrompt bool
}

// parseWindows decodes `kitten @ ls` output into every window it reports. It
// keeps windows with no user variables, because a window restored from a
// snapshot has published no agent state yet.
func parseWindows(data []byte) ([]Window, error) {
	var osWindows []rawOSWindow
	if err := json.Unmarshal(data, &osWindows); err != nil {
		return nil, err
	}

	var windows []Window
	for _, osw := range osWindows {
		for _, tab := range osw.Tabs {
			for _, w := range tab.Windows {
				window := Window{
					ID:          w.ID,
					Title:       w.Title,
					CWD:         w.CWD,
					UserVars:    w.UserVars,
					SessionName: w.SessionName,
					AtPrompt:    w.AtPrompt,
				}
				if w.CreatedAt > 0 {
					window.CreatedAt = time.Unix(0, w.CreatedAt)
				}
				windows = append(windows, window)
			}
		}
	}
	return windows, nil
}

// parseAgents keeps the windows that published an AGENT_DISPLAY value. It never
// touches git and never sorts. ListAgents fills in the repo facts the ordering
// depends on, then sorts.
func parseAgents(data []byte) ([]Agent, error) {
	windows, err := parseWindows(data)
	if err != nil {
		return nil, err
	}

	var agents []Agent
	for _, w := range windows {
		display := w.UserVars["AGENT_DISPLAY"]
		if display == "" {
			continue
		}
		a := Agent{
			ID:        w.ID,
			Kind:      w.UserVars["AGENT_KIND"],
			Display:   display,
			Title:     w.Title,
			CWD:       w.CWD,
			Msg:       w.UserVars["AGENT_MSG"],
			CreatedAt: w.CreatedAt,
		}
		if raw := w.UserVars["AGENT_SINCE"]; raw != "" {
			if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
				a.Since = time.Unix(secs, 0)
			}
		}
		agents = append(agents, a)
	}
	return agents, nil
}

// sortAgents groups agents by project and orders each group oldest session
// first. Project order is alphabetical, so a group never moves under the user
// while agents change state; the status filter tabs cover triage. Agents whose
// project could not be determined go last.
func sortAgents(agents []Agent) {
	sort.SliceStable(agents, func(i, j int) bool {
		a, b := agents[i], agents[j]
		if (a.ProjectKey == "") != (b.ProjectKey == "") {
			return b.ProjectKey == ""
		}
		if la, lb := strings.ToLower(a.Project), strings.ToLower(b.Project); la != lb {
			return la < lb
		}
		// Two repositories can share a label; the common dir separates them.
		if a.ProjectKey != b.ProjectKey {
			return a.ProjectKey < b.ProjectKey
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
}
