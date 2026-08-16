// Package state publishes agent state to kitty as AGENT_* user variables. It
// backs `cattery state <working|blocked|idle|clear> [--kind <claude|codex>]`.
// Claude Code runs it from five hooks (SessionStart, Notification,
// UserPromptSubmit, Stop, SessionEnd) and Codex from five of its own
// (SessionStart, PermissionRequest, UserPromptSubmit, Stop, SessionEnd), and
// shell wrappers run `clear` after any agent process returns, which catches
// crashes and force-quits the agent's own cleanup never saw.
//
// Nothing here fails loudly. A hook error would surface in the agent's
// transcript, so every path returns and the command exits 0.
package state

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alexander-akhmetov/cattery/internal/kitty"
	"github.com/alexander-akhmetov/cattery/internal/shellquote"
)

// The user-variable contract, shared with the watcher, the tab bar, the picker,
// and every running pi, Claude and Codex session. Any other program that
// writes these names works too.
const (
	varKind   = "AGENT_KIND"
	varState  = "AGENT_STATE"
	varMsg    = "AGENT_MSG"
	varResume = "AGENT_RESUME"

	kindClaude = "claude"
	kindCodex  = "codex"
)

// The command AGENT_RESUME starts with. The default, the agent's own name, is
// wrong for anyone who reaches it through a wrapper, such as a sandbox or a
// profile that moves CLAUDE_CONFIG_DIR: a session id resolves only under the
// configuration directory that created it.
//
// A per-agent name wins over the shared one, which now has three claimants.
// The three compose different arguments onto the prefix: Claude takes
// "--resume <id>", Codex "resume <id>", and pi's writer "--session <file>". So
// an exported CATTERY_RESUME_PREFIX="nono run claude" would make every pi and
// Codex session in that shell publish a Claude command aimed at another
// agent's session.
const (
	envResumePrefix       = "CATTERY_RESUME_PREFIX"
	envResumePrefixClaude = "CATTERY_RESUME_PREFIX_CLAUDE"
	envResumePrefixCodex  = "CATTERY_RESUME_PREFIX_CODEX"
)

// kind is one agent this writer speaks for. The agents differ in two things:
// the word they publish as AGENT_KIND, and where the session id sits in the
// resume command.
type kind struct {
	name string
	// resumeArg goes between the prefix and the session id. Codex takes the id
	// as a positional argument of a "resume" subcommand, Claude after a
	// "--resume" flag.
	resumeArg string
	prefixEnv string
}

var kinds = map[string]kind{
	kindClaude: {name: kindClaude, resumeArg: "--resume", prefixEnv: envResumePrefixClaude},
	kindCodex:  {name: kindCodex, resumeArg: "resume", prefixEnv: envResumePrefixCodex},
}

// resolveKind reads a --kind value, falling back to Claude. An unknown word
// must not reach AGENT_KIND: the picker draws that value in the row's chip as
// it stands, so a typo would put itself on the row. Claude is the fallback
// because the bare `cattery state <word>` form is what every install predating
// the flag runs.
func resolveKind(name string) kind {
	if k, ok := kinds[name]; ok {
		return k
	}
	return kinds[kindClaude]
}

// The SessionStart sources cattery reads. Claude fires that hook for five.
// startup, resume and clear open a session waiting for a prompt. compact fires
// in the middle of a turn, where an idle would move the tab marker from working
// to done and fire a "finished" banner over a running agent.
//
// fork is both. The /fork background copy runs mid-turn, in the same window,
// while `--fork-session` and /branch open a session waiting for a prompt. The
// payload does not say which, so the whole word is excluded: a forked session
// gets no picker row until its first prompt, which costs less than marking a
// running agent finished. Claude before 2.1.214 called a fork "resume", so on
// those versions a /fork copy passes both guards.
//
// The settings.json matcher keeps compact and fork away; these cover an older
// Claude and a settings.json the user edited.
//
// Codex fires the same hook for four sources, startup, resume, clear and
// compact, and has no fork. So these two lists cover it as they stand.
var (
	startSources   = []string{"startup", "resume", "clear"}
	midTurnSources = []string{"compact", "fork"}
)

