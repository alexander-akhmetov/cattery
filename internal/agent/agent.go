// Package agent holds the half of cattery's inventory that no host owns: what
// an agent is, how the picker identifies one, and the git facts it groups by.
// kitty windows and tmux panes both become agents, and neither host package has
// to import the other to say so.
package agent

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
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

// Process is one process a host reports running in an agent's window. A kitty
// window has several: a sandboxed agent's first foreground process is the
// sandbox's log reader, and the agent itself is behind it, which is the same
// fact that makes kitty's --use-foreground-process useless for snapshots.
type Process struct {
	PID     int
	Cmdline []string
	CWD     string
}

// Agent is one running agent, wherever it lives: a kitty window carrying
// AGENT_DISPLAY, or a tmux pane carrying @AGENT_STATE.
//
// It carries a slice, so it is not comparable: compare two with
// reflect.DeepEqual rather than ==.
type Agent struct {
	ID      int
	Kind    string // AGENT_KIND, e.g. "pi" or "claude"
	Display string // blocked, stalled, done, working, idle
	Title   string
	CWD     string
	Since   time.Time // from AGENT_SINCE; zero when unknown
	Msg     string    // AGENT_MSG: the latest user prompt, when published

	// State is the word the agent itself published, before Display derived
	// anything from it. "done" and "stalled" are displays only, so a caller
	// deciding whether an agent can take input has to read this instead. Empty
	// on a window whose agent was killed: `cattery state clear` drops the
	// state, and nothing clears the watcher's own AGENT_DISPLAY.
	State string

	// Resume is AGENT_RESUME, the command that reopens this session.
	Resume string

	// Self marks the window or pane the current process runs in. Only
	// internal/agents may set it: kitty's own is_self is computed from the
	// caller's $KITTY_WINDOW_ID, which a tmux pane inherits from whatever
	// started its server.
	Self bool

	// PID is the window's own process: kitty's window pid, or #{pane_pid}.
	// Part of the fingerprint a caller re-checks before typing at an agent.
	PID int

	// Command is #{pane_current_command}, a command name and never argv. tmux
	// only; the weaker half of the fingerprint.
	Command string

	// Procs is kitty's foreground_processes, with argv. kitty only.
	Procs []Process

	// Tool is the tool call the agent is running now, "bash: go test ./...",
	// and ToolSince is when that call started. Both parsers drop them unless
	// PublishesTool(Kind, Display).
	Tool      string
	ToolSince time.Time

	// Host is where the agent runs: HostKitty or HostTmux.
	Host string

	// Target is the tmux "<session>:<window index>.<pane id>" to attach to.
	// Empty for a kitty agent, which is reached by window id instead.
	Target string

	// CreatedAt is when the host opened the window, a stable session age. Since
	// restamps on a display change, except between working and stalled, so it
	// times the current turn rather than the session. Zero when the host does
	// not report it.
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

// UnixSeconds reads an AGENT_* value holding a unix timestamp. An unset one is
// the empty string, and a zero is not a timestamp either: time.Unix(0, 0) is
// not IsZero, so it would read as 1970 and any "has this run too long" rule
// would fire on it at once.
func UnixSeconds(raw string) time.Time {
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

// The agent kinds that publish the running-tool pair. Claude and Codex have no
// per-tool-call hook, so neither reports one and neither can reach "stalled".
// Keep this in step with _TOOL_KINDS in kitty/cattery_watcher.py.
const (
	KindPi       = "pi"
	KindOpencode = "opencode"
)

var toolKinds = []string{KindPi, KindOpencode}

// PublishesTool reports whether the tool pair on a window or a pane belongs to
// the agent reading it now. Two things have to hold.
//
// The kind has to be one that publishes it. A window or a pane outlives its
// agents, and `cattery state clear` drops the state, the kind and the message
// and nothing else, so a pi killed mid-call leaves its label standing: without
// the kind test the Claude started in that window reads as stalled from its
// first second, with the dead pi's command on the row.
//
// And the agent has to be running. An idle or a finished agent's label is its
// own, but the call is over. "stalled" counts as running: the kitty watcher
// publishes that one itself, and dropping the label there would take the tool
// line off the row it exists for.
func PublishesTool(kind, display string) bool {
	return slices.Contains(toolKinds, kind) && (display == "working" || display == "stalled")
}

// StallThreshold is how long one tool call has to run before an agent is shown
// as stalled rather than working. Ten minutes, not five: pi's subagent calls
// routinely run several minutes, so a shorter threshold would flag ordinary
// work. Keep it in step with _STALL_THRESHOLD in kitty/cattery_watcher.py:
// TestStalled here and test_the_threshold_is_ten_minutes there both write the
// ages out, so moving one number alone fails one of the two.
const StallThreshold = 10 * time.Minute

// Stalled reports whether one tool call has run long enough to be worth
// doubting. No agent publishes it: nothing fires while a tool hangs, so a state
// the writer set would never arrive. The picker derives it on every reload, and
// the kitty watcher runs the same rule on a timer for the tab marker.
//
// An agent that publishes no tool never reaches it, which is what keeps Claude
// agents out with no special case.
func Stalled(a Agent, now time.Time) bool {
	if a.Display != "working" || a.Tool == "" || a.ToolSince.IsZero() {
		return false
	}
	return now.Sub(a.ToolSince) >= StallThreshold
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
