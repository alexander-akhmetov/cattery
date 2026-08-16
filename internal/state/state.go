// Package state publishes agent state to kitty as AGENT_* user variables. It
// backs `cattery state <working|blocked|idle|clear>`. Claude Code runs it from
// four hooks (Notification, UserPromptSubmit, Stop, SessionEnd), and shell
// wrappers run `clear` after any agent process returns, which catches crashes
// and force-quits the agent's own cleanup never saw.
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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alexander-akhmetov/cattery/internal/kitty"
	"github.com/alexander-akhmetov/cattery/internal/shellquote"
)

// The user-variable contract, shared with the watcher, the tab bar, the picker,
// and every running pi and Claude session. Any other program that writes these
// names works too.
const (
	varKind   = "AGENT_KIND"
	varState  = "AGENT_STATE"
	varMsg    = "AGENT_MSG"
	varResume = "AGENT_RESUME"

	kindClaude = "claude"
)

// The command AGENT_RESUME starts with. The default "claude" is wrong for
// anyone who reaches Claude through a wrapper, such as a sandbox or a profile
// that moves CLAUDE_CONFIG_DIR: a session id resolves only under the
// configuration directory that created it.
//
// The Claude-only name wins over the shared one. pi's writer appends
// "--session <file>" to the same prefix, so an exported
// CATTERY_RESUME_PREFIX="nono run claude" would make every pi session in that
// shell publish a Claude command aimed at a pi transcript.
const (
	envResumePrefix       = "CATTERY_RESUME_PREFIX"
	envResumePrefixClaude = "CATTERY_RESUME_PREFIX_CLAUDE"
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
	// WindowID is KITTY_WINDOW_ID, which the kitty transports match on. It says
	// nothing about where the state goes: New picks tmux over kitty.
	WindowID string

	// Stdin carries the agent's hook payload. A character device is never read,
	// so a manual run in a terminal does not hang waiting for EOF.
	Stdin io.Reader

	// Transport is nil when there is no route to a host. The writer then does
	// nothing: the agent could not act on the failure anyway.
	Transport Transport

	// ResumePrefix is what AGENT_RESUME starts with, before "--resume <id>".
	// Empty means "claude".
	ResumePrefix string
}

// Run is the `cattery state <x>` entry point.
func Run(args []string) {
	if len(args) == 0 {
		return
	}
	New().Write(args[0])
}

// New builds the writer for this process from its environment.
func New() Writer {
	w := Writer{
		WindowID:     os.Getenv("KITTY_WINDOW_ID"),
		Stdin:        os.Stdin,
		ResumePrefix: resumePrefix(),
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
	vars := []Var{{Name: varKind, Value: kindClaude}}
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
	// AGENT_STATE goes last in the batch. Writing it is what wakes the kitty
	// watcher, and anything written after it is missing from the transition the
	// watcher publishes to its subscribers.
	vars = append(vars, Var{Name: varState, Value: state})
	_ = w.Transport.Publish(vars)
}

// resumeCommand reopens this Claude session. `cattery restore` types it at the
// prompt of the restored window.
//
// Only the session id is quoted. The prefix is a raw command fragment: an
// override adds words the writer cannot guess ("nono run claude --profile
// personal"), and quoting would turn those into one filename.
func (w Writer) resumeCommand(sessionID string) string {
	prefix := w.ResumePrefix
	if prefix == "" {
		prefix = kindClaude
	}
	return prefix + " --resume " + shellquote.Quote(sessionID)
}

// resumePrefix reads the override, Claude's own name first. An empty value
// counts as unset: an exported-but-cleared variable would otherwise publish a
// command with no program in front of it.
func resumePrefix() string {
	if prefix := os.Getenv(envResumePrefixClaude); prefix != "" {
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
// sends session_id on every hook and prompt only on UserPromptSubmit.
type hookPayload struct {
	Prompt    string `json:"prompt"`
	SessionID string `json:"session_id"`
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
