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

	"github.com/alexander-akhmetov/cattery/internal/agent"
	"github.com/alexander-akhmetov/cattery/internal/session"
	"github.com/alexander-akhmetov/cattery/internal/setup"
)

// filters are the status tabs, in cycle order for the "f" key.
var filters = []string{"all", "working", "blocked", "done", "idle"}

// Client is the inventory the picker lists and jumps into. An interface, so a
// test can drive the model without a live kitty or tmux.
//
// Focus takes the whole agent rather than an id, because reaching a tmux agent
// means attaching to its pane, not focusing a window.
type Client interface {
	ListAgents(context.Context) ([]agent.Agent, error)
	Focus(context.Context, agent.Agent) error

	// Preview is what one agent is showing right now, with its colours. It
	// reaches an unfocused kitty window and a detached tmux pane alike, and
	// leaves the user where they are.
	Preview(context.Context, agent.Agent) (string, error)
}

type (
	agentsMsg struct {
		generation uint64
		agents     []agent.Agent
	}
	// reloadErrMsg carries the rows the reload did produce. One host can fail
	// while the other answers, and those rows are still worth showing under the
	// warning.
	reloadErrMsg struct {
		generation uint64
		agents     []agent.Agent
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

	// previewMsg is one agent's captured screen. It carries the key as well as
	// the generation, so a result that arrives after the cursor moved is
	// dropped rather than shown under another agent's name.
	previewMsg struct {
		generation uint64
		key        string
		screen     string
		err        error
	}
	// previewDueMsg is the debounce timer. Holding "j" repeats far faster than
	// a subprocess round trip, so a selection change schedules this instead of
	// spawning at once, and only the generation that is still current fetches.
	previewDueMsg struct{ generation uint64 }
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

// previewTimeout bounds one screen capture, the budget the picker gives every
// other interactive round trip.
const previewTimeout = 2 * time.Second

// previewDebounce is how long the cursor has to settle before the sidebar
// fetches. A held movement key repeats around thirty times a second, and each
// fetch is a process.
const previewDebounce = 150 * time.Millisecond

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
	client Client

	// snapshots is the kitty half, for "s" and "R". Snapshots hold kitty tabs
	// only: a tmux agent's lifecycle belongs to whatever started it.
	snapshots session.Client

	agents           []agent.Agent
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

	// The preview sidebar, toggled with "v". previewKey is the agent screen
	// belongs to, so a result for an agent the cursor has left is discarded,
	// and previewGeneration drops a result the debounce has superseded.
	previewOpen       bool
	previewScreen     string
	previewKey        string
	previewErr        error
	previewGeneration uint64

	width    int
	height   int
	now      time.Time
	spin     int  // braille spinner frame, advanced by spinMsg
	spinning bool // a spin tick is scheduled

	quitting bool
}

// New builds a Model with an initial reload scheduled in Init. client lists and
// reaches agents of every host; snapshots is kitty alone, for save and restore.
func New(client Client, snapshots session.Client) Model {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = "search by name, path, branch, prompt…"
	in.CharLimit = 120

	return Model{
		client:           client,
		snapshots:        snapshots,
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
			return reloadErrMsg{generation: generation, agents: agents, err: err}
		}
		return agentsMsg{generation: generation, agents: agents}
	}
}

func focusCmd(ctx context.Context, client Client, a agent.Agent) tea.Cmd {
	return func() tea.Msg {
		return focusMsg{err: client.Focus(ctx, a)}
	}
}

// saveCmd snapshots the tab tree to the default snapshot path. The key is a
// shortcut; `cattery save <path>` is how to name a path.
func saveCmd(client session.Client) tea.Cmd {
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
func restoreCmd(client session.Client) tea.Cmd {
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

// previewCmd captures the screen of one agent. The key travels with the result
// so Update can tell whether the cursor is still on that agent.
func previewCmd(client Client, generation uint64, a agent.Agent) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), previewTimeout)
		defer cancel()
		screen, err := client.Preview(ctx, a)
		return previewMsg{generation: generation, key: a.Key(), screen: screen, err: err}
	}
}

