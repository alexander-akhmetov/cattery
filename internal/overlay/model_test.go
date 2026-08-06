package overlay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/alexander-akhmetov/cattery"
	"github.com/alexander-akhmetov/cattery/internal/kitty"
)

type fakeClient struct {
	agents   []kitty.Agent
	focused  []int
	focusErr error
}

func (f *fakeClient) ListAgents(context.Context) ([]kitty.Agent, error) { return f.agents, nil }

func (f *fakeClient) FocusWindow(_ context.Context, id int) error {
	f.focused = append(f.focused, id)
	return f.focusErr
}

// checkout builds an agent in the primary checkout of its own project, the
// shape ListAgents produces for a plain `cd ~/projects/x` session.
func checkout(id int, kind, display, project, branch string) kitty.Agent {
	root := "/p/" + project
	return kitty.Agent{
		ID: id, Kind: kind, Display: display,
		CWD: root, Project: project, ProjectKey: root + "/.git", Root: root, Branch: branch,
	}
}

func sampleModel() Model {
	m := New(&fakeClient{})
	m.loaded = true
	m.loading = false
	// Project order, the way ListAgents delivers them.
	m.agents = []kitty.Agent{
		checkout(3, "pi", "working", "astra-l", "feat/oauth"),
		checkout(2, "pi", "working", "dotfiles", "main"),
		checkout(1, "claude", "blocked", "llm-proxy", "master"),
		checkout(4, "codex", "done", "qmp-relay", "qmp"),
	}
	return m
}

