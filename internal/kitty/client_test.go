package kitty

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const sampleLs = `[
  {
    "tabs": [
      {
        "windows": [
          {
            "id": 119,
            "title": "pi - dotfiles",
            "cwd": "/Users/x/projects/dotfiles",
            "created_at": 1700000000000000000,
            "user_vars": {"AGENT_DISPLAY": "working", "AGENT_KIND": "pi", "AGENT_STATE": "working", "AGENT_SINCE": "1700000000"}
          },
          {
            "id": 200,
            "title": "plain shell",
            "cwd": "/Users/x",
            "user_vars": {}
          }
        ]
      },
      {
        "windows": [
          {
            "id": 114,
            "title": "Check bot",
            "cwd": "/Users/x/projects/astra-l",
            "user_vars": {"AGENT_DISPLAY": "blocked", "AGENT_KIND": "claude"}
          }
        ]
      }
    ]
  },
  {
    "tabs": [
      {
        "windows": [
          {
            "id": 118,
            "title": "pi - work",
            "cwd": "/Users/x/work",
            "user_vars": {"AGENT_DISPLAY": "idle", "AGENT_KIND": "pi"}
          }
        ]
      }
    ]
  }
]`

func TestParseAgents(t *testing.T) {
	agents, err := parseAgents([]byte(sampleLs))
	if err != nil {
		t.Fatalf("parseAgents: %v", err)
	}

	// Only the three windows with AGENT_DISPLAY are kept; the plain shell drops.
	if len(agents) != 3 {
		t.Fatalf("got %d agents, want 3", len(agents))
	}

	// parseAgents does not sort: ordering needs the repo lookup ListAgents runs
	// afterwards, so windows come back in kitty's own order.
	wantOrder := []struct {
		id      int
		display string
	}{
		{119, "working"},
		{114, "blocked"},
		{118, "idle"},
	}
	for i, w := range wantOrder {
		if agents[i].ID != w.id || agents[i].Display != w.display {
			t.Errorf("position %d: got id=%d display=%s, want id=%d display=%s",
				i, agents[i].ID, agents[i].Display, w.id, w.display)
		}
	}

	if agents[0].Kind != "pi" {
		t.Errorf("working agent kind: got %q, want pi", agents[0].Kind)
	}
	if got := agents[0].Since; !got.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("AGENT_SINCE parse: got %v, want %v", got, time.Unix(1700000000, 0))
	}
	if got := agents[0].CreatedAt; !got.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("created_at parse: got %v, want %v", got, time.Unix(1700000000, 0))
	}
	if !agents[1].Since.IsZero() {
		t.Errorf("missing AGENT_SINCE should be zero time, got %v", agents[1].Since)
	}
	if !agents[1].CreatedAt.IsZero() {
		t.Errorf("missing created_at should be zero time, got %v", agents[1].CreatedAt)
	}
}

