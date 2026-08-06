// Package overlay implements the picker TUI, which kitty runs in an overlay
// window: a two-lines-per-agent list with status filters, search, and
// jump-to-focus.
package overlay

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/alexander-akhmetov/cattery/internal/kitty"
	"github.com/alexander-akhmetov/cattery/internal/session"
	"github.com/alexander-akhmetov/cattery/internal/setup"
)

// filters are the status tabs, in cycle order for the "f" key.
var filters = []string{"all", "working", "blocked", "done", "idle"}

// KittyClient is the part of kitty remote control the picker needs. An
// interface, so a test can drive the model without a live kitty instance.
//
// It embeds session.Client because "s" and "R" snapshot and restore the tab
// tree from inside the picker.
type KittyClient interface {
	ListAgents(context.Context) ([]kitty.Agent, error)
	FocusWindow(context.Context, int) error
	session.Client
}

type (
	agentsMsg struct {
		generation uint64
		agents     []kitty.Agent
	}
	reloadErrMsg struct {
		generation uint64
		err        error
	}
	focusMsg struct{ err error }
	staleMsg struct{ stale bool }
	tickMsg  time.Time
	spinMsg  struct{}

	// sessionMsg is the result of a save or a restore started from the picker.
	// A restore that typed fewer commands than the snapshot holds sets short,
	// because that shortfall carries no error.
	sessionMsg struct {
		summary string
		short   bool
		err     error
	}
	// noticeExpiredMsg clears only the notice it was scheduled for. A second
	// action before the first tick arrives keeps its own message.
	noticeExpiredMsg struct{ id uint64 }
)

// noticeLife is how long a save or restore result stays on screen: long enough
// to read a path, short enough to look temporary.
const noticeLife = 4 * time.Second

// saveTimeout and restoreTimeout bound a save or a restore started from the
// picker. Restore waits for restored windows to draw a prompt, so it gets more
// time.
const (
	saveTimeout    = 15 * time.Second
	restoreTimeout = 60 * time.Second
)

// noticeLevel decides how a save or restore result is coloured.
type noticeLevel int

const (
	noticeOK noticeLevel = iota
	noticeShort
	noticeErr
)

// spinInterval drives the braille activity spinner on working agents. It runs
// faster than the data reload, so the animation stays smooth without re-running
// `kitten @ ls` and git on every frame. The tick runs only while an agent is
// working, or it would redraw the same frame nine times a second.
const spinInterval = 110 * time.Millisecond

// Model is the Bubble Tea model for the picker.
type Model struct {
	client KittyClient

	agents           []kitty.Agent
	filter           string
	search           textinput.Model
	searching        bool
	selected         int
	loaded           bool
	loading          bool
	reloadGeneration uint64
	reloadErr        error
	focusErr         error
	focusing         bool
	focusCancel      context.CancelFunc

	// staleAssets is set when the installed kitty files no longer match the
	// copies this binary carries, which means the binary was upgraded and setup
	// has not run since.
	staleAssets bool

	// notice reports what "s" or "R" just did. It clears itself on a tick, so
	// the result does not sit above the list for the rest of the session.
	notice      string
	noticeLevel noticeLevel
	noticeID    uint64

	// sessionBusy is a save or restore in flight. s and R do nothing until it
	// finishes, and sessionVerb is what the footer calls it meanwhile.
	sessionBusy bool
	sessionVerb string

	width    int
	height   int
	now      time.Time
	spin     int  // braille spinner frame, advanced by spinMsg
	spinning bool // a spin tick is scheduled

	quitting bool
}

// New builds a Model with an initial reload scheduled in Init.
func New(client KittyClient) Model {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = "search by name, path, branch, prompt…"
	in.CharLimit = 120

	return Model{
		client:           client,
		filter:           "all",
		search:           in,
		loading:          true,
		reloadGeneration: 1,
		now:              time.Now(),
	}
}

// Init kicks off the first agent load, the refresh tick, and the one-shot check
// for stale kitty files. The spinner tick starts with the first working agent.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.reload(m.reloadGeneration), tick(), checkAssets())
}

