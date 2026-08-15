// Package agent holds the half of cattery's inventory that no host owns: what
// an agent is, how the picker identifies one, and the git facts it groups by.
// kitty windows and tmux panes both become agents, and neither host package has
// to import the other to say so.
package agent

import (
	"context"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The hosts an agent can run in. Host decides how the picker reaches it: a
// kitty window is focused, a tmux pane is attached to read-only.
const (
	HostKitty = "kitty"
	HostTmux  = "tmux"
)

// Agent is one running agent, wherever it lives: a kitty window carrying
// AGENT_DISPLAY, or a tmux pane carrying @AGENT_STATE.
type Agent struct {
	ID      int
	Kind    string // AGENT_KIND, e.g. "pi" or "claude"
	Display string // blocked, done, working, idle
	Title   string
	CWD     string
	Since   time.Time // from AGENT_SINCE; zero when unknown
	Msg     string    // AGENT_MSG: the latest user prompt, when published

	// Host is where the agent runs: HostKitty or HostTmux.
	Host string

	// Target is the tmux "<session>:<window index>.<pane id>" to attach to.
	// Empty for a kitty agent, which is reached by window id instead.
	Target string

	// CreatedAt is when the host opened the window, a stable session age. Since
	// resets on every display transition. Zero when the host does not report it.
	CreatedAt time.Time

	// Worktrees of one repository share Project and ProjectKey but differ in
	// Root and Branch, so the picker groups them together and still tells
	// them apart.
	Project    string // repo label, e.g. "dotfiles"
	ProjectKey string // stable project identity: the git common dir, else the cwd
	Root       string // worktree top level; empty outside git
	Branch     string // empty on a detached HEAD or outside git
}

// Key identifies an agent across reloads, and across hosts. A kitty window id
// and a tmux pane id are both small integers and collide, so the host name
// leads. The tmux form keeps the "%" tmux itself puts in front of a pane id.
func (a Agent) Key() string {
	host := a.Host
	if host == "" {
		host = HostKitty
	}
	if host == HostTmux {
		return host + ":%" + strconv.Itoa(a.ID)
	}
	return host + ":" + strconv.Itoa(a.ID)
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

// Resolver answers "which repository is this agent in", and remembers recent
// answers. One resolver serves every host, so a reload that mixes kitty windows
// and tmux panes runs one git process per unique directory rather than one per
// host.
type Resolver struct {
	// lookup resolves one directory's git facts. A field, so tests can count
	// lookups without spawning git. nil means gitRepo.
	lookup func(ctx context.Context, cwd string) (found repo, answered bool)

	mu    sync.Mutex
	cache map[string]repoCacheEntry // cwd -> recently observed repo facts
	ttl   time.Duration
}

// NewResolver returns a resolver with an empty cache.
func NewResolver() *Resolver {
	return &Resolver{lookup: gitRepo, cache: map[string]repoCacheEntry{}, ttl: defaultRepoTTL}
}

// Populate resolves each unique cwd once, concurrently, then fills every agent
// that shares it. The worker limit keeps a large inventory from spawning an
// unbounded number of git processes.
func (r *Resolver) Populate(ctx context.Context, agents []Agent) {
	indices := make(map[string][]int)
	for i, a := range agents {
		if a.CWD != "" {
			indices[a.CWD] = append(indices[a.CWD], i)
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
			repos[i] = r.repoFor(ctx, cwd)
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
func (r *Resolver) repoFor(ctx context.Context, cwd string) repo {
	if cwd == "" {
		return repo{}
	}
	now := time.Now()
	r.mu.Lock()
	if cached, ok := r.cache[cwd]; ok && now.Before(cached.expires) {
		r.mu.Unlock()
		return cached.repo
	}
	r.mu.Unlock()

	lookup := r.lookup
	if lookup == nil {
		lookup = gitRepo
	}
	found, answered := lookup(ctx, cwd)

	if r.ttl > 0 && answered {
		r.mu.Lock()
		if r.cache == nil {
			r.cache = make(map[string]repoCacheEntry)
		}
		r.cache[cwd] = repoCacheEntry{repo: found, expires: now.Add(r.ttl)}
		r.mu.Unlock()
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

// Sort groups agents by project and orders each group oldest session first.
// Project order is alphabetical, so a group never moves under the user while
// agents change state; the status filter tabs cover triage. Agents whose
// project could not be determined go last.
func Sort(agents []Agent) {
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
		// A kitty window id and a tmux pane id can be the same number, so the
		// host breaks the last tie and keeps the order stable.
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Host < b.Host
	})
}