// promptLimit caps AGENT_MSG. The picker draws it on one line beside a spinner,
// and the row already names the cwd, branch, and agent kind.
const promptLimit = 200

// publishTimeout bounds the remote-control fallback. A hook waits for this
// command, so a stalled kitty socket must not hold up the agent.
const publishTimeout = 2 * time.Second

// ttyDevice is the controlling terminal, where the OSC escape has to go. A
// constant, so a test can point a transport at a file it can read back.
const ttyDevice = "/dev/tty"

// Var is one user-variable update. Delete carries no value: an OSC SetUserVar
// without one removes the variable, and so does `kitten @ set-user-vars` given
// only a name.
type Var struct {
	Name   string
	Value  string
	Delete bool
}

// Transport publishes an ordered batch of updates to one kitty window.
type Transport interface {
	Publish(vars []Var) error
}

// Writer publishes the AGENT_* variables of a single agent window or pane.
type Writer struct {
	// Kind is the agent this writer speaks for. Empty, or a word no kind
	// carries, is Claude.
	Kind string

	// WindowID is KITTY_WINDOW_ID, which the kitty transports match on. It says
	// nothing about where the state goes: New picks tmux over kitty.
	WindowID string

	// Stdin carries the agent's hook payload. A character device is never read,
	// so a manual run in a terminal does not hang waiting for EOF.
	Stdin io.Reader

	// Transport is nil when there is no route to a host. The writer then does
	// nothing: the agent could not act on the failure anyway.
	Transport Transport

	// ResumePrefix is what AGENT_RESUME starts with, before the resume argument
	// and the session id. Empty means the kind's own name.
	ResumePrefix string
}

// Run is the `cattery state <x> [--kind <claude|codex>]` entry point.
func Run(args []string) {
	state, kindName := parseArgs(args)
	if state == "" {
		return
	}
	New(kindName).Write(state)
}

// parseArgs reads the state word and the --kind value out of the argv. Written
// out rather than handed to the flag package, which stops at the first
// operand: the state word stands in front of the flag, so flag.Parse would see
// no flags at all. This argv is written by `cattery setup` and by the Codex
// plugin manifest, never typed.
func parseArgs(args []string) (state, kindName string) {
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--kind" && i+1 < len(args):
			i++
			kindName = args[i]
		case strings.HasPrefix(arg, "--kind="):
			kindName = strings.TrimPrefix(arg, "--kind=")
		case state == "":
			state = arg
		}
	}
	return state, kindName
}

// New builds the writer for this process from its environment.
func New(kindName string) Writer {
	k := resolveKind(kindName)
	w := Writer{
		Kind:         k.name,
		WindowID:     os.Getenv("KITTY_WINDOW_ID"),
		Stdin:        os.Stdin,
		ResumePrefix: resumePrefix(k),
	}
	// tmux first, and the kitty window id is not even consulted. A tmux server
	// inherits the environment of whatever started it, so every pane under a
	// server that dev launched from kitty carries that window's
	// KITTY_WINDOW_ID. Publishing there would move an unrelated window's tab
	// marker, and the pane the agent runs in would stay blank.
	if os.Getenv("TMUX") != "" {
		if pane := os.Getenv("TMUX_PANE"); pane != "" {
			w.Transport = tmuxTransport{tmux: "tmux", pane: pane}
			return w
		}
	}
	if w.WindowID == "" {
		return w
	}
	// The terminal first. It reaches kitty even when stdout is a pipe, which it
	// is under Claude: Claude captures hook stdout into the transcript.
	transports := chain{ttyTransport{path: ttyDevice}}
	// A Claude command hook runs without a controlling terminal (checked on
	// Claude Code v2.1.139), so the kitten socket is its only path.
	if id, err := strconv.Atoi(w.WindowID); err == nil && os.Getenv("KITTY_LISTEN_ON") != "" {
		transports = append(transports, kittenTransport{client: kitty.NewClient(), id: id})
	}
	w.Transport = transports
	return w
}