// The status filter and the query both narrow the same list, and the query
// searches inside the filter, not beside it.
func TestVisible(t *testing.T) {
	cases := []struct {
		name   string
		filter string
		query  string
		want   []int
	}{
		{"no filter, no query", "all", "", []int{3, 2, 1, 4}},
		{"filter keeps only its status", "working", "", []int{3, 2}},
		{"query matches a branch", "all", "oauth", []int{3}},
		{"query matches a kind", "all", "pi", []int{3, 2}},
		{"query matches nothing", "all", "nomatch", nil},
		{"query searches inside the filter", "working", "master", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.filter = tc.filter
			m.search.SetValue(tc.query)
			vis := m.visible()
			got := make([]int, 0, len(vis))
			for _, a := range vis {
				got = append(got, a.ID)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("visible ids: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMoveWraps(t *testing.T) {
	m := sampleModel()
	m.selected = 0
	m.move(-1)
	if m.selected != 3 {
		t.Errorf("move up from top: got %d, want 3", m.selected)
	}
	m.move(1)
	if m.selected != 0 {
		t.Errorf("move down wraps to top: got %d, want 0", m.selected)
	}
}

func TestClampSelectionAfterFilter(t *testing.T) {
	m := sampleModel()
	m.selected = 3
	m.filter = "blocked" // only one match now
	m.clampSelection()
	if m.selected != 0 {
		t.Errorf("clamp after shrink: got %d, want 0", m.selected)
	}
}

func TestCycleFilterResetsSelection(t *testing.T) {
	m := sampleModel()
	m.selected = 2
	m.cycleFilter()
	if m.filter != "working" {
		t.Errorf("cycle from all: got %q, want working", m.filter)
	}
	if m.selected != 0 {
		t.Errorf("cycle should reset selection, got %d", m.selected)
	}
	m.cycleFilter()
	m.cycleFilter()
	if m.filter != "done" {
		t.Errorf("third cycle: got %q, want done", m.filter)
	}
	m.cycleFilter()
	if m.filter != "idle" {
		t.Errorf("fourth cycle: got %q, want idle", m.filter)
	}
	m.cycleFilter()
	if m.filter != "all" {
		t.Errorf("cycle wraps to all, got %q", m.filter)
	}
}

func TestNumberKeySelectsVisibleRow(t *testing.T) {
	m := sampleModel()
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	if updated.(Model).selected != 2 {
		t.Errorf("number key 3: got %d, want 2", updated.(Model).selected)
	}
}

func TestFocusSelectedTargetsVisibleRow(t *testing.T) {
	m := sampleModel()
	m.filter = "working"
	m.selected = 1 // second working agent => id 2

	// The selected row in the filtered view must map back to the right window.
	if got := m.visible()[m.selected].ID; got != 2 {
		t.Fatalf("selected visible id: got %d, want 2", got)
	}

	model, cmd := m.focusSelected()
	if !model.focusing || model.quitting {
		t.Error("focusSelected should wait for the focus result before quitting")
	}
	if cmd == nil {
		t.Fatal("focusSelected should return a command")
	}
}

func TestFocusCmdCallsClient(t *testing.T) {
	fc := &fakeClient{}
	msg := focusCmd(context.Background(), fc, 3)()
	if got := msg.(focusMsg).err; got != nil {
		t.Errorf("focusCmd error: got %v, want nil", got)
	}
	if len(fc.focused) != 1 || fc.focused[0] != 3 {
		t.Errorf("focused windows: got %v, want [3]", fc.focused)
	}
}

func TestFocusSelectedEmptyNoop(t *testing.T) {
	m := sampleModel()
	m.filter = "working"
	m.search.SetValue("nomatch")
	model, cmd := m.focusSelected()
	if model.quitting || cmd != nil {
		t.Error("focusSelected with no visible rows should be a no-op")
	}
}

func TestSearchEscClears(t *testing.T) {
	m := sampleModel()
	m.searching = true
	m.search.Focus()
	m.search.SetValue("oauth")
	model, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	got := model.(Model)
	if got.searching {
		t.Error("esc should leave search mode")
	}
	if got.search.Value() != "" {
		t.Errorf("esc should clear query, got %q", got.search.Value())
	}
}

func TestCounts(t *testing.T) {
	m := sampleModel()
	m.agents = append(m.agents, kitty.Agent{ID: 5, Display: "idle"})
	c := m.counts()
	want := map[string]int{"all": 5, "working": 2, "blocked": 1, "done": 1, "idle": 1}
	for k, v := range want {
		if c[k] != v {
			t.Errorf("counts[%q]: got %d, want %d", k, c[k], v)
		}
	}
}

// The spinner tick is the only thing that redraws a still list, so it must stop
// when no agent is working and start again when one shows up.
func TestSpinTickFollowsWorkingAgents(t *testing.T) {
	idle := []kitty.Agent{{ID: 1, Display: "idle"}}
	working := []kitty.Agent{{ID: 1, Display: "working"}}

	m := sampleModel()
	m.agents, m.spinning = nil, false
	afterIdle, cmd := m.Update(agentsMsg{generation: m.reloadGeneration, agents: idle})
	if cmd != nil || afterIdle.(Model).spinning {
		t.Errorf("an idle inventory must not schedule a spin tick")
	}

	started, cmd := afterIdle.(Model).Update(agentsMsg{generation: m.reloadGeneration, agents: working})
	if cmd == nil || !started.(Model).spinning {
		t.Fatalf("a working agent must schedule a spin tick")
	}

	still, cmd := started.(Model).Update(spinMsg{})
	if cmd == nil || !still.(Model).spinning {
		t.Errorf("the tick must keep going while an agent works")
	}

	stopping, _ := started.(Model).Update(agentsMsg{generation: m.reloadGeneration, agents: idle})
	stopped, cmd := stopping.(Model).Update(spinMsg{})
	if cmd != nil || stopped.(Model).spinning {
		t.Errorf("the tick must stop once no agent is working")
	}
}

func TestElapsed(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cases := []struct {
		name  string
		since time.Time
		want  string
	}{
		{"zero", time.Time{}, ""},
		{"seconds", now.Add(-47 * time.Second), "47s"},
		{"minutes", now.Add(-(2*time.Minute + 18*time.Second)), "2m 18s"},
		{"hours", now.Add(-(1*time.Hour + 5*time.Minute)), "1h 05m"},
		{"future", now.Add(30 * time.Second), "0s"},
	}
	for _, tc := range cases {
		if got := elapsed(now, tc.since); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestActivity(t *testing.T) {
	cases := []struct {
		name string
		in   kitty.Agent
		want string
	}{
		{"message wins over title", kitty.Agent{Display: "working", Title: "zsh", Msg: "refactor the prompt"}, "refactor the prompt"},
		{"blocked with title", kitty.Agent{Display: "blocked", Title: "build"}, "build"},
		{"blocked no title", kitty.Agent{Display: "blocked"}, "waiting for input"},
		{"working with title", kitty.Agent{Display: "working", Title: "writing tests"}, "writing tests"},
		{"done no title", kitty.Agent{Display: "done"}, "finished"},
		{"idle with message", kitty.Agent{Display: "idle", Msg: "ship the release"}, "ship the release"},
		{"idle with title", kitty.Agent{Display: "idle", Title: "fish"}, "fish"},
		// The status column already says "idle", so the row has nothing to add.
		{"idle bare", kitty.Agent{Display: "idle"}, ""},
	}
	for _, tc := range cases {
		if got := activity(tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The glyphs must stay in step with _AGENT_STATE_STYLE in kitty/cattery_tab.py:
// a dot means the same thing in the tab bar and in the picker, and only the
// color separates a finished agent from a running one. Idle is picker-only,
// because the tab bar draws nothing for it.
func TestStatusGlyph(t *testing.T) {
	cases := map[string]string{
		"working": "●",
		"blocked": "◆",
		"done":    "●",
		"idle":    "·",
	}
	for display, want := range cases {
		if got := statusGlyph(display); got != want {
			t.Errorf("statusGlyph(%q): got %q, want %q", display, got, want)
		}
	}
}

func TestActivityGlyph(t *testing.T) {
	wrk := kitty.Agent{Display: "working"}
	if a, b := activityGlyph(wrk, 0), activityGlyph(wrk, 1); a == b {
		t.Errorf("working glyph should advance with spin: %q == %q", a, b)
	}
	cases := map[string]string{
		"blocked": "◆",
		"done":    "●",
		// Idle's status glyph is the same middot as the separator in front of
		// it, so the row leaves it out.
		"idle": "",
	}
	for display, want := range cases {
		if got := activityGlyph(kitty.Agent{Display: display}, 3); got != want {
			t.Errorf("activityGlyph(%q): got %q, want %q", display, got, want)
		}
	}
}

func TestTimeLabel(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cases := []struct {
		name string
		in   kitty.Agent
		want string
	}{
		{"working", kitty.Agent{Display: "working", Since: now.Add(-90 * time.Second)}, "1m 30s"},
		{"blocked", kitty.Agent{Display: "blocked", Since: now.Add(-30 * time.Second)}, "waiting 30s"},
		{"done", kitty.Agent{Display: "done", Since: now.Add(-6 * time.Minute)}, "6m ago"},
		{"blocked unknown", kitty.Agent{Display: "blocked"}, "waiting"},
	}
	for _, tc := range cases {
		if got := timeLabel(now, tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestMetaRight(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cases := []struct {
		name string
		in   kitty.Agent
		want string
	}{
		{"working", kitty.Agent{Display: "working", Since: now.Add(-(13*time.Minute + 41*time.Second))}, "13m 41s"},
		{"done", kitty.Agent{Display: "done", Since: now.Add(-2 * time.Minute)}, "finished 2m ago"},
		{"done unknown", kitty.Agent{Display: "done"}, "finished"},
		{"idle has no summary", kitty.Agent{Display: "idle", Since: now.Add(-time.Minute)}, ""},
	}
	for _, tc := range cases {
		if got := metaRight(now, tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestAgentName(t *testing.T) {
	if got := agentName(kitty.Agent{CWD: "/p/dotfiles"}); got != "dotfiles" {
		t.Errorf("name from cwd: %q", got)
	}
	if got := agentName(kitty.Agent{Title: "fallback"}); got != "fallback" {
		t.Errorf("name fallback to title: %q", got)
	}
}

// Inside a project group the row has to say which checkout it is. The branch
// does that until there is no branch to show.
func TestRowLabel(t *testing.T) {
	cases := []struct {
		name  string
		agent kitty.Agent
		want  string
	}{
		{
			name:  "branch names the checkout",
			agent: kitty.Agent{CWD: "/p/dotfiles", Root: "/p/dotfiles", Branch: "main"},
			want:  "main",
		},
		{
			name:  "worktree branch beats the directory",
			agent: kitty.Agent{CWD: "/wt/feat-oauth", Root: "/wt/feat-oauth", Branch: "feat/oauth"},
			want:  "feat/oauth",
		},
		{
			name:  "detached HEAD falls back to the worktree",
			agent: kitty.Agent{CWD: "/tmp/sig-review/sub", Root: "/tmp/sig-review"},
			want:  "sig-review",
		},
		{
			name:  "outside git falls back to the folder",
			agent: kitty.Agent{CWD: "/home/x/scratch"},
			want:  "scratch",
		},
		{
			name:  "no path at all falls back to the title",
			agent: kitty.Agent{Title: "pi"},
			want:  "pi",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rowLabel(tc.agent); got != tc.want {
				t.Errorf("rowLabel: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGroupLabel(t *testing.T) {
	if got := groupLabel(kitty.Agent{Project: "dotfiles"}); got != "dotfiles" {
		t.Errorf("group label: %q", got)
	}
	if got := groupLabel(kitty.Agent{}); got != "unknown" {
		t.Errorf("group label without a project: %q", got)
	}
}

func TestGroupSize(t *testing.T) {
	agents := []kitty.Agent{
		{ID: 1, Project: "a", ProjectKey: "/a/.git"},
		{ID: 2, Project: "a", ProjectKey: "/a/.git"},
		// Same label, different repository: a separate group.
		{ID: 3, Project: "a", ProjectKey: "/other/a/.git"},
		{ID: 4, Project: "b", ProjectKey: "/b/.git"},
	}
	for start, want := range map[int]int{0: 2, 1: 1, 2: 1, 3: 1} {
		if got := groupSize(agents, start); got != want {
			t.Errorf("groupSize(%d): got %d, want %d", start, got, want)
		}
	}
}

// The point of grouping: several worktrees of one repository sit under a
// single heading, each identified by its own branch.
func TestViewGroupsWorktreesUnderOneHeading(t *testing.T) {
	m := sampleModel()
	m.width, m.height = 100, 40
	m.agents = []kitty.Agent{
		{ID: 1, Kind: "pi", Display: "working", CWD: "/p/dotfiles", Project: "dotfiles",
			ProjectKey: "/p/dotfiles/.git", Root: "/p/dotfiles", Branch: "main"},
		{ID: 2, Kind: "pi", Display: "idle", CWD: "/wt/dotfiles/feat-oauth", Project: "dotfiles",
			ProjectKey: "/p/dotfiles/.git", Root: "/wt/dotfiles/feat-oauth", Branch: "feat/oauth"},
		{ID: 3, Kind: "pi", Display: "idle", CWD: "/tmp/dotfiles-review", Project: "dotfiles",
			ProjectKey: "/p/dotfiles/.git", Root: "/tmp/dotfiles-review"},
		{ID: 4, Kind: "pi", Display: "working", CWD: "/p/sigil", Project: "sigil",
			ProjectKey: "/p/sigil/.git", Root: "/p/sigil", Branch: "main"},
	}

	out := ansi.Strip(m.View())

	// One heading per project, counting only that project's agents.
	if !strings.Contains(out, "dotfiles 3") {
		t.Errorf("missing dotfiles heading with 3 agents, got:\n%s", out)
	}
	if !strings.Contains(out, "sigil 1") {
		t.Errorf("missing sigil heading with 1 agent, got:\n%s", out)
	}
	if got := strings.Count(out, "dotfiles 3"); got != 1 {
		t.Errorf("dotfiles should head exactly one group, headed %d:\n%s", got, out)
	}
	// Each worktree names itself: two branches and the detached one's directory.
	for _, want := range []string{"feat/oauth", "dotfiles-review"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing row label %q, got:\n%s", want, out)
		}
	}
}

// A heading draws only when a row fits under it, and a viewport that starts
// mid-group re-draws the heading it scrolled past.
func TestViewGroupHeadingsSurviveScrolling(t *testing.T) {
	heading := func(project string, count int) string { return fmt.Sprintf("%s %d", project, count) }

	cases := []struct {
		name     string
		height   int
		selected int
		want     []string
		absent   []string
	}{
		{
			name: "everything fits", height: 40, selected: 0,
			want: []string{heading("alpha", 2), heading("zulu", 2)},
		},
		{
			// Room for alpha's heading and one row, but not for zulu's heading.
			name: "no orphan heading at the bottom edge", height: 14, selected: 0,
			want: []string{heading("alpha", 2)}, absent: []string{heading("zulu", 2)},
		},
		{
			name: "heading re-drawn when the group scrolls off", height: 14, selected: 3,
			want: []string{heading("zulu", 2)}, absent: []string{heading("alpha", 2)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.width, m.height = 60, tc.height
			m.agents = []kitty.Agent{
				checkout(1, "pi", "working", "alpha", "main"),
				checkout(2, "pi", "working", "alpha", "topic"),
				checkout(3, "pi", "working", "zulu", "main"),
				checkout(4, "pi", "working", "zulu", "topic"),
			}
			// Two rows of one project cannot share a ProjectKey built from the
			// branch, so give the pair the same repo explicitly.
			m.agents[1].ProjectKey, m.agents[1].CWD = m.agents[0].ProjectKey, "/p/alpha-wt"
			m.agents[3].ProjectKey, m.agents[3].CWD = m.agents[2].ProjectKey, "/p/zulu-wt"
			m.selected = tc.selected

			out := ansi.Strip(m.View())
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q, got:\n%s", w, out)
				}
			}
			for _, w := range tc.absent {
				if strings.Contains(out, w) {
					t.Errorf("should not contain %q, got:\n%s", w, out)
				}
			}
		})
	}
}

func TestViewRenders(t *testing.T) {
	m := sampleModel()
	m.width, m.height = 120, 40
	out := m.View()
	want := []string{
		"AGENTS",
		"blocked", "working",
		"dotfiles",    // name on line 1
		"/p/dotfiles", // cwd on line 2
		"esc close",
		"4 agents", // footer inventory summary
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("view missing %q", w)
		}
	}
}

// Every chrome line costs the list a line, so the top chrome draws only what
// is in use: the title sits in the title bar, and the search row waits for a
// query.
func TestHeaderLines(t *testing.T) {
	pad := lipgloss.NewStyle().PaddingLeft(2)
	cases := []struct {
		name      string
		roomy     bool
		searching bool
		query     string
		want      int
	}{
		{name: "roomy and untouched", roomy: true, want: 5},
		{name: "roomy while typing", roomy: true, searching: true, want: 6},
		{name: "roomy with a query", roomy: true, query: "oauth", want: 6},
		{name: "compact and untouched", want: 3},
		{name: "compact with a query", query: "oauth", want: 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.width, m.height = 100, 30
			m.searching = tc.searching
			m.search.SetValue(tc.query)
			if got := len(m.headerLines(100, 96, tc.roomy, pad)); got != tc.want {
				t.Fatalf("header lines: got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSearchRowVisibility(t *testing.T) {
	cases := []struct {
		name      string
		searching bool
		query     string
		want      string // text that must appear, or "" for the placeholder's absence
	}{
		{name: "untouched hides the field"},
		{name: "typing shows the placeholder", searching: true, want: "search by name"},
		{name: "a query keeps the field on screen", query: "oauth", want: "oauth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.width, m.height = 100, 30
			m.searching = tc.searching
			m.search.SetValue(tc.query)
			out := ansi.Strip(m.View())
			if tc.want == "" {
				if strings.Contains(out, "search by name") {
					t.Fatalf("unused search field still drawn:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("view missing %q:\n%s", tc.want, out)
			}
		})
	}
}

func TestTitleBarStates(t *testing.T) {
	cases := []struct {
		name      string
		searching bool
		focusing  bool
		want      string
	}{
		{name: "idle", want: "esc close"},
		{name: "searching", searching: true, want: "esc clear"},
		{name: "focusing", focusing: true, want: "focusing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.searching, m.focusing = tc.searching, tc.focusing
			bar := ansi.Strip(m.renderTitleBar(100))
			if !strings.Contains(bar, "AGENTS") {
				t.Errorf("title bar lost its name: %q", bar)
			}
			if !strings.Contains(bar, tc.want) {
				t.Errorf("title bar missing %q: %q", tc.want, bar)
			}
		})
	}
}

// Selecting a row must not slide the time column: holding j would otherwise
// shuffle the whole right edge on every keypress.
func TestRowTimeColumnHoldsStill(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	agent := kitty.Agent{
		ID: 1, Kind: "pi", Display: "working",
		CWD: "/p/dotfiles", Branch: "main", Since: now.Add(-90 * time.Second),
	}
	label := timeLabel(now, agent)

	cases := []struct {
		name            string
		inner           int
		wantTimeOnFocus bool
	}{
		{name: "wide row keeps time and hint together", inner: 100, wantTimeOnFocus: true},
		{name: "narrow row trades time for the hint", inner: 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.now = now

			m.selected = 0
			selected, _ := m.renderRow(0, agent, tc.inner, true)
			m.selected = 1
			unselected, _ := m.renderRow(0, agent, tc.inner, true)
			selected, unselected = ansi.Strip(selected), ansi.Strip(unselected)

			if !strings.Contains(selected, jumpHint) {
				t.Fatalf("selected row missing the jump hint: %q", selected)
			}
			if !strings.Contains(unselected, label) {
				t.Fatalf("unselected row missing its time: %q", unselected)
			}
			if !tc.wantTimeOnFocus {
				if strings.Contains(selected, label) {
					t.Fatalf("narrow selected row should drop the time: %q", selected)
				}
				return
			}
			// Compare terminal cells, not bytes: the selection bar is a wide rune.
			column := func(line string) int {
				before, _, found := strings.Cut(line, label)
				if !found {
					t.Fatalf("row missing the time label %q: %q", label, line)
				}
				return ansi.StringWidth(before)
			}
			if got, want := column(selected), column(unselected); got != want {
				t.Fatalf("time column moved on selection: %d vs %d\n%q\n%q", got, want, selected, unselected)
			}
		})
	}
}

// Line 2 of an idle row: nothing to report means the row stops after the path,
// and a title or message is printed without the redundant middot glyph.
func TestIdleRowLine2(t *testing.T) {
	indent := strings.Repeat(" ", rowIndent)
	cases := []struct {
		name  string
		agent kitty.Agent
		want  string
	}{
		{
			name:  "nothing to report",
			agent: kitty.Agent{ID: 8, Kind: "pi", Display: "idle", CWD: "/p/org"},
			want:  indent + "/p/org",
		},
		{
			name:  "message without a status glyph",
			agent: kitty.Agent{ID: 9, Kind: "pi", Display: "idle", CWD: "/p/org", Msg: "tidy the notes"},
			want:  indent + "/p/org · tidy the notes",
		},
		{
			name:  "title fallback",
			agent: kitty.Agent{ID: 10, Kind: "pi", Display: "idle", CWD: "/p/org", Title: "π - org"},
			want:  indent + "/p/org · π - org",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			_, line2 := m.renderRow(0, tc.agent, 80, true)
			if got := strings.TrimRight(ansi.Strip(line2), " "); got != tc.want {
				t.Fatalf("idle line 2: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestViewSearchMatchCount(t *testing.T) {
	m := sampleModel()
	m.width, m.height = 120, 40
	m.search.SetValue("oauth")
	if out := m.View(); !strings.Contains(out, "1 match") {
		t.Errorf("view should show match count for an active query")
	}
}

func TestAgentsMsgPreservesSelectedID(t *testing.T) {
	m := sampleModel()
	m.selected = 1 // id 2
	updated, _ := m.Update(agentsMsg{
		generation: m.reloadGeneration,
		agents: []kitty.Agent{
			{ID: 1, Display: "blocked", CWD: "/p/llm-proxy"},
			{ID: 4, Display: "done", CWD: "/p/qmp-relay"},
			{ID: 9, Display: "done", CWD: "/p/new"},
			{ID: 2, Display: "working", CWD: "/p/dotfiles"},
			{ID: 3, Display: "working", CWD: "/p/astra-l"},
		},
	})
	got := updated.(Model)
	if id := got.visible()[got.selected].ID; id != 2 {
		t.Fatalf("selected id after reorder: got %d, want 2", id)
	}
}

func TestStaleReloadResultIsIgnored(t *testing.T) {
	m := sampleModel()
	m.loading = true
	m.reloadGeneration = 2
	updated, _ := m.Update(agentsMsg{generation: 1, agents: nil})
	got := updated.(Model)
	if len(got.agents) != len(m.agents) || !got.loading {
		t.Fatalf("stale reload changed model: agents=%d loading=%v", len(got.agents), got.loading)
	}
}

func TestTickDoesNotStartOverlappingReload(t *testing.T) {
	m := sampleModel()
	m.loading = true
	generation := m.reloadGeneration
	updated, cmd := m.Update(tickMsg(time.Now()))
	got := updated.(Model)
	if got.reloadGeneration != generation || !got.loading || cmd == nil {
		t.Fatalf("tick while loading: generation=%d loading=%v cmd nil=%v", got.reloadGeneration, got.loading, cmd == nil)
	}
}

func TestFocusResult(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		quitting bool
	}{
		{name: "success", quitting: true},
		{name: "failure", err: errors.New("window gone")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.focusing = true
			updated, cmd := m.Update(focusMsg{err: tc.err})
			got := updated.(Model)
			if got.quitting != tc.quitting {
				t.Fatalf("quitting: got %v, want %v", got.quitting, tc.quitting)
			}
			if tc.err == nil {
				if cmd == nil || got.focusErr != nil {
					t.Fatalf("successful focus: cmd nil=%v err=%v", cmd == nil, got.focusErr)
				}
			} else if !errors.Is(got.focusErr, tc.err) || cmd != nil {
				t.Fatalf("failed focus: cmd nil=%v err=%v", cmd == nil, got.focusErr)
			}
		})
	}
}

func TestFocusCanBeCancelled(t *testing.T) {
	m := sampleModel()
	ctx, cancel := context.WithCancel(context.Background())
	m.focusing = true
	m.focusCancel = cancel
	updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	got := updated.(Model)
	if !got.quitting || cmd == nil {
		t.Fatalf("esc while focusing: quitting=%v cmd nil=%v", got.quitting, cmd == nil)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("focus context was not cancelled")
	}
}

func TestKeysWhilePendingFocus(t *testing.T) {
	cases := []struct {
		name  string
		key   tea.KeyMsg
		quits bool
	}{
		{name: "q closes", key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}, quits: true},
		{name: "esc closes", key: tea.KeyMsg{Type: tea.KeyEsc}, quits: true},
		{name: "ctrl+c closes", key: tea.KeyMsg{Type: tea.KeyCtrlC}, quits: true},
		{name: "down ignored", key: tea.KeyMsg{Type: tea.KeyDown}},
		{name: "j ignored", key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}}},
		{name: "enter ignored", key: tea.KeyMsg{Type: tea.KeyEnter}},
		{name: "f ignored", key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}}},
		{name: "number ignored", key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.focusing = true
			m.selected = 1
			updated, cmd := m.handleKey(tc.key)
			got := updated.(Model)
			if got.quitting != tc.quits {
				t.Fatalf("quitting: got %v, want %v", got.quitting, tc.quits)
			}
			if tc.quits {
				if cmd == nil {
					t.Fatal("close key should return a quit command")
				}
				return
			}
			if cmd != nil {
				t.Error("ignored key should not issue a command")
			}
			if got.selected != 1 || got.filter != "all" || !got.focusing {
				t.Errorf("ignored key changed state: selected=%d filter=%s focusing=%v",
					got.selected, got.filter, got.focusing)
			}
		})
	}
}

func TestPendingFocusCancelsOnClose(t *testing.T) {
	m := sampleModel()
	ctx, cancel := context.WithCancel(context.Background())
	m.focusing = true
	m.focusCancel = cancel
	if _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}); ctx.Err() == nil {
		t.Fatal("q while focusing should cancel the pending focus context")
	}
}

// A jump error names one agent, so it must not outlive a move to another. It is
// keyed on the selected window, not on keystrokes, so a key that leaves the
// cursor where it was keeps the retry hint on screen.
func TestFocusErrClearsWhenSelectionChanges(t *testing.T) {
	cases := []struct {
		name     string
		selected int
		key      tea.KeyMsg
		cleared  bool
	}{
		{name: "move down", selected: 1, key: tea.KeyMsg{Type: tea.KeyDown}, cleared: true},
		{name: "move up", selected: 1, key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}}, cleared: true},
		{name: "number select", selected: 1, key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'4'}}, cleared: true},
		{name: "jump to top", selected: 1, key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}}, cleared: true},
		{name: "jump to end", selected: 1, key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}}, cleared: true},
		// all -> working drops the blocked agent and resets to row 0, moving the
		// cursor off window 1.
		{name: "cycle filter onto another agent", selected: 2, key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}}, cleared: true},
		// all -> working keeps window 3 on row 0, so the retry stands.
		{name: "cycle filter onto the same agent", selected: 0, key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}}},
		{name: "open search keeps error", selected: 1, key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}}},
		{name: "unbound key keeps error", selected: 1, key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.selected = tc.selected
			m.focusErr = errors.New("window gone")
			updated, _ := m.handleKey(tc.key)
			if got := updated.(Model).focusErr; (got == nil) != tc.cleared {
				t.Fatalf("focusErr after %s: got %v, want cleared=%v", tc.name, got, tc.cleared)
			}
		})
	}
}