// checkAssets compares the installed kitty files with the copies embedded in
// this binary, once. A command rather than part of New, so the constructor does
// no file I/O.
func checkAssets() tea.Cmd {
	return func() tea.Msg {
		dir, err := setup.KittyDir()
		if err != nil {
			return staleMsg{}
		}
		return staleMsg{stale: setup.Stale(dir)}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func spin() tea.Cmd {
	return tea.Tick(spinInterval, func(time.Time) tea.Msg { return spinMsg{} })
}

func (m Model) reload(generation uint64) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		agents, err := client.ListAgents(ctx)
		if err != nil {
			return reloadErrMsg{generation: generation, err: err}
		}
		return agentsMsg{generation: generation, agents: agents}
	}
}

func focusCmd(ctx context.Context, client KittyClient, id int) tea.Cmd {
	return func() tea.Msg {
		return focusMsg{err: client.FocusWindow(ctx, id)}
	}
}

// saveCmd snapshots the tab tree to the default snapshot path. The key is a
// shortcut; `cattery save <path>` is how to name a path.
func saveCmd(client KittyClient) tea.Cmd {
	return func() tea.Msg {
		path, err := session.DefaultPath()
		if err != nil {
			return sessionMsg{err: err}
		}
		if err := session.EnsureDir(path); err != nil {
			return sessionMsg{err: err}
		}
		ctx, cancel := context.WithTimeout(context.Background(), saveTimeout)
		defer cancel()
		stats, err := session.Save(ctx, client, path)
		if err != nil {
			return sessionMsg{err: err}
		}
		return sessionMsg{summary: stats.Saved(path)}
	}
}

// restoreCmd puts the default snapshot back. It never runs the resumed agents,
// because one keypress in the picker is easy to hit by accident.
// `cattery restore -run` is how to ask for that on purpose.
func restoreCmd(client KittyClient) tea.Cmd {
	return func() tea.Msg {
		path, err := session.DefaultPath()
		if err != nil {
			return sessionMsg{err: err}
		}
		ctx, cancel := context.WithTimeout(context.Background(), restoreTimeout)
		defer cancel()
		stats, err := session.Restore(ctx, client, path, false)
		if err != nil {
			return sessionMsg{err: err}
		}
		return sessionMsg{summary: stats.Restored(false), short: stats.Incomplete()}
	}
}

func expireNotice(id uint64) tea.Cmd {
	return tea.Tick(noticeLife, func(time.Time) tea.Msg { return noticeExpiredMsg{id: id} })
}

// Update handles messages and key input.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resizeSearch()
		return m, nil
	case tickMsg:
		m.now = time.Time(msg)
		if m.loading {
			return m, tick()
		}
		m.loading = true
		m.reloadGeneration++
		return m, tea.Batch(m.reload(m.reloadGeneration), tick())
	case spinMsg:
		m.spin++
		if !m.anyWorking() {
			m.spinning = false
			return m, nil
		}
		return m, spin()
	case agentsMsg:
		if msg.generation != m.reloadGeneration {
			return m, nil
		}
		m.replaceAgents(msg.agents)
		m.resizeSearch()
		m.loaded = true
		m.loading = false
		m.reloadErr = nil
		if !m.spinning && m.anyWorking() {
			m.spinning = true
			return m, spin()
		}
		return m, nil
	case reloadErrMsg:
		if msg.generation != m.reloadGeneration {
			return m, nil
		}
		m.loaded = true
		m.loading = false
		m.reloadErr = msg.err
		return m, nil
	case staleMsg:
		m.staleAssets = msg.stale
		return m, nil
	case sessionMsg:
		m.sessionBusy = false
		m.noticeID++
		switch {
		case msg.err != nil:
			m.notice, m.noticeLevel = oneLine(msg.err.Error()), noticeErr
		case msg.short:
			m.notice, m.noticeLevel = msg.summary, noticeShort
		default:
			m.notice, m.noticeLevel = msg.summary, noticeOK
		}
		return m, expireNotice(m.noticeID)
	case noticeExpiredMsg:
		// A newer notice replaced this one and owns its own tick.
		if msg.id == m.noticeID {
			m.notice = ""
			m.noticeLevel = noticeOK
		}
		return m, nil
	case focusMsg:
		m.cancelFocus()
		m.focusing = false
		if msg.err != nil {
			m.focusErr = msg.err
			return m, nil
		}
		m.quitting = true
		return m, tea.Quit
	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	if m.searching {
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		m.resizeSearch()
		return m, cmd
	}
	return m, nil
}