// Write publishes the updates the named state calls for. An unknown word and a
// missing transport both mean "publish nothing". No transport is what a caller
// outside both kitty and tmux gets.
func (w Writer) Write(state string) {
	switch state {
	case "working", "blocked", "idle", "clear":
	default:
		return
	}
	if w.Transport == nil {
		return
	}

	// Forget the window so the tab marker drops when the agent ends: the watcher
	// clears AGENT_DISPLAY once AGENT_STATE is gone. AGENT_KIND goes with it, so
	// a reused shell keeps no stale tag.
	//
	// This returns before the stdin read below. A fish wrapper calls
	// `cattery state clear` with whatever stdin it inherited, which can be a
	// pipe that never closes. Returning here also comes before any
	// AGENT_KIND=claude write, so a pi or shell caller leaves untagged.
	//
	// AGENT_RESUME survives, because `cattery save` reads it off the window long
	// after the agent is gone.
	if state == "clear" {
		_ = w.Transport.Publish([]Var{
			{Name: varKind, Delete: true},
			{Name: varMsg, Delete: true},
			{Name: varState, Delete: true},
		})
		return
	}

	// Each hook invocation is a fresh process and cannot tell a first call from
	// a later one. The write is one short escape sequence, so resend it always.
	payload := parseHook(readHook(w.Stdin))
	// Both SessionStart and Stop run `cattery state idle`, and of the hooks
	// cattery installs only SessionStart sends a source. An unrecognised source
	// reads as Stop on purpose: were Claude ever to put one on Stop, taking it
	// for a session start would drop the done marker with nothing said.
	sessionStart := false
	if state == "idle" {
		switch {
		case slices.Contains(midTurnSources, payload.Source):
			return
		case slices.Contains(startSources, payload.Source):
			sessionStart = true
		}
	}

	vars := []Var{{Name: varKind, Value: w.kind().name}}
	// Every hook payload carries session_id, so all three live states publish
	// the resume command.
	if payload.SessionID != "" {
		vars = append(vars, Var{Name: varResume, Value: w.resumeCommand(payload.SessionID)})
	}
	// UserPromptSubmit maps to "working" and carries the prompt. Every new
	// prompt overwrites the last, because the picker draws this beside a live
	// spinner and has to show the current request.
	if state == "working" {
		if prompt := normalizePrompt(payload.Prompt); prompt != "" {
			vars = append(vars, Var{Name: varMsg, Value: prompt})
		}
	}
	// A session start drops what the agent before it left in the window or the
	// pane. A Claude killed without SessionEnd leaves both standing: the prompt
	// would sit on the new row, and the "has worked" flag turns this opening
	// idle into a "done" the moment the picker reads it.
	//
	// AGENT_WORKED is a tmux option rather than part of the shared contract. The
	// delete goes in the batch instead of behind a check for the host because
	// the kitty watcher never reads that name and kitty drops a variable it
	// never set. It buys nothing there: what the watcher works from is
	// AGENT_DISPLAY, which it wrote itself and this batch does not touch, so a
	// session started in an unfocused window whose last agent was killed still
	// reads as done once.
	if sessionStart {
		vars = append(vars,
			Var{Name: varMsg, Delete: true},
			Var{Name: varWorked, Delete: true},
		)
	}
	// AGENT_STATE goes last in the batch. Writing it is what wakes the kitty
	// watcher, and anything written after it is missing from the transition the
	// watcher publishes to its subscribers.
	vars = append(vars, Var{Name: varState, Value: state})
	_ = w.Transport.Publish(vars)
}

// kind is the agent this writer speaks for.
func (w Writer) kind() kind { return resolveKind(w.Kind) }