func TestFocusErrClearsWhenSearchNarrowsSelection(t *testing.T) {
	m := sampleModel()
	m.searching = true
	m.search.Focus()
	m.selected = 2 // id 1, llm-proxy
	m.focusErr = errors.New("window 1 gone")

	// "f" matches dotfiles and feat/oauth but not the selected llm-proxy row,
	// so the cursor moves to another window and the stale error goes with it.
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	got := updated.(Model)
	if got.selectedID() == 1 {
		t.Fatal("query should have moved the selection off window 1")
	}
	if got.focusErr != nil {
		t.Fatalf("focusErr after search edit: got %v, want nil", got.focusErr)
	}
}

func TestReplaceAgentsSelectionFallback(t *testing.T) {
	cases := []struct {
		name     string
		filter   string
		query    string
		selected int
		agents   []kitty.Agent
		wantID   int
	}{
		{
			name:     "selected window closes, index stays valid",
			selected: 1, // id 2
			agents: []kitty.Agent{
				{ID: 1, Display: "blocked"},
				{ID: 3, Display: "working"},
				{ID: 4, Display: "done"},
			},
			wantID: 3, // same index, next agent down
		},
		{
			name:     "selected window closes at the end, clamps",
			selected: 3, // id 4
			agents: []kitty.Agent{
				{ID: 1, Display: "blocked"},
				{ID: 2, Display: "working"},
			},
			wantID: 2,
		},
		{
			name:     "selection survives under an active filter",
			filter:   "working",
			selected: 1, // id 2
			agents: []kitty.Agent{
				{ID: 9, Display: "working", CWD: "/p/new"},
				{ID: 2, Display: "working", CWD: "/p/dotfiles"},
				{ID: 3, Display: "working", CWD: "/p/astra-l"},
			},
			wantID: 2,
		},
		{
			name:     "selection survives under an active query",
			query:    "working",
			selected: 1, // id 2
			agents: []kitty.Agent{
				{ID: 3, Display: "working", CWD: "/p/astra-l"},
				{ID: 1, Display: "blocked", CWD: "/p/llm-proxy"},
				{ID: 2, Display: "working", CWD: "/p/dotfiles"},
			},
			wantID: 2,
		},
		{
			name:     "empty inventory resets to the empty state",
			selected: 2,
			agents:   nil,
			wantID:   0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			if tc.filter != "" {
				m.filter = tc.filter
			}
			if tc.query != "" {
				m.search.SetValue(tc.query)
			}
			m.selected = tc.selected

			m.replaceAgents(tc.agents)

			if got := m.selectedID(); got != tc.wantID {
				t.Fatalf("selected id: got %d, want %d", got, tc.wantID)
			}
			if vis := m.visible(); len(vis) > 0 && (m.selected < 0 || m.selected >= len(vis)) {
				t.Fatalf("selection %d outside visible list of %d", m.selected, len(vis))
			}
		})
	}
}