func previewDue(generation uint64) tea.Cmd {
	return tea.Tick(previewDebounce, func(time.Time) tea.Msg {
		return previewDueMsg{generation: generation}
	})
}

// schedulePreview starts the debounce for whatever is under the cursor now. It
// drops the text of the agent the cursor came from, so the sidebar never shows
// one agent's screen under another's name.
func (m *Model) schedulePreview() tea.Cmd {
	if !m.previewOpen || !previewFits(m.width) {
		return nil
	}
	m.previewGeneration++
	m.previewScreen = ""
	m.previewKey = ""
	m.previewErr = nil
	return previewDue(m.previewGeneration)
}

// refreshPreview asks again for the agent already on display, without clearing
// it. The reload tick calls this, so a working agent's sidebar keeps up without
// blinking through an empty frame every second.
func (m *Model) refreshPreview() tea.Cmd {
	a, ok := m.selectedAgent()
	if !m.previewOpen || !ok || !previewFits(m.width) {
		return nil
	}
	m.previewGeneration++
	return previewCmd(m.client, m.previewGeneration, a)
}

// startPreview runs the capture a debounce timer was scheduled for, unless a
// newer move superseded it. That move scheduled a timer of its own.
func (m Model) startPreview(generation uint64) tea.Cmd {
	a, ok := m.selectedAgent()
	if generation != m.previewGeneration || !m.previewOpen || !ok {
		return nil
	}
	return previewCmd(m.client, m.previewGeneration, a)
}