func (m *Model) resizeSearch() {
	m.search.Width = m.searchFieldWidth(m.width - 4)
	m.search.SetCursor(m.search.Position())
}

func (m Model) searchFieldWidth(inner int) int {
	width := inner - 2 // search prompt
	if match := m.searchMatchText(); match != "" {
		width -= ansi.StringWidth(match) + 1
	}
	if width < 1 {
		return 1
	}
	return width
}

// searchMatchText is the count shown beside the query. Matches are counted
// inside the active filter, so the text names that filter unless it is "all".
func (m Model) searchMatchText() string {
	if strings.TrimSpace(m.search.Value()) == "" {
		return ""
	}
	count := len(m.visible())
	noun := "matches"
	if count == 1 {
		noun = "match"
	}
	if m.filter != "all" {
		return fmt.Sprintf("%d %s in %s", count, noun, m.filter)
	}
	return fmt.Sprintf("%d %s", count, noun)
}

// searchActive reports whether the search field belongs on screen: while the
// user types, and for as long as a query keeps narrowing the list.
func (m Model) searchActive() bool {
	return m.searching || strings.TrimSpace(m.search.Value()) != ""
}

// selectedID is the kitty window under the cursor, or 0 when nothing is
// visible. The window id survives filter, search, and inventory changes; a row
// index does not.
func (m Model) selectedID() int {
	vis := m.visible()
	if m.selected < 0 || m.selected >= len(vis) {
		return 0
	}
	return vis[m.selected].ID
}

func (m *Model) replaceAgents(agents []kitty.Agent) {
	selectedID := m.selectedID()

	m.agents = agents
	if selectedID != 0 {
		for i, agent := range m.visible() {
			if agent.ID == selectedID {
				m.selected = i
				return
			}
		}
	}
	m.clampSelection()
}

// handleKey dispatches a key press. While a focus command is in flight, only
// the close keys act and every other key waits for the result. Moving the
// selection drops a stale jump error, so "press enter to retry" always names
// the agent under the cursor.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.focusing {
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			m.cancelFocus()
			m.quitting = true
			return m, tea.Quit
		default:
			return m, nil
		}
	}

	before := m.selectedID()
	next, cmd := m.handleActiveKey(msg)
	if next.focusErr != nil && next.selectedID() != before {
		next.focusErr = nil
	}
	return next, cmd
}

func (m Model) handleActiveKey(msg tea.KeyMsg) (Model, tea.Cmd) {
	if m.searching {
		switch msg.Type {
		case tea.KeyCtrlC:
			m.quitting = true
			return m, tea.Quit
		case tea.KeyEsc:
			m.searching = false
			m.search.Blur()
			m.search.SetValue("")
			m.clampSelection()
			m.resizeSearch()
			return m, nil
		case tea.KeyEnter:
			return m.focusSelected()
		case tea.KeyUp:
			m.move(-1)
			return m, nil
		case tea.KeyDown:
			m.move(1)
			return m, nil
		}
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		m.clampSelection()
		m.resizeSearch()
		return m, cmd
	}

	switch msg.String() {
	case "q", "esc", "ctrl+c":
		m.quitting = true
		return m, tea.Quit
	case "/":
		m.searching = true
		m.search.Focus()
		m.resizeSearch()
		return m, textinput.Blink
	case "f":
		m.cycleFilter()
		return m, nil
	case "j", "down":
		m.move(1)
		return m, nil
	case "k", "up":
		m.move(-1)
		return m, nil
	case "g", "home":
		m.selected = 0
		return m, nil
	case "G", "end":
		m.selected = len(m.visible()) - 1
		m.clampSelection()
		return m, nil
	case "enter":
		return m.focusSelected()
	case "s":
		return m.startSession("saving", saveCmd(m.client))
	case "R":
		// Shift-only, because it creates tabs. Lowercase "r" sits close to the
		// movement keys and is easy to hit by accident.
		return m.startSession("restoring", restoreCmd(m.client))
	}

	if len(msg.Runes) == 1 && msg.Runes[0] >= '1' && msg.Runes[0] <= '9' {
		if idx := int(msg.Runes[0] - '1'); idx < len(m.visible()) {
			m.selected = idx
		}
	}
	return m, nil
}