func TestSearchCtrlCQuits(t *testing.T) {
	m := sampleModel()
	m.searching = true
	m.search.Focus()
	updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	got := updated.(Model)
	if !got.quitting || cmd == nil {
		t.Fatalf("ctrl+c in search: quitting=%v cmd nil=%v", got.quitting, cmd == nil)
	}
}

// The one-shot check has to look at the directory setup installs into, or the
// warning it raises names a fix that changes nothing.
func TestCheckAssetsReadsTheKittyDirectory(t *testing.T) {
	cases := []struct {
		name    string
		install bool
		want    bool
	}{
		{name: "an install that does not match this binary", install: true, want: true},
		{name: "nothing installed", install: false, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("KITTY_CONFIG_DIRECTORY", dir)
			if tc.install {
				for _, name := range cattery.ManagedFiles {
					if err := os.WriteFile(filepath.Join(dir, name), []byte("# stale\n"), 0o644); err != nil {
						t.Fatalf("write %s: %v", name, err)
					}
				}
			}
			msg, ok := checkAssets()().(staleMsg)
			if !ok {
				t.Fatalf("checkAssets returned %T, want staleMsg", msg)
			}
			if msg.stale != tc.want {
				t.Fatalf("stale: got %v, want %v", msg.stale, tc.want)
			}
		})
	}
}