// applyPreview takes a captured screen, if it is still the one being waited
// for. The cursor can move between a capture and its answer without the
// generation catching it, so the agent is checked by name too.
func (m Model) applyPreview(msg previewMsg) Model {
	if msg.generation != m.previewGeneration || !m.previewOpen || msg.key != m.selectedKey() {
		return m
	}
	m.previewKey = msg.key
	m.previewErr = msg.err
	if msg.err == nil {
		m.previewScreen = msg.screen
	}
	return m
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
		// The sidebar refreshes on the same tick as the list, so a working
		// agent is watched live rather than sampled when it was opened. It does
		// not wait on the list: a reload still in flight is no reason to leave
		// the screen a second stale.
		preview := m.refreshPreview()
		if m.loading {
			return m, tea.Batch(preview, tick())
		}
		m.loading = true
		m.reloadGeneration++
		return m, tea.Batch(m.reload(m.reloadGeneration), preview, tick())
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
		before := m.selectedKey()
		m.replaceAgents(msg.agents)
		m.resizeSearch()
		m.loaded = true
		m.loading = false
		m.reloadErr = nil
		var cmds []tea.Cmd
		// The previewed agent can end between two reloads. Its screen goes with
		// it, rather than sitting under whichever row took its place.
		if m.selectedKey() != before {
			cmds = append(cmds, m.schedulePreview())
		}
		if !m.spinning && m.anyWorking() {
			m.spinning = true
			cmds = append(cmds, spin())
		}
		return m, tea.Batch(cmds...)
	case reloadErrMsg:
		if msg.generation != m.reloadGeneration {
			return m, nil
		}
		// One source can fail while the other answers. Its rows go up, and the
		// warning above them says the list is incomplete.
		if len(msg.agents) > 0 {
			m.replaceAgents(msg.agents)
			m.resizeSearch()
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
	case previewDueMsg:
		return m, m.startPreview(msg.generation)
	case previewMsg:
		return m.applyPreview(msg), nil
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

// selectedAgent is the agent under the cursor, and false when the list is
// empty or the filter hides every row.
func (m Model) selectedAgent() (agent.Agent, bool) {
	vis := m.visible()
	if m.selected < 0 || m.selected >= len(vis) {
		return agent.Agent{}, false
	}
	return vis[m.selected], true
}

// selectedKey identifies the agent under the cursor, or "" when nothing is
// visible. The key survives filter, search, and inventory changes; a row index
// does not. It carries the host, because a kitty window id and a tmux pane id
// are both small integers and collide.
func (m Model) selectedKey() string {
	a, ok := m.selectedAgent()
	if !ok {
		return ""
	}
	return a.Key()
}

func (m *Model) replaceAgents(agents []agent.Agent) {
	selected := m.selectedKey()

	m.agents = agents
	if selected != "" {
		for i, a := range m.visible() {
			if a.Key() == selected {
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

	before := m.selectedKey()
	next, cmd := m.handleActiveKey(msg)
	// Every key that moves the cursor lands here, so the sidebar follows the
	// selection from one place rather than from each movement handler.
	if next.selectedKey() != before {
		next.focusErr = nil
		if preview := next.schedulePreview(); preview != nil {
			cmd = tea.Batch(cmd, preview)
		}
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
	case "v":
		return m.togglePreview()
	case "s":
		return m.startSession("saving", saveCmd(m.snapshots))
	case "R":
		// Shift-only, because it creates tabs. Lowercase "r" sits close to the
		// movement keys and is easy to hit by accident.
		return m.startSession("restoring", restoreCmd(m.snapshots))
	}

	if len(msg.Runes) == 1 && msg.Runes[0] >= '1' && msg.Runes[0] <= '9' {
		if idx := int(msg.Runes[0] - '1'); idx < len(m.visible()) {
			m.selected = idx
		}
	}
	return m, nil
}

// togglePreview opens or closes the sidebar.
//
// A terminal too narrow to hold both the list and a readable column of screen
// refuses, rather than squeezing the rows down to nothing. It reports that
// through the notice line, where "s" and "R" report themselves.
func (m Model) togglePreview() (Model, tea.Cmd) {
	if m.previewOpen {
		m.previewOpen = false
		m.previewScreen, m.previewKey, m.previewErr = "", "", nil
		// Strand any capture already on its way back.
		m.previewGeneration++
		return m, nil
	}
	if !previewFits(m.width) {
		m.noticeID++
		m.notice, m.noticeLevel = "terminal too narrow for the preview", noticeErr
		return m, expireNotice(m.noticeID)
	}
	m.previewOpen = true
	return m, m.schedulePreview()
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
	a, ok := m.selectedAgent()
	if !ok {
		return m, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	m.focusErr = nil
	m.focusing = true
	m.focusCancel = cancel
	return m, focusCmd(ctx, m.client, a)
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
func (m Model) visible() []agent.Agent {
	q := strings.ToLower(strings.TrimSpace(m.search.Value()))
	var out []agent.Agent
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

func matches(a agent.Agent, q string) bool {
	hay := strings.ToLower(strings.Join([]string{
		a.Display, a.Kind, a.Project, agentName(a), a.Branch, a.CWD, a.Title, a.Msg,
	}, " "))
	return strings.Contains(hay, q)
}

// groupLabel names a project group. An agent in a directory outside git is
// grouped by that folder, so only a window with no cwd has no label.
func groupLabel(a agent.Agent) string {
	if a.Project != "" {
		return a.Project
	}
	return "unknown"
}

// rowLabel identifies an agent inside its project group, whose heading already
// names the project. The branch does that for a normal checkout and for most
// worktrees. A detached HEAD has no branch, so the row falls back to the
// worktree directory, which still separates it from its siblings.
func rowLabel(a agent.Agent) string {
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
func agentName(a agent.Agent) string {
	if a.CWD != "" {
		return filepath.Base(a.CWD)
	}
	return a.Title
}

// groupSize counts the agents sharing start's project from start onward. The
// list is sorted by project, so a group is one consecutive run.
func groupSize(agents []agent.Agent, start int) int {
	n := 0
	for i := start; i < len(agents) && sameProject(agents[i], agents[start]); i++ {
		n++
	}
	return n
}

func sameProject(a, b agent.Agent) bool {
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