// startSession runs a snapshot command unless one is already running, and names
// it for the footer. It leaves the reload loop alone, so the list keeps
// refreshing while kitty works.
func (m Model) startSession(verb string, cmd tea.Cmd) (Model, tea.Cmd) {
	if m.sessionBusy {
		return m, nil
	}
	m.sessionBusy = true
	m.sessionVerb = verb
	return m, cmd
}

func (m Model) focusSelected() (Model, tea.Cmd) {
	vis := m.visible()
	if m.selected < 0 || m.selected >= len(vis) {
		return m, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	m.focusErr = nil
	m.focusing = true
	m.focusCancel = cancel
	return m, focusCmd(ctx, m.client, vis[m.selected].ID)
}

func (m *Model) cancelFocus() {
	if m.focusCancel != nil {
		m.focusCancel()
		m.focusCancel = nil
	}
}

func (m *Model) move(delta int) {
	n := len(m.visible())
	if n == 0 {
		return
	}
	m.selected = (m.selected + delta + n) % n
}

func (m *Model) clampSelection() {
	n := len(m.visible())
	switch {
	case n == 0:
		m.selected = 0
	case m.selected >= n:
		m.selected = n - 1
	case m.selected < 0:
		m.selected = 0
	}
}

func (m *Model) cycleFilter() {
	for i, f := range filters {
		if f == m.filter {
			m.filter = filters[(i+1)%len(filters)]
			m.selected = 0
			return
		}
	}
	m.filter = "all"
}

// anyWorking reports whether the inventory holds an agent whose row animates.
func (m Model) anyWorking() bool {
	for _, a := range m.agents {
		if a.Display == "working" {
			return true
		}
	}
	return false
}

// visible returns the agents passing the active filter and search query.
func (m Model) visible() []kitty.Agent {
	q := strings.ToLower(strings.TrimSpace(m.search.Value()))
	var out []kitty.Agent
	for _, a := range m.agents {
		if m.filter != "all" && a.Display != m.filter {
			continue
		}
		if q != "" && !matches(a, q) {
			continue
		}
		out = append(out, a)
	}
	return out
}

func matches(a kitty.Agent, q string) bool {
	hay := strings.ToLower(strings.Join([]string{
		a.Display, a.Kind, a.Project, agentName(a), a.Branch, a.CWD, a.Title, a.Msg,
	}, " "))
	return strings.Contains(hay, q)
}

// groupLabel names a project group. An agent in a directory outside git is
// grouped by that folder, so only a window with no cwd has no label.
func groupLabel(a kitty.Agent) string {
	if a.Project != "" {
		return a.Project
	}
	return "unknown"
}

// rowLabel identifies an agent inside its project group, whose heading already
// names the project. The branch does that for a normal checkout and for most
// worktrees. A detached HEAD has no branch, so the row falls back to the
// worktree directory, which still separates it from its siblings.
func rowLabel(a kitty.Agent) string {
	if a.Branch != "" {
		return a.Branch
	}
	if a.Root != "" {
		return filepath.Base(a.Root)
	}
	return agentName(a)
}

// agentName is the agent's full identity, for a row with no project heading
// above it: the cwd basename, or the title when cwd is empty.
func agentName(a kitty.Agent) string {
	if a.CWD != "" {
		return filepath.Base(a.CWD)
	}
	return a.Title
}

// groupSize counts the agents sharing start's project from start onward. The
// list is sorted by project, so a group is one consecutive run.
func groupSize(agents []kitty.Agent, start int) int {
	n := 0
	for i := start; i < len(agents) && sameProject(agents[i], agents[start]); i++ {
		n++
	}
	return n
}

func sameProject(a, b kitty.Agent) bool {
	return a.ProjectKey == b.ProjectKey && a.Project == b.Project
}

// counts is the number of agents per status, plus "all". The keys are filter
// names, so the status tabs and the footer index it directly.
func (m Model) counts() map[string]int {
	c := map[string]int{"all": len(m.agents)}
	for _, a := range m.agents {
		c[a.Display]++
	}
	return c
}