func TestStaleMsgSetsTheFlag(t *testing.T) {
	for _, stale := range []bool{true, false} {
		m := sampleModel()
		updated, cmd := m.Update(staleMsg{stale: stale})
		if got := updated.(Model).staleAssets; got != stale {
			t.Errorf("staleAssets after staleMsg{%v}: got %v", stale, got)
		}
		// One shot: the check does not reschedule itself.
		if cmd != nil {
			t.Errorf("staleMsg{%v} returned a command", stale)
		}
	}
}

func TestViewLoadStates(t *testing.T) {
	cases := []struct {
		name   string
		set    func(*Model)
		want   string
		absent []string
	}{
		{
			name: "loading",
			set:  func(m *Model) { m.loaded = false },
			want: "loading agents",
			absent: []string{
				"no agents.",                 // loading is not an empty inventory
				"showing cached agents",      // nor stale data
				"remote control unavailable", // nor a fatal failure
			},
		},
		{
			name:   "empty inventory",
			set:    func(m *Model) { m.agents = nil },
			want:   "nothing has published agent state yet",
			absent: []string{"loading agents", "showing cached agents", "remote control unavailable"},
		},
		{
			// A first load that fails is fatal: there is nothing cached to show.
			name:   "fatal first load",
			set:    func(m *Model) { m.agents = nil; m.reloadErr = errors.New("socket unavailable") },
			want:   "kitty remote control unavailable",
			absent: []string{"loading agents", "no agents.", "showing cached agents"},
		},
		{
			name:   "stale",
			set:    func(m *Model) { m.reloadErr = errors.New("socket unavailable") },
			want:   "showing cached agents",
			absent: []string{"loading agents", "remote control unavailable"},
		},
		{
			name: "focus failure",
			set:  func(m *Model) { m.focusErr = errors.New("window 42: no matching window") },
			want: "jump failed",
		},
		{
			name: "focus failure names the window and retry",
			set:  func(m *Model) { m.focusErr = errors.New("window 42: no matching window") },
			want: "window 42",
		},
		{
			name: "focus failure offers a retry",
			set:  func(m *Model) { m.focusErr = errors.New("window 42: no matching window") },
			want: "press enter to retry",
		},
		{
			// A binary upgraded without a second `cattery setup` leaves the
			// installed kitty files behind, and nothing else would say so.
			name:   "stale kitty files",
			set:    func(m *Model) { m.staleAssets = true },
			want:   "kitty files are out of date · run cattery setup",
			absent: []string{"showing cached agents"},
		},
		{
			name:   "fresh kitty files say nothing",
			set:    func(m *Model) { m.staleAssets = false },
			absent: []string{"out of date"},
			want:   "astra-l", // a normal list, with no warning row
		},
		{
			// One warning row, and the failed refresh owns it: it explains the
			// rows on screen, while stale files are a background chore.
			name:   "a failed refresh outranks stale files",
			set:    func(m *Model) { m.staleAssets = true; m.reloadErr = errors.New("socket unavailable") },
			want:   "showing cached agents",
			absent: []string{"out of date"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.width, m.height = 120, 40
			tc.set(&m)
			out := ansi.Strip(m.View())
			if !strings.Contains(out, tc.want) {
				t.Fatalf("view missing %q, got:\n%s", tc.want, out)
			}
			for _, a := range tc.absent {
				if strings.Contains(out, a) {
					t.Errorf("view should not contain %q, got:\n%s", a, out)
				}
			}
		})
	}
}