func TestParseAgentsEmpty(t *testing.T) {
	agents, err := parseAgents([]byte(`[]`))
	if err != nil {
		t.Fatalf("parseAgents: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("got %d agents, want 0", len(agents))
	}
}

func TestParseAgentsInvalid(t *testing.T) {
	if _, err := parseAgents([]byte(`not json`)); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestSortAgents(t *testing.T) {
	at := func(sec int64) time.Time { return time.Unix(sec, 0) }

	tests := []struct {
		name  string
		input []Agent
		want  []int
	}{
		{
			name: "projects are alphabetical regardless of status",
			input: []Agent{
				{ID: 1, Display: "idle", Project: "zulu", ProjectKey: "/z/.git"},
				{ID: 2, Display: "blocked", Project: "alpha", ProjectKey: "/a/.git"},
				{ID: 3, Display: "done", Project: "mike", ProjectKey: "/m/.git"},
			},
			want: []int{2, 3, 1},
		},
		{
			name: "oldest session first inside a project",
			input: []Agent{
				{ID: 1, Project: "a", ProjectKey: "/a/.git", CreatedAt: at(300)},
				{ID: 2, Project: "a", ProjectKey: "/a/.git", CreatedAt: at(100)},
				{ID: 3, Project: "a", ProjectKey: "/a/.git", CreatedAt: at(200)},
			},
			want: []int{2, 3, 1},
		},
		{
			name: "worktrees of one repo stay in the same group",
			input: []Agent{
				{ID: 1, Project: "repo", ProjectKey: "/p/repo/.git", Root: "/wt/b", CreatedAt: at(200)},
				{ID: 2, Project: "other", ProjectKey: "/p/other/.git", CreatedAt: at(50)},
				{ID: 3, Project: "repo", ProjectKey: "/p/repo/.git", Root: "/p/repo", CreatedAt: at(100)},
			},
			want: []int{2, 3, 1},
		},
		{
			name: "same label from different repos does not merge",
			input: []Agent{
				{ID: 1, Project: "grafana", ProjectKey: "/work/grafana/.git"},
				{ID: 2, Project: "grafana", ProjectKey: "/oss/grafana/.git"},
				{ID: 3, Project: "grafana", ProjectKey: "/work/grafana/.git"},
			},
			want: []int{2, 1, 3},
		},
		{
			name: "label order ignores case",
			input: []Agent{
				{ID: 1, Project: "beta", ProjectKey: "/b/.git"},
				{ID: 2, Project: "Alpha", ProjectKey: "/A/.git"},
			},
			want: []int{2, 1},
		},
		{
			name: "windows without a project go last",
			input: []Agent{
				{ID: 1},
				{ID: 2, Project: "zulu", ProjectKey: "/z/.git"},
			},
			want: []int{2, 1},
		},
		{
			name: "equal timestamps fall back to window id",
			input: []Agent{
				{ID: 9, Project: "a", ProjectKey: "/a/.git"},
				{ID: 4, Project: "a", ProjectKey: "/a/.git"},
			},
			want: []int{4, 9},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sortAgents(tt.input)
			var got []int
			for _, a := range tt.input {
				got = append(got, a.ID)
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
			name: "subdirectory resolves to the repo, not the subdirectory",
			cwd:  "/p/dotfiles/internal/kitty",
			out:  "/p/dotfiles/.git\n/p/dotfiles\nmain\n",
			want: repo{project: "dotfiles", projectKey: "/p/dotfiles/.git", root: "/p/dotfiles", branch: "main"},
		},
		{
			name: "detached HEAD keeps the project but has no branch",
			cwd:  "/tmp/sig-review",
			out:  "/p/sigil/.git\n/tmp/sig-review\nHEAD\n",
			want: repo{project: "sigil", projectKey: "/p/sigil/.git", root: "/tmp/sig-review"},
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

func TestFocusCommand(t *testing.T) {
	cmd := focusCommand(context.Background(), "kitten", 42)
	want := []string{"kitten", "@", "focus-window", "--match", "id:42"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("focus args: got %v, want %v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("focus arg %d: got %q, want %q", i, cmd.Args[i], want[i])
		}
	}
}

func TestSetUserVarsCommand(t *testing.T) {
	cmd := setUserVarsCommand(context.Background(), "kitten", 42, []string{"AGENT_KIND=claude", "AGENT_STATE=working", "AGENT_MSG"})
	want := []string{
		"kitten", "@", "set-user-vars", "--match", "id:42",
		"AGENT_KIND=claude", "AGENT_STATE=working", "AGENT_MSG",
	}
	if len(cmd.Args) != len(want) {
		t.Fatalf("set-user-vars args: got %v, want %v", cmd.Args, want)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("set-user-vars arg %d: got %q, want %q", i, cmd.Args[i], want[i])
		}
	}
}

// The state writer calls this from a Claude hook, where a failure is swallowed
// and the tab glyph simply stops updating, so the reason has to survive the
// call rather than come back as a bare exit status.
func TestSetUserVars(t *testing.T) {
	vars := []string{"AGENT_KIND=claude", "AGENT_STATE=working"}

	t.Run("nothing to publish runs no command", func(t *testing.T) {
		client := &Client{kitten: filepath.Join(t.TempDir(), "absent-kitten")}
		if err := client.SetUserVars(context.Background(), 42, nil); err != nil {
			t.Fatalf("set-user-vars: got %v, want nil", err)
		}
	})

	t.Run("success returns nil", func(t *testing.T) {
		client := &Client{kitten: fakeKitten(t, "exit 0")}
		if err := client.SetUserVars(context.Background(), 42, vars); err != nil {
			t.Fatalf("set-user-vars: got %v, want nil", err)
		}
	})

	cases := []struct {
		name   string
		kitten string
		want   []string
	}{
		{
			name:   "reports kitty's own reason",
			kitten: fakeKitten(t, "printf 'no listening socket\\nfor id:42\\n' >&2; exit 1"),
			want:   []string{"window 42", "no listening socket for id:42"},
		},
		{
			name:   "falls back to the exit status when output is silent",
			kitten: fakeKitten(t, "exit 3"),
			want:   []string{"window 42", "exit status 3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{kitten: tc.kitten}
			err := client.SetUserVars(context.Background(), 42, vars)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error missing %q, got %q", w, err.Error())
				}
			}
		})
	}
}

func TestCondense(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{name: "single line", in: "no matching window", want: "no matching window"},
		{name: "trailing newline", in: "no matching window\n", want: "no matching window"},
		{
			name: "python traceback",
			in:   "Traceback (most recent call last):\n  File \"kitty\", line 1\nError: no matching window for id:42\n",
			want: `Traceback (most recent call last): File "kitty", line 1 Error: no matching window for id:42`,
		},
		{name: "tabs and runs", in: "a\t\tb   c", want: "a b c"},
		{name: "whitespace only", in: " \n\t ", want: ""},
		{name: "empty", in: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := condense(tc.in); got != tc.want {
				t.Fatalf("condense: got %q, want %q", got, tc.want)
			}
		})
	}
}

// A failed jump must name the window it targeted and stay on one line, so the
// overlay banner can show something more useful than "exit status 1".
func TestFocusWindowError(t *testing.T) {
	cases := []struct {
		name   string
		kitten string
		want   []string
	}{
		{
			name:   "reports command output",
			kitten: fakeKitten(t, "printf 'no matching window\\nfor id:42\\n' >&2; exit 1"),
			want:   []string{"window 42", "no matching window for id:42"},
		},
		{
			name:   "falls back to the exit status when output is silent",
			kitten: fakeKitten(t, "exit 3"),
			want:   []string{"window 42", "exit status 3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{kitten: tc.kitten}
			err := client.FocusWindow(context.Background(), 42)
			if err == nil {
				t.Fatal("expected a focus error")
			}
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("focus error spans lines: %q", err.Error())
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("focus error missing %q, got %q", w, err.Error())
				}
			}
		})
	}

	t.Run("success returns nil", func(t *testing.T) {
		client := &Client{kitten: fakeKitten(t, "exit 0")}
		if err := client.FocusWindow(context.Background(), 42); err != nil {
			t.Fatalf("focus: got %v, want nil", err)
		}
	})
}

