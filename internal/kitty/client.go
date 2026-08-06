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

	// CreatedAt is when kitty opened the window. Unlike Since, which resets on
	// every display transition, it is a stable session age.
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

	// lookup resolves one directory's git facts. It is a field so tests can
	// count lookups without spawning git; nil means gitRepo.
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
	out, err := exec.CommandContext(ctx, c.kitten, "@", "ls").Output()
	if err != nil {
		return nil, commandError(err)
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
// if needed. kitty explains a rejection on stderr ("no matching window"), so
// failures carry that reason and the target id instead of a bare exit status.
func (c *Client) FocusWindow(ctx context.Context, id int) error {
	out, err := focusCommand(ctx, c.kitten, id).CombinedOutput()
	if err == nil {
		return nil
	}
	if reason := condense(string(out)); reason != "" {
		return fmt.Errorf("window %d: %s", id, reason)
	}
	return fmt.Errorf("window %d: %w", id, err)
}

// condense flattens command output to one line. The picker renders errors in a
// single-line banner, and kitty's tracebacks span many lines.
func condense(out string) string {
	return strings.Join(strings.Fields(out), " ")
}

// commandError keeps kitty's own explanation for a failed command. Output()
// stashes stderr on the ExitError but leaves Error() as a bare "exit status 1",
// which tells the user nothing in the picker's error banner.
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
// which is what `kitten @ set-user-vars` itself takes.
//
// The state writer uses this when its OSC escape cannot reach a terminal, which
// is every Claude command hook. Matching by window id rather than the active
// window means the value lands on the window the agent runs in.
func (c *Client) SetUserVars(ctx context.Context, id int, vars []string) error {
	if len(vars) == 0 {
		return nil
	}
	out, err := setUserVarsCommand(ctx, c.kitten, id, vars).CombinedOutput()
	if err == nil {
		return nil
	}
	if reason := condense(string(out)); reason != "" {
		return fmt.Errorf("window %d: %s", id, reason)
	}
	return fmt.Errorf("window %d: %w", id, err)
}

func setUserVarsCommand(ctx context.Context, kitten string, id int, vars []string) *exec.Cmd {
	args := append([]string{"@", "set-user-vars", "--match", "id:" + strconv.Itoa(id)}, vars...)
	return exec.CommandContext(ctx, kitten, args...)
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

// repoFor returns the recent git facts for cwd. An answer from git is cached for
// the TTL, including "this is not a repository": without that, a single agent
// outside git would start a git process on every reload, for as long as the
// picker stays open.
//
// A lookup that ran out of time is not an answer, so it is not cached. Caching
// it would keep the folder fallback for the whole TTL, which drops the branch
// from the row and splits worktrees of one repository into separate headings,
// and would suppress the retry that fixes it.
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
// whether git answered: a lookup killed by the budget or by a cancelled reload
// returns the folder fallback with answered false, and the caller must not cache
// that as the truth about the directory.
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
// level, branch. It takes the output line by line instead of all-or-nothing,
// because git prints the paths it resolved before it fails on a later argument
// (a repository with no commits prints both paths, then exits 128).
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
	ID        int               `json:"id"`
	Title     string            `json:"title"`
	CWD       string            `json:"cwd"`
	CreatedAt int64             `json:"created_at"` // unix nanoseconds
	UserVars  map[string]string `json:"user_vars"`
}

type rawTab struct {
	Windows []rawWindow `json:"windows"`
}

type rawOSWindow struct {
	Tabs []rawTab `json:"tabs"`
}

// parseAgents decodes `kitten @ ls` output and keeps windows that published an
// AGENT_DISPLAY value. It does not touch git and does not sort: ListAgents
// fills in the repo facts the ordering depends on, then sorts.
func parseAgents(data []byte) ([]Agent, error) {
	var osWindows []rawOSWindow
	if err := json.Unmarshal(data, &osWindows); err != nil {
		return nil, err
	}

	var agents []Agent
	for _, osw := range osWindows {
		for _, tab := range osw.Tabs {
			for _, w := range tab.Windows {
				display := w.UserVars["AGENT_DISPLAY"]
				if display == "" {
					continue
				}
				a := Agent{
					ID:      w.ID,
					Kind:    w.UserVars["AGENT_KIND"],
					Display: display,
					Title:   w.Title,
					CWD:     w.CWD,
				}
				if w.CreatedAt > 0 {
					a.CreatedAt = time.Unix(0, w.CreatedAt)
				}
				if raw := w.UserVars["AGENT_SINCE"]; raw != "" {
					if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
						a.Since = time.Unix(secs, 0)
					}
				}
				a.Msg = w.UserVars["AGENT_MSG"]
				agents = append(agents, a)
			}
		}
	}
	return agents, nil
}

// sortAgents groups agents by project and orders each group oldest session
// first. Project order is alphabetical, not most-urgent-first: it does not move
// under the user while agents change state, and the status filter tabs cover
// triage. Agents whose project could not be determined go last.
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