func TestViewFitsTerminal(t *testing.T) {
	sizes := []struct{ width, height int }{
		{120, 40},
		{80, 24},
		{80, 14}, // full-header layout, minimum height
		{80, 13}, // compact-header layout, maximum height
		{60, 14},
		{40, 14},
		{40, 13},
		{40, 10},
		{20, 7}, // smallest supported terminal
		{19, 7}, // below the supported width
		{19, 6},
		{1, 1},
	}
	// Each variant stresses a different source of over-long content.
	variants := []struct {
		name string
		set  func(*Model)
	}{
		{name: "plain", set: func(*Model) {}},
		{name: "idle filter", set: func(m *Model) { m.filter = "idle" }},
		{name: "done filter", set: func(m *Model) { m.filter = "done" }},
		{name: "search", set: func(m *Model) { m.search.SetValue("oauth") }},
		{name: "search no match", set: func(m *Model) { m.search.SetValue("zzz") }},
		{
			name: "filtered search no match",
			set:  func(m *Model) { m.filter = "done"; m.search.SetValue("zzz") },
		},
		{name: "loading", set: func(m *Model) { m.loaded = false }},
		{
			name: "fatal reload",
			set:  func(m *Model) { m.agents = nil; m.reloadErr = errors.New("kitty socket unavailable at /tmp/mykitty") },
		},
		{name: "stale cache", set: func(m *Model) { m.reloadErr = errors.New("socket unavailable") }},
		{name: "stale kitty files", set: func(m *Model) { m.staleAssets = true }},
		{
			name: "focus failure",
			set:  func(m *Model) { m.focusErr = errors.New("window 42: no matching window found for id:42") },
		},
		{
			// Both banners plus rows squeeze the body to its minimum.
			name: "both banners",
			set: func(m *Model) {
				m.focusErr = errors.New("window 42: gone")
				m.reloadErr = errors.New("socket unavailable")
			},
		},
		{
			name: "multiline error",
			set:  func(m *Model) { m.focusErr = errors.New("first line\nsecond line\nthird line") },
		},
		{name: "wide unicode", set: func(m *Model) { m.agents = wideAgents() }},
		{name: "scrolled", set: func(m *Model) { m.agents = manyAgents(30); m.selected = 25 }},
		{name: "stale selection", set: func(m *Model) { m.selected = 99 }},
	}
	for _, size := range sizes {
		for _, v := range variants {
			t.Run(fmt.Sprintf("%dx%d/%s", size.width, size.height, v.name), func(t *testing.T) {
				m := sampleModel()
				m.width, m.height = size.width, size.height
				v.set(&m)
				lines := strings.Split(m.View(), "\n")
				if len(lines) != size.height {
					t.Fatalf("line count: got %d, want %d", len(lines), size.height)
				}
				for i, line := range lines {
					if width := ansi.StringWidth(line); width > size.width {
						t.Errorf("line %d width: got %d, max %d\n%q", i+1, width, size.width, line)
					}
				}
			})
		}
	}
}