// The reload banner shows this error unchanged, so a failed inventory read must
// carry kitty's own reason, not a bare exit status.
func TestListAgentsError(t *testing.T) {
	cases := []struct {
		name   string
		kitten string
		want   string
	}{
		{
			name:   "reports stderr",
			kitten: fakeKitten(t, "printf 'no listening socket\\nfor id:1\\n' >&2; exit 1"),
			want:   "no listening socket for id:1",
		},
		{
			name:   "falls back to the exit status when output is silent",
			kitten: fakeKitten(t, "exit 3"),
			want:   "exit status 3",
		},
		{
			name:   "missing kitten binary",
			kitten: filepath.Join(t.TempDir(), "absent-kitten"),
			want:   "absent-kitten",
		},
		{
			name:   "unparseable inventory",
			kitten: fakeKitten(t, "printf 'not json'"),
			want:   "invalid character",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := newTestClient()
			client.kitten = tc.kitten
			_, err := client.ListAgents(context.Background())
			if err == nil {
				t.Fatal("expected a list error")
			}
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("list error spans lines: %q", err.Error())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("list error missing %q, got %q", tc.want, err.Error())
			}
		})
	}
}

// fakeKitten writes a stub kitten script running body, and returns its path.
func fakeKitten(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kitten")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// countingLookup replaces the git call with a stub that records how often each
// directory was resolved, so a lookup per agent instead of per unique cwd is
// visible without spawning anything.
func countingLookup(client *Client) func() map[string]int {
	var mu sync.Mutex
	calls := map[string]int{}
	client.lookup = func(_ context.Context, cwd string) (repo, bool) {
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

func TestPopulateRepos(t *testing.T) {
	t.Run("one lookup per unique cwd, fanned out to every agent", func(t *testing.T) {
		client := newTestClient()
		calls := countingLookup(client)
		agents := []Agent{
			{ID: 1, CWD: "/repo"},
			{ID: 2, CWD: "/repo/sub"},
			{ID: 3, CWD: "/repo"},
			{ID: 4, CWD: ""}, // no cwd, no lookup
			{ID: 5, CWD: "/repo"},
		}
		client.populateRepos(context.Background(), agents)

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

	t.Run("distinct cwds each get their own facts", func(t *testing.T) {
		dir := initRepo(t)
		other := initRepo(t)
		runGit(t, other, "checkout", "-b", "feature")

		client := newTestClient()
		agents := []Agent{
			{ID: 1, CWD: dir},
			{ID: 2, CWD: other},
			{ID: 3, CWD: dir},
			{ID: 4, CWD: ""}, // no cwd, must stay empty
			{ID: 5, CWD: dir},
		}
		client.populateRepos(context.Background(), agents)

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
		if len(client.repo) != 2 {
			t.Errorf("cache entries: got %d, want 2", len(client.repo))
		}
	})

	// A lookup the caller cut short says nothing about the directory, so caching
	// it would hold the folder fallback for the whole TTL and block the retry.
	t.Run("cancelled lookup is not cached", func(t *testing.T) {
		dir := initRepo(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		client := newTestClient()
		client.populateRepos(ctx, []Agent{{ID: 1, CWD: dir}})
		if len(client.repo) != 0 {
			t.Fatalf("cache entries after a cancelled lookup: got %d, want 0", len(client.repo))
		}

		agents := []Agent{{ID: 1, CWD: dir}}
		client.populateRepos(context.Background(), agents)
		if agents[0].Branch != "main" {
			t.Errorf("branch after the retry: got %q, want main", agents[0].Branch)
		}
	})

	// Worktrees are the reason grouping keys off the common dir instead of the
	// working directory: three paths, one project.
	t.Run("worktrees share their main repo's project", func(t *testing.T) {
		main := initRepo(t)
		wt := filepath.Join(t.TempDir(), "feature-wt")
		runGit(t, main, "worktree", "add", "-b", "feature", wt)

		client := newTestClient()
		agents := []Agent{{ID: 1, CWD: main}, {ID: 2, CWD: wt}}
		client.populateRepos(context.Background(), agents)

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

	// The picker still has to draw a row, so a cancelled lookup falls back to
	// grouping by folder instead of dropping the agent into "unknown".
	t.Run("cancelled context falls back to the folder", func(t *testing.T) {
		dir := initRepo(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		client := newTestClient()
		agents := []Agent{{ID: 1, CWD: dir}}
		client.populateRepos(ctx, agents)

		if agents[0].Branch != "" {
			t.Errorf("cancelled lookup branch: got %q, want empty", agents[0].Branch)
		}
		if agents[0].Project != filepath.Base(dir) || agents[0].ProjectKey != dir {
			t.Errorf("cancelled lookup project: got %q/%q, want %q/%q",
				agents[0].Project, agents[0].ProjectKey, filepath.Base(dir), dir)
		}
	})

	t.Run("no cwd anywhere is a no-op", func(t *testing.T) {
		client := newTestClient()
		agents := []Agent{{ID: 1}, {ID: 2}}
		client.populateRepos(context.Background(), agents)
		if len(client.repo) != 0 {
			t.Errorf("cache entries: got %d, want 0", len(client.repo))
		}
	})
}

func TestRepoFor(t *testing.T) {
	t.Run("fresh entry is served from cache", func(t *testing.T) {
		// A non-repo path: any value returned must have come from the cache.
		dir := t.TempDir()
		client := newTestClient()
		client.repo[dir] = repoCacheEntry{
			repo:    repo{project: "cached", projectKey: "/cached/.git", branch: "cached"},
			expires: time.Now().Add(time.Hour),
		}
		if got := client.repoFor(context.Background(), dir); got.branch != "cached" || got.project != "cached" {
			t.Fatalf("repo: got %+v, want the cached entry", got)
		}
	})

	t.Run("detached HEAD keeps the project but has no branch", func(t *testing.T) {
		dir := initRepo(t)
		runGit(t, dir, "checkout", "--detach")

		client := newTestClient()
		got := client.repoFor(context.Background(), dir)
		if got.branch != "" {
			t.Fatalf("detached branch: got %q, want empty", got.branch)
		}
		if got.project != filepath.Base(dir) {
			t.Errorf("detached project: got %q, want %q", got.project, filepath.Base(dir))
		}
		// Detached HEAD is a successful lookup, so it is cached as it stands.
		if entry, ok := client.repo[dir]; !ok || entry.repo.branch != "" {
			t.Errorf("detached HEAD cache entry: %+v present=%v", entry, ok)
		}
	})

	t.Run("empty cwd is skipped", func(t *testing.T) {
		client := newTestClient()
		if got := client.repoFor(context.Background(), ""); got != (repo{}) {
			t.Fatalf("empty cwd: got %+v, want zero", got)
		}
	})
}

func TestRepoCache(t *testing.T) {
	t.Run("expired entry refreshes", func(t *testing.T) {
		dir := initRepo(t)
		client := newTestClient()
		if got := client.repoFor(context.Background(), dir); got.branch != "main" {
			t.Fatalf("initial branch: got %q, want main", got.branch)
		}
		runGit(t, dir, "checkout", "-b", "feature")

		expireRepo(t, client, dir)
		if got := client.repoFor(context.Background(), dir); got.branch != "feature" {
			t.Fatalf("refreshed branch: got %q, want feature", got.branch)
		}
	})

	// A directory outside git resolves to the folder fallback on every reload.
	// Caching that answer is what stops the picker starting a git process per
	// second for as long as it stays open.
	t.Run("failure is cached until the ttl expires", func(t *testing.T) {
		dir := t.TempDir()
		client := newTestClient()
		if got := client.repoFor(context.Background(), dir); got.branch != "" || got.projectKey != dir {
			t.Fatalf("non-repo lookup: got %+v, want the folder fallback", got)
		}
		if _, ok := client.repo[dir]; !ok {
			t.Fatal("failed lookup should be cached")
		}

		initRepoAt(t, dir)
		if got := client.repoFor(context.Background(), dir); got.branch != "" {
			t.Fatalf("branch before expiry: got %q, want the cached empty value", got.branch)
		}
		expireRepo(t, client, dir)
		if got := client.repoFor(context.Background(), dir); got.branch != "main" {
			t.Fatalf("retried branch: got %q, want main", got.branch)
		}
	})
}

func newTestClient() *Client {
	return &Client{repo: map[string]repoCacheEntry{}, repoTTL: time.Hour}
}

// expireRepo ages out a cache entry so the next lookup re-runs git.
func expireRepo(t *testing.T, client *Client, cwd string) {
	t.Helper()
	client.mu.Lock()
	defer client.mu.Unlock()
	entry, ok := client.repo[cwd]
	if !ok {
		t.Fatalf("no cache entry for %q", cwd)
	}
	entry.expires = time.Now().Add(-time.Second)
	client.repo[cwd] = entry
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