// resumeCommand reopens this agent's session. `cattery restore` types it at
// the prompt of the restored window.
//
// Only the session id is quoted. The prefix is a raw command fragment: an
// override adds words the writer cannot guess ("nono run claude --profile
// personal"), and quoting would turn those into one filename.
func (w Writer) resumeCommand(sessionID string) string {
	k := w.kind()
	prefix := w.ResumePrefix
	if prefix == "" {
		prefix = k.name
	}
	return prefix + " " + k.resumeArg + " " + shellquote.Quote(sessionID)
}

// resumePrefix reads the override, the agent's own name first. An empty value
// counts as unset: an exported-but-cleared variable would otherwise publish a
// command with no program in front of it.
func resumePrefix(k kind) string {
	if prefix := os.Getenv(k.prefixEnv); prefix != "" {
		return prefix
	}
	return os.Getenv(envResumePrefix)
}

// readHook reads the agent's hook payload from stdin. It skips a character
// device, because reading a terminal would block a manual run until ctrl-d. A
// failed stat cannot rule out a terminal, so it skips too.
func readHook(r io.Reader) []byte {
	if r == nil {
		return nil
	}
	if f, ok := r.(interface{ Stat() (fs.FileInfo, error) }); ok {
		info, err := f.Stat()
		if err != nil || info.Mode()&fs.ModeCharDevice != 0 {
			return nil
		}
	}
	data, _ := io.ReadAll(r)
	return data
}

// hookPayload is the part of an agent hook payload the writer reads. Claude
// sends session_id on every hook, prompt only on UserPromptSubmit, and source
// only on SessionStart.
type hookPayload struct {
	Prompt    string `json:"prompt"`
	SessionID string `json:"session_id"`
	Source    string `json:"source"`
}

// parseHook decodes the payload, or returns an empty one. A payload that is not
// JSON publishes no prompt and no resume command.
func parseHook(data []byte) hookPayload {
	var payload hookPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return hookPayload{}
	}
	return payload
}

// normalizePrompt folds the prompt to one line of at most promptLimit runes.
// Cutting on runes cannot leave a partial UTF-8 encoding. `kitten @ ls` returns
// JSON, and invalid bytes there would break the picker's parse.
func normalizePrompt(raw string) string {
	prompt := strings.Join(strings.Fields(raw), " ")
	if utf8.RuneCountInString(prompt) <= promptLimit {
		return prompt
	}
	return string([]rune(prompt)[:promptLimit])
}

// --- transports -------------------------------------------------------------

// chain publishes through the first transport that accepts the batch.
type chain []Transport

func (c chain) Publish(vars []Var) error {
	var err error
	for _, t := range c {
		if err = t.Publish(vars); err == nil {
			return nil
		}
	}
	return err
}

// ttyTransport writes OSC escapes to the controlling terminal. The sequence
// cannot go to stdout, because Claude captures hook stdout into the transcript.
// Without a controlling terminal the open fails and the chain moves on.
type ttyTransport struct{ path string }

func (t ttyTransport) Publish(vars []Var) error {
	tty, err := os.OpenFile(t.path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer tty.Close()
	var b strings.Builder
	for _, v := range vars {
		b.WriteString(osc(v))
	}
	_, err = io.WriteString(tty, b.String())
	return err
}

// osc renders one update as kitty's OSC 1337;SetUserVar sequence. The value is
// base64, and omitting it deletes the variable.
func osc(v Var) string {
	if v.Delete {
		return "\x1b]1337;SetUserVar=" + v.Name + "\a"
	}
	return "\x1b]1337;SetUserVar=" + v.Name + "=" + base64.StdEncoding.EncodeToString([]byte(v.Value)) + "\a"
}

// kittenTransport publishes over kitty remote control. It matches by window id,
// so the values reach the window the agent runs in even when another window is
// active. It needs allow_remote_control and listen_on in kitty.conf.
type kittenTransport struct {
	client *kitty.Client
	id     int
}

func (k kittenTransport) Publish(vars []Var) error {
	args := make([]string, 0, len(vars))
	for _, v := range vars {
		if v.Delete {
			args = append(args, v.Name)
			continue
		}
		args = append(args, v.Name+"="+v.Value)
	}
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	return k.client.SetUserVars(ctx, k.id, args)
}