// wideAgents exercises cell-width truncation: CJK, emoji ZWJ sequences, and
// combining marks in every field the row renders.
func wideAgents() []kitty.Agent {
	return []kitty.Agent{
		{
			ID: 1, Kind: "claude", Display: "blocked",
			CWD:    "/p/项目名称很长很长很长很长/子目录",
			Branch: "功能/认证",
			Msg:    "请帮我实现一个非常复杂的认证系统👩‍💻👨‍👩‍👧‍👦",
			Title:  "ééé combining",
		},
		{
			ID: 2, Kind: "pi", Display: "working",
			CWD:    "/p/🚀🚀🚀",
			Branch: "feat/🔥",
			Msg:    strings.Repeat("界", 200),
		},
	}
}

func manyAgents(n int) []kitty.Agent {
	agents := make([]kitty.Agent, 0, n)
	for i := range n {
		agents = append(agents, kitty.Agent{
			ID:      i + 1,
			Kind:    "pi",
			Display: "working",
			CWD:     fmt.Sprintf("/p/project-%02d", i),
			Branch:  "main",
		})
	}
	return agents
}

func TestViewFilterTabs(t *testing.T) {
	cases := []struct {
		name       string
		width      int
		filter     string
		want       []string
		wantAbsent []string
	}{
		{
			name:  "all tabs fit at 120 columns",
			width: 120, filter: "all",
			want: []string{"all", "working", "blocked", "done", "idle"},
		},
		{
			name:  "idle stays visible when tabs collapse",
			width: 40, filter: "idle",
			want:       []string{"idle", "f next"},
			wantAbsent: []string{"blocked"},
		},
		{
			name:  "done stays visible when tabs collapse",
			width: 40, filter: "done",
			want:       []string{"done", "f next"},
			wantAbsent: []string{"blocked"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.width, m.height = tc.width, 24
			m.filter = tc.filter
			out := ansi.Strip(m.View())
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("view missing %q, got:\n%s", w, out)
				}
			}
			for _, w := range tc.wantAbsent {
				if strings.Contains(out, w) {
					t.Errorf("collapsed tabs should not show %q, got:\n%s", w, out)
				}
			}
		})
	}
}

func TestViewEmptyStates(t *testing.T) {
	cases := []struct {
		name   string
		set    func(*Model)
		want   []string
		absent []string
	}{
		{
			name: "no inventory at all",
			set:  func(m *Model) { m.agents = nil },
			want: []string{"no agents.", "nothing has published agent state yet"},
		},
		{
			name:   "filter excludes everything",
			set:    func(m *Model) { m.filter = "idle" },
			want:   []string{"no idle agents.", "press f to change filter"},
			absent: []string{"nothing has published agent state yet"},
		},
		{
			name: "query excludes everything",
			set:  func(m *Model) { m.search.SetValue("zzz-nope") },
			want: []string{`no agents match "zzz-nope".`, "press esc to clear the search"},
		},
		{
			name: "filter and query exclude everything",
			set:  func(m *Model) { m.filter = "done"; m.search.SetValue("zzz-nope") },
			want: []string{`no done agents match "zzz-nope".`, "press esc to clear the search", "f to change filter"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.width, m.height = 120, 40
			tc.set(&m)
			out := ansi.Strip(m.View())
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("view missing %q, got:\n%s", w, out)
				}
			}
			for _, w := range tc.absent {
				if strings.Contains(out, w) {
					t.Errorf("view should not contain %q, got:\n%s", w, out)
				}
			}
		})
	}
}

func TestSearchMatchTextNamesFilterScope(t *testing.T) {
	cases := []struct {
		name   string
		filter string
		query  string
		want   string
	}{
		{name: "no query", filter: "all", want: ""},
		{name: "all scope plural", filter: "all", query: "p/", want: "4 matches"},
		{name: "all scope singular", filter: "all", query: "oauth", want: "1 match"},
		{name: "filtered scope", filter: "working", query: "oauth", want: "1 match in working"},
		{name: "filtered scope empty", filter: "done", query: "zzz", want: "0 matches in done"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModel()
			m.filter = tc.filter
			m.search.SetValue(tc.query)
			if got := m.searchMatchText(); got != tc.want {
				t.Fatalf("searchMatchText: got %q, want %q", got, tc.want)
			}
		})
	}
}

