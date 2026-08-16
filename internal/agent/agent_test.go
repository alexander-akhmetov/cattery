package agent

import (
	"context"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// A kitty window id and a tmux pane id are both small integers, so the key has
// to say which host the number belongs to.
func TestKey(t *testing.T) {
	cases := []struct {
		name string
		in   Agent
		want string
	}{
		{name: "kitty window", in: Agent{ID: 12, Host: HostKitty}, want: "kitty:12"},
		{name: "tmux pane keeps tmux's own % form", in: Agent{ID: 17, Host: HostTmux}, want: "tmux:%17"},
		{name: "the same number in two hosts differs", in: Agent{ID: 17, Host: HostKitty}, want: "kitty:17"},
		{name: "an unset host reads as kitty", in: Agent{ID: 12}, want: "kitty:12"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Key(); got != tc.want {
				t.Fatalf("Key: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUnixSeconds(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want time.Time
	}{
		{name: "a timestamp", raw: "1700000000", want: time.Unix(1700000000, 0)},
		{name: "unset"},
		// time.Unix(0, 0) is not IsZero, so without the guard this reads as a
		// value fifty-odd years old and every threshold fires on it.
		{name: "zero", raw: "0"},
		{name: "negative", raw: "-1"},
		{name: "not a number", raw: "soon"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UnixSeconds(tc.raw); !got.Equal(tc.want) {
				t.Fatalf("UnixSeconds(%q): got %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

// A window or a pane outlives its agents, and `cattery state clear` drops the
// state, the kind and the message and nothing else. So a pi killed mid-call
// leaves its label standing, and the kind is what keeps the next agent in that
// window from wearing it.
func TestPublishesTool(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		display string
		want    bool
	}{
		{name: "pi working", kind: "pi", display: "working", want: true},
		{name: "pi stalled", kind: "pi", display: "stalled", want: true},
		{name: "pi idle", kind: "pi", display: "idle"},
		{name: "pi done", kind: "pi", display: "done"},
		{name: "pi blocked", kind: "pi", display: "blocked"},
		{name: "claude in a window a pi died in", kind: "claude", display: "working"},
		{name: "opencode working", kind: "opencode", display: "working", want: true},
		{name: "opencode stalled", kind: "opencode", display: "stalled", want: true},
		{name: "opencode idle", kind: "opencode", display: "idle"},
		// `cattery state clear` leaves AGENT_TOOL standing, so the dead agent's
		// label would sit on the next agent's row and read as stalled at once.
		{name: "claude in a window an opencode died in", kind: "claude", display: "working"},
		// Codex has no per-tool-call hook, so no tool column and no stalled.
		{name: "codex working", kind: "codex", display: "working"},
		{name: "an untagged agent", display: "working"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PublishesTool(tc.kind, tc.display); got != tc.want {
				t.Fatalf("PublishesTool(%q, %q): got %v, want %v", tc.kind, tc.display, got, tc.want)
			}
		})
	}
}

func TestStalled(t *testing.T) {
	now := time.Unix(1700000000, 0)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }

	cases := []struct {
		name string
		in   Agent
		want bool
	}{
		{
			name: "one tool past the threshold",
			in:   Agent{Display: "working", Tool: "bash: sleep 900", ToolSince: ago(11 * time.Minute)},
			want: true,
		},
		{
			name: "a tool still inside it",
			in:   Agent{Display: "working", Tool: "bash: go test ./...", ToolSince: ago(9 * time.Minute)},
		},
		// The zero value is older than every threshold, so a rule comparing
		// timestamps alone would call every working agent stalled.
		{name: "no timestamp", in: Agent{Display: "working", Tool: "bash: x"}},
		{name: "no tool at all", in: Agent{Display: "working", ToolSince: ago(time.Hour)}},
		{
			name: "a blocked agent is waiting, not stuck",
			in:   Agent{Display: "blocked", Tool: "bash: x", ToolSince: ago(time.Hour)},
		},
		{
			// Idempotent: the kitty watcher may have published it already.
			name: "an agent already stalled",
			in:   Agent{Display: "stalled", Tool: "bash: x", ToolSince: ago(time.Hour)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Stalled(tc.in, now); got != tc.want {
				t.Fatalf("Stalled: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSort(t *testing.T) {
	at := func(sec int64) time.Time { return time.Unix(sec, 0) }

	tests := []struct {
		name  string
		input []Agent
		want  []string
	}{
		{
			name: "projects are alphabetical regardless of status",
			input: []Agent{
				{ID: 1, Display: "idle", Project: "zulu", ProjectKey: "/z/.git"},
				{ID: 2, Display: "blocked", Project: "alpha", ProjectKey: "/a/.git"},
				{ID: 3, Display: "done", Project: "mike", ProjectKey: "/m/.git"},
			},
			want: []string{"kitty:2", "kitty:3", "kitty:1"},
		},
		{
			name: "oldest session first inside a project",
			input: []Agent{
				{ID: 1, Project: "a", ProjectKey: "/a/.git", CreatedAt: at(300)},
				{ID: 2, Project: "a", ProjectKey: "/a/.git", CreatedAt: at(100)},
				{ID: 3, Project: "a", ProjectKey: "/a/.git", CreatedAt: at(200)},
			},
			want: []string{"kitty:2", "kitty:3", "kitty:1"},
		},
		{
			name: "worktrees of one repo stay in the same group",
			input: []Agent{
				{ID: 1, Project: "repo", ProjectKey: "/p/repo/.git", Root: "/wt/b", CreatedAt: at(200)},
				{ID: 2, Project: "other", ProjectKey: "/p/other/.git", CreatedAt: at(50)},
				{ID: 3, Project: "repo", ProjectKey: "/p/repo/.git", Root: "/p/repo", CreatedAt: at(100)},
			},
			want: []string{"kitty:2", "kitty:3", "kitty:1"},
		},
		{
			name: "same label from different repos does not merge",
			input: []Agent{
				{ID: 1, Project: "myapp", ProjectKey: "/work/myapp/.git"},
				{ID: 2, Project: "myapp", ProjectKey: "/oss/myapp/.git"},
				{ID: 3, Project: "myapp", ProjectKey: "/work/myapp/.git"},
			},
			want: []string{"kitty:2", "kitty:1", "kitty:3"},
		},
		{
			name: "label order ignores case",
			input: []Agent{
				{ID: 1, Project: "beta", ProjectKey: "/b/.git"},
				{ID: 2, Project: "Alpha", ProjectKey: "/A/.git"},
			},
			want: []string{"kitty:2", "kitty:1"},
		},
		{
			name: "windows without a project go last",
			input: []Agent{
				{ID: 1},
				{ID: 2, Project: "zulu", ProjectKey: "/z/.git"},
			},
			want: []string{"kitty:2", "kitty:1"},
		},
		{
			name: "equal timestamps fall back to window id",
			input: []Agent{
				{ID: 9, Project: "a", ProjectKey: "/a/.git"},
				{ID: 4, Project: "a", ProjectKey: "/a/.git"},
			},
			want: []string{"kitty:4", "kitty:9"},
		},
		{
			// A kitty worktree and a tmux pane in the same repository sort
			// together, and one pane id equal to one window id keeps a stable
			// order instead of swapping on every reload.
			name: "hosts mix inside a project",
			input: []Agent{
				{ID: 7, Host: HostTmux, Project: "a", ProjectKey: "/a/.git"},
				{ID: 7, Host: HostKitty, Project: "a", ProjectKey: "/a/.git"},
			},
			want: []string{"kitty:7", "tmux:%7"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Sort(tt.input)
			var got []string
			for _, a := range tt.input {
				got = append(got, a.Key())
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("order: got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseRepo(t *testing.T) {
	tests := []struct {
		name string
		cwd  string
		out  string
		want repo
	}{
		{
			name: "normal checkout",
			cwd:  "/p/dotfiles",
			out:  "/p/dotfiles/.git\n/p/dotfiles\nmain\n",
			want: repo{project: "dotfiles", projectKey: "/p/dotfiles/.git", root: "/p/dotfiles", branch: "main"},
		},
		{
			name: "worktree resolves to its main repo",
			cwd:  "/wt/dotfiles/feat-oauth",
			out:  "/p/dotfiles/.git\n/wt/dotfiles/feat-oauth\nfeat/oauth\n",
			want: repo{project: "dotfiles", projectKey: "/p/dotfiles/.git", root: "/wt/dotfiles/feat-oauth", branch: "feat/oauth"},
		},
		{
			// The dev worktree a tmux agent runs in. It groups with the
			// repository's other agents and keeps the ticket branch.
			name: "a dev worktree groups with its main repo",
			cwd:  "/Users/x/.worktrees/myapp/feat-42",
			out:  "/Users/x/projects/myapp/.git\n/Users/x/.worktrees/myapp/feat-42\nwt/feat-42\n",
			want: repo{
				project:    "myapp",
				projectKey: "/Users/x/projects/myapp/.git",
				root:       "/Users/x/.worktrees/myapp/feat-42",
				branch:     "wt/feat-42",
			},
		},
		{
			name: "subdirectory resolves to the repo, not the subdirectory",
			cwd:  "/p/dotfiles/internal/kitty",
			out:  "/p/dotfiles/.git\n/p/dotfiles\nmain\n",
			want: repo{project: "dotfiles", projectKey: "/p/dotfiles/.git", root: "/p/dotfiles", branch: "main"},
		},
		{
			name: "detached HEAD keeps the project but has no branch",
			cwd:  "/tmp/notes-review",
			out:  "/p/notes/.git\n/tmp/notes-review\nHEAD\n",
			want: repo{project: "notes", projectKey: "/p/notes/.git", root: "/tmp/notes-review"},
		},
		{
			name: "bare repo drops the .git suffix from its label",
			cwd:  "/tmp/wt",
			out:  "/srv/dotfiles.git\n/tmp/wt\nwt\n",
			want: repo{project: "dotfiles", projectKey: "/srv/dotfiles.git", root: "/tmp/wt", branch: "wt"},
		},
		{
			// git prints the paths it resolved, then exits 128 on HEAD.
			name: "repository without commits still yields a project",
			cwd:  "/p/fresh",
			out:  "/p/fresh/.git\n/p/fresh\nHEAD\n",
			want: repo{project: "fresh", projectKey: "/p/fresh/.git", root: "/p/fresh"},
		},
		{
			name: "no git output falls back to the folder",
			cwd:  "/home/x/scratch",
			out:  "",
			want: repo{project: "scratch", projectKey: "/home/x/scratch"},
		},
		{
			name: "no cwd yields nothing to group by",
			cwd:  "",
			out:  "",
			want: repo{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseRepo(tt.cwd, []byte(tt.out)); got != tt.want {
				t.Errorf("parseRepo(%q):\n got %+v\nwant %+v", tt.cwd, got, tt.want)
			}
		})
	}
}

// countingLookup replaces the git call with a stub that records how often each
// directory was resolved. A lookup per agent instead of per unique cwd then
// shows without spawning anything.
func countingLookup(r *Resolver) func() map[string]int {
	var mu sync.Mutex
	calls := map[string]int{}
	r.lookup = func(_ context.Context, cwd string) (repo, bool) {
		mu.Lock()
		calls[cwd]++
		mu.Unlock()
		return repo{project: "repo", projectKey: "/repo/.git", root: "/repo", branch: "main"}, true
	}
	return func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		return maps.Clone(calls)
	}
}

func TestPopulate(t *testing.T) {
	t.Run("one lookup per unique cwd, fanned out to every agent", func(t *testing.T) {
		resolver := newTestResolver()
		calls := countingLookup(resolver)
		agents := []Agent{
			{ID: 1, CWD: "/repo"},
			{ID: 2, CWD: "/repo/sub"},
			{ID: 3, CWD: "/repo"},
			{ID: 4, CWD: ""}, // no cwd, no lookup
			{ID: 5, CWD: "/repo"},
		}
		resolver.Populate(context.Background(), agents)

		want := map[string]int{"/repo": 1, "/repo/sub": 1}
		if got := calls(); !maps.Equal(got, want) {
			t.Errorf("lookups: got %v, want %v", got, want)
		}
		for _, a := range agents {
			wantBranch := "main"
			if a.CWD == "" {
				wantBranch = ""
			}
			if a.Branch != wantBranch {
				t.Errorf("agent %d branch: got %q, want %q", a.ID, a.Branch, wantBranch)
			}
		}
	})

	// Both hosts share one resolver, so a kitty window and a tmux pane in the
	// same directory cost one git process between them.
	t.Run("agents from two hosts share a lookup", func(t *testing.T) {
		resolver := newTestResolver()
		calls := countingLookup(resolver)
		agents := []Agent{
			{ID: 1, Host: HostKitty, CWD: "/repo"},
			{ID: 1, Host: HostTmux, CWD: "/repo"},
		}
		resolver.Populate(context.Background(), agents)

		if got := calls(); !maps.Equal(got, map[string]int{"/repo": 1}) {
			t.Errorf("lookups: got %v, want one for /repo", got)
		}
		if agents[0].ProjectKey != agents[1].ProjectKey {
			t.Errorf("project keys differ across hosts: %q vs %q", agents[0].ProjectKey, agents[1].ProjectKey)
		}
	})

	t.Run("distinct cwds each get their own facts", func(t *testing.T) {
		dir := initRepo(t)
		other := initRepo(t)
		runGit(t, other, "checkout", "-b", "feature")

		resolver := newTestResolver()
		agents := []Agent{
			{ID: 1, CWD: dir},
			{ID: 2, CWD: other},
			{ID: 3, CWD: dir},
			{ID: 4, CWD: ""}, // no cwd, must stay empty
			{ID: 5, CWD: dir},
		}
		resolver.Populate(context.Background(), agents)

		wantBranch := []string{"main", "feature", "main", "", "main"}
		wantProject := []string{
			filepath.Base(dir), filepath.Base(other), filepath.Base(dir), "", filepath.Base(dir),
		}
		for i := range agents {
			if agents[i].Branch != wantBranch[i] {
				t.Errorf("agent %d branch: got %q, want %q", agents[i].ID, agents[i].Branch, wantBranch[i])
			}
			if agents[i].Project != wantProject[i] {
				t.Errorf("agent %d project: got %q, want %q", agents[i].ID, agents[i].Project, wantProject[i])
			}
		}
		// Two distinct cwds, so two cache entries despite five agents.
		if len(resolver.cache) != 2 {
			t.Errorf("cache entries: got %d, want 2", len(resolver.cache))
		}
	})

	// A lookup the caller cut short says nothing about the directory. Caching it
	// would hold the folder fallback for the whole TTL and block the retry.
	t.Run("cancelled lookup is not cached", func(t *testing.T) {
		dir := initRepo(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		resolver := newTestResolver()
		resolver.Populate(ctx, []Agent{{ID: 1, CWD: dir}})
		if len(resolver.cache) != 0 {
			t.Fatalf("cache entries after a cancelled lookup: got %d, want 0", len(resolver.cache))
		}

		agents := []Agent{{ID: 1, CWD: dir}}
		resolver.Populate(context.Background(), agents)
		if agents[0].Branch != "main" {
			t.Errorf("branch after the retry: got %q, want main", agents[0].Branch)
		}
	})

	// Worktrees are why grouping keys off the common dir instead of the working
	// directory: three paths, one project.
	t.Run("worktrees share their main repo's project", func(t *testing.T) {
		main := initRepo(t)
		wt := filepath.Join(t.TempDir(), "feature-wt")
		runGit(t, main, "worktree", "add", "-b", "feature", wt)

		resolver := newTestResolver()
		agents := []Agent{{ID: 1, CWD: main}, {ID: 2, CWD: wt}}
		resolver.Populate(context.Background(), agents)

		if agents[0].ProjectKey != agents[1].ProjectKey {
			t.Errorf("worktree project keys differ: %q vs %q", agents[0].ProjectKey, agents[1].ProjectKey)
		}
		if agents[0].Project != filepath.Base(main) || agents[1].Project != filepath.Base(main) {
			t.Errorf("projects: got %q and %q, want %q", agents[0].Project, agents[1].Project, filepath.Base(main))
		}
		if agents[0].Branch != "main" || agents[1].Branch != "feature" {
			t.Errorf("branches: got %q and %q, want main and feature", agents[0].Branch, agents[1].Branch)
		}
		if filepath.Base(agents[1].Root) != "feature-wt" {
			t.Errorf("worktree root: got %q, want .../feature-wt", agents[1].Root)
		}
	})

	// The picker still has to draw a row, so a cancelled lookup groups by folder
	// instead of dropping the agent into "unknown".
	t.Run("cancelled context falls back to the folder", func(t *testing.T) {
		dir := initRepo(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		resolver := newTestResolver()
		agents := []Agent{{ID: 1, CWD: dir}}
		resolver.Populate(ctx, agents)

		if agents[0].Branch != "" {
			t.Errorf("cancelled lookup branch: got %q, want empty", agents[0].Branch)
		}
		if agents[0].Project != filepath.Base(dir) || agents[0].ProjectKey != dir {
			t.Errorf("cancelled lookup project: got %q/%q, want %q/%q",
				agents[0].Project, agents[0].ProjectKey, filepath.Base(dir), dir)
		}
	})

	t.Run("no cwd anywhere is a no-op", func(t *testing.T) {
		resolver := newTestResolver()
		agents := []Agent{{ID: 1}, {ID: 2}}
		resolver.Populate(context.Background(), agents)
		if len(resolver.cache) != 0 {
			t.Errorf("cache entries: got %d, want 0", len(resolver.cache))
		}
	})
}

func TestRepoFor(t *testing.T) {
	t.Run("fresh entry is served from cache", func(t *testing.T) {
		// A non-repo path. Any value returned came from the cache.
		dir := t.TempDir()
		resolver := newTestResolver()
		resolver.cache[dir] = repoCacheEntry{
			repo:    repo{project: "cached", projectKey: "/cached/.git", branch: "cached"},
			expires: time.Now().Add(time.Hour),
		}
		if got := resolver.repoFor(context.Background(), dir); got.branch != "cached" || got.project != "cached" {
			t.Fatalf("repo: got %+v, want the cached entry", got)
		}
	})

	t.Run("detached HEAD keeps the project but has no branch", func(t *testing.T) {
		dir := initRepo(t)
		runGit(t, dir, "checkout", "--detach")

		resolver := newTestResolver()
		got := resolver.repoFor(context.Background(), dir)
		if got.branch != "" {
			t.Fatalf("detached branch: got %q, want empty", got.branch)
		}
		if got.project != filepath.Base(dir) {
			t.Errorf("detached project: got %q, want %q", got.project, filepath.Base(dir))
		}
		// Detached HEAD is a successful lookup, and is cached as it stands.
		if entry, ok := resolver.cache[dir]; !ok || entry.repo.branch != "" {
			t.Errorf("detached HEAD cache entry: %+v present=%v", entry, ok)
		}
	})

	t.Run("empty cwd is skipped", func(t *testing.T) {
		resolver := newTestResolver()
		if got := resolver.repoFor(context.Background(), ""); got != (repo{}) {
			t.Fatalf("empty cwd: got %+v, want zero", got)
		}
	})
}

func TestRepoCache(t *testing.T) {
	t.Run("expired entry refreshes", func(t *testing.T) {
		dir := initRepo(t)
		resolver := newTestResolver()
		if got := resolver.repoFor(context.Background(), dir); got.branch != "main" {
			t.Fatalf("initial branch: got %q, want main", got.branch)
		}
		runGit(t, dir, "checkout", "-b", "feature")

		expireRepo(t, resolver, dir)
		if got := resolver.repoFor(context.Background(), dir); got.branch != "feature" {
			t.Fatalf("refreshed branch: got %q, want feature", got.branch)
		}
	})

	// A directory outside git resolves to the folder fallback on every reload.
	// Caching that answer is what stops the picker starting a git process per
	// second for as long as it stays open.
	t.Run("failure is cached until the ttl expires", func(t *testing.T) {
		dir := t.TempDir()
		resolver := newTestResolver()
		if got := resolver.repoFor(context.Background(), dir); got.branch != "" || got.projectKey != dir {
			t.Fatalf("non-repo lookup: got %+v, want the folder fallback", got)
		}
		if _, ok := resolver.cache[dir]; !ok {
			t.Fatal("failed lookup should be cached")
		}

		initRepoAt(t, dir)
		if got := resolver.repoFor(context.Background(), dir); got.branch != "" {
			t.Fatalf("branch before expiry: got %q, want the cached empty value", got.branch)
		}
		expireRepo(t, resolver, dir)
		if got := resolver.repoFor(context.Background(), dir); got.branch != "main" {
			t.Fatalf("retried branch: got %q, want main", got.branch)
		}
	})
}

func newTestResolver() *Resolver {
	return &Resolver{cache: map[string]repoCacheEntry{}, ttl: time.Hour}
}

// expireRepo ages out a cache entry so the next lookup re-runs git.
func expireRepo(t *testing.T, r *Resolver, cwd string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.cache[cwd]
	if !ok {
		t.Fatalf("no cache entry for %q", cwd)
	}
	entry.expires = time.Now().Add(-time.Second)
	r.cache[cwd] = entry
}

func initRepo(t *testing.T) string {
	t.Helper()
	// t.TempDir() is a symlinked /var path on macOS; git reports the resolved
	// one, so resolve up front or every path comparison here fails.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	initRepoAt(t, dir)
	return dir
}

func initRepoAt(t *testing.T, repo string) {
	t.Helper()
	runGit(t, repo, "init", "-b", "main")
	runGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "init")
}

func runGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", repo}, args...)
	if out, err := exec.Command("git", cmdArgs...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