// Position leads the footer summary, because truncation takes the right side
// first and a scrolled list is where the position matters most.
func TestFooterKeepsPositionWhenScrolled(t *testing.T) {
	for _, width := range []int{40, 80} {
		t.Run(fmt.Sprintf("%d columns", width), func(t *testing.T) {
			m := sampleModel()
			m.width, m.height = width, 24
			m.agents = manyAgents(30)
			m.selected = 25

			out := ansi.Strip(m.View())
			if !strings.Contains(out, "26/30") {
				t.Errorf("footer missing selected position, got:\n%s", out)
			}

			// The selected row must also stay inside the rendered viewport.
			if !strings.Contains(out, "project-25") {
				t.Errorf("selected row scrolled out of view, got:\n%s", out)
			}
		})
	}
}

// The compact boundary must keep the four things needed to act: which filter
// is applied, which row is selected, where it sits, and how to get out.
func TestViewKeepsEssentialsWhenCompact(t *testing.T) {
	sizes := []struct{ width, height int }{
		{20, 7},
		{40, 13},
		{40, 14},
		{80, 13},
		{80, 14},
	}
	for _, size := range sizes {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			m := sampleModel()
			m.width, m.height = size.width, size.height
			m.filter = "working"
			m.selected = 0 // first working agent, id 3

			out := ansi.Strip(m.View())
			for _, want := range []string{
				"working",   // active filter stays visible
				"▌",         // the selected row is marked
				"ast",       // and is identifiable, even truncated
				"esc close", // close guidance survives whole
				"1/2",       // selected/visible position
			} {
				if !strings.Contains(out, want) {
					t.Errorf("compact view missing %q, got:\n%s", want, out)
				}
			}
		})
	}
}

func TestPickHints(t *testing.T) {
	tiers := []string{"aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbb", "cc"}
	cases := []struct {
		name       string
		rightWidth int
		inner      int
		want       string
	}{
		{name: "widest tier fits", rightWidth: 5, inner: 40, want: "aaaaaaaaaaaaaaaaaaaa"},
		{name: "falls to middle tier", rightWidth: 5, inner: 16, want: "bbbbbbbbbb"},
		{name: "falls to shortest tier", rightWidth: 20, inner: 9, want: "cc"},
		{name: "keeps shortest when nothing fits", rightWidth: 20, inner: 1, want: "cc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickHints(tiers, tc.rightWidth, tc.inner); got != tc.want {
				t.Fatalf("pickHints: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFitPair(t *testing.T) {
	left := []string{"AGENTS", "AG"}
	right := []string{"esc clear · ^c close", "esc clear"}
	cases := []struct {
		name                string
		inner               int
		wantLeft, wantRight string
	}{
		{name: "both full", inner: 40, wantLeft: "AGENTS", wantRight: "esc clear · ^c close"},
		{name: "right degrades first", inner: 18, wantLeft: "AGENTS", wantRight: "esc clear"},
		{name: "left degrades last", inner: 13, wantLeft: "AG", wantRight: "esc clear"},
		{name: "nothing fits keeps barest", inner: 2, wantLeft: "AG", wantRight: "esc clear"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotLeft, gotRight := fitPair(left, right, tc.inner)
			if gotLeft != tc.wantLeft || gotRight != tc.wantRight {
				t.Fatalf("fitPair: got (%q, %q), want (%q, %q)", gotLeft, gotRight, tc.wantLeft, tc.wantRight)
			}
		})
	}
}

func TestComposeLine(t *testing.T) {
	sp := func(text string) []span { return []span{{text, cText, false, ""}} }
	cases := []struct {
		name        string
		left, right []span
		inner       int
		want        string
	}{
		{name: "exact fit", left: sp("abcd"), inner: 4, want: "abcd"},
		{name: "pads short left", left: sp("ab"), inner: 4, want: "ab  "},
		{name: "truncates overflowing left", left: sp("abcdefgh"), inner: 4, want: "abc…"},
		{name: "left and right with gap", left: sp("ab"), right: sp("yz"), inner: 6, want: "ab  yz"},
		{name: "clamps right to a third", left: sp("abcdef"), right: sp("wxyz"), inner: 9, want: "abcd… wx…"},
		{name: "zero width", left: sp("abc"), inner: 0, want: ""},
		{name: "negative width", left: sp("abc"), inner: -3, want: ""},
		{name: "single cell", left: sp("abc"), inner: 1, want: "…"},
		{name: "wide runes fit", left: sp("界界"), inner: 4, want: "界界"},
		// The ellipsis cannot split a wide rune, so the line pads to inner.
		{name: "wide runes truncate", left: sp("界界界"), inner: 4, want: "界… "},
		{name: "wide runes pad odd width", left: sp("界"), inner: 3, want: "界 "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ansi.Strip(composeLine(tc.left, tc.right, tc.inner, ""))
			if got != tc.want {
				t.Fatalf("composeLine: got %q, want %q", got, tc.want)
			}
			if tc.inner > 0 && ansi.StringWidth(got) > tc.inner {
				t.Fatalf("composeLine exceeded inner: %d > %d", ansi.StringWidth(got), tc.inner)
			}
		})
	}
}

func TestOneLine(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{name: "single line unchanged", in: "window 42: gone", want: "window 42: gone"},
		{name: "collapses newlines", in: "first\nsecond\nthird", want: "first second third"},
		{name: "collapses tabs and runs", in: "a\t\tb   c", want: "a b c"},
		{name: "trims surrounding space", in: "  padded  \n", want: "padded"},
		{name: "empty", in: " \n\t ", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := oneLine(tc.in); got != tc.want {
				t.Fatalf("oneLine: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTruncateUsesTerminalCells(t *testing.T) {
	cases := []struct {
		name string
		text string
		max  int
		want string
	}{
		{name: "ascii", text: "abcdef", max: 4, want: "abc…"},
		{name: "wide", text: "界界界", max: 3, want: "界…"},
		{name: "grapheme", text: "👩‍💻abc", max: 3, want: "👩‍💻…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncate(tc.text, tc.max); got != tc.want {
				t.Fatalf("truncate: got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestShortenHomeRequiresPathBoundary(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := home + "-other/project"
	if got := shortenHome(path); got != path {
		t.Fatalf("shortenHome(%q): got %q", path, got)
	}
}
