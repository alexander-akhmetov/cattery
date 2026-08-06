// Package state publishes agent state to kitty as AGENT_* user variables. It
// backs `cattery state <working|blocked|idle|clear>`, which Claude Code runs
// from its hooks (Notification -> blocked, UserPromptSubmit -> working, Stop ->
// idle, SessionEnd -> clear) and shell wrappers run as `clear` after any agent
// process returns, which catches crashes and force-quits that never reached the
// agent's own cleanup.
//
// Nothing here fails loudly. A hook that reports an error would surface it in
// the agent's transcript, and a state write is not worth that, so every path
// returns and the command exits 0.
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
)

// The user-variable contract, shared with the watcher, the tab bar, the picker,
// and every running pi and Claude session. The names are AGENT_*, not CATTERY_*,
// so any other program that writes them works too.
const (
	varKind  = "AGENT_KIND"
	varState = "AGENT_STATE"
	varMsg   = "AGENT_MSG"

	kindClaude = "claude"
)

// promptLimit caps AGENT_MSG. The picker draws it on one line beside a spinner,
// and the row already names the cwd, branch, and agent kind.
const promptLimit = 200

// publishTimeout bounds the remote-control fallback. A hook waits for this
// command, so a stalled kitty socket must not hold up the agent.
const publishTimeout = 2 * time.Second

// ttyDevice is the controlling terminal, which is where the OSC escape has to
// go. It is a constant rather than a literal inside the transport so a test can
// point one at a file it can read back.
const ttyDevice = "/dev/tty"

// Var is one user-variable update. Delete carries no value: an OSC SetUserVar
// with no value removes the variable, and `kitten @ set-user-vars` removes one
// when given just its name.
type Var struct {
	Name   string
	Value  string
	Delete bool
}

// Transport publishes an ordered batch of updates to one kitty window.
type Transport interface {
	Publish(vars []Var) error
}

// Writer publishes the AGENT_* variables of a single kitty window.
type Writer struct {
	// WindowID is KITTY_WINDOW_ID. Empty means the caller is not inside kitty
	// and there is nothing to publish.
	WindowID string

	// Stdin carries the agent's hook payload. A character device is never read,
	// so a manual run in a terminal does not hang waiting for EOF.
	Stdin io.Reader

	// Transport is nil when no route to kitty exists; the writer then does
	// nothing rather than reporting a failure the agent cannot act on.
	Transport Transport
}

// Run is the `cattery state <x>` entry point.
func Run(args []string) {
	if len(args) == 0 {
		return
	}
	New().Write(args[0])
}

// New builds the writer for this process from the kitty environment.
func New() Writer {
	w := Writer{WindowID: os.Getenv("KITTY_WINDOW_ID"), Stdin: os.Stdin}
	if w.WindowID == "" {
		return w
	}
	// The terminal first: it reaches kitty even when stdout is a pipe, which it
	// is under Claude, because Claude captures hook stdout into the transcript.
	transports := chain{ttyTransport{path: ttyDevice}}
	// The kitten socket is the only path inside a Claude command hook, which
	// runs without a controlling terminal (checked on Claude Code v2.1.139).
	if id, err := strconv.Atoi(w.WindowID); err == nil && os.Getenv("KITTY_LISTEN_ON") != "" {
		transports = append(transports, kittenTransport{client: kitty.NewClient(), id: id})
	}
	w.Transport = transports
	return w
}

// Write publishes the updates the named state calls for. An unknown word, a
// caller outside kitty, and a missing transport all mean "publish nothing".
func (w Writer) Write(state string) {
	switch state {
	case "working", "blocked", "idle", "clear":
	default:
		return
	}
	if w.WindowID == "" || w.Transport == nil {
		return
	}

	// Forget the window entirely so the tab-bar glyph drops when the agent ends:
	// the watcher clears AGENT_DISPLAY once AGENT_STATE is gone. AGENT_KIND goes
	// with it, so a reused shell does not keep a stale tag.
	//
	// This returns before the stdin read below. A fish wrapper calls
	// `cattery state clear` with whatever stdin it inherited, which can be an
	// open pipe that never produces data; reading it would hang the shell after
	// every agent exit. Deleting also comes before any AGENT_KIND=claude write,
	// so a pi or shell caller is not tagged as claude on its way out.
	if state == "clear" {
		_ = w.Transport.Publish([]Var{
			{Name: varState, Delete: true},
			{Name: varKind, Delete: true},
			{Name: varMsg, Delete: true},
		})
		return
	}

	// Each hook invocation is a fresh process, so nothing here can tell a first
	// call from a later one without storing state outside it. The write is one
	// short escape sequence, so resend it every time.
	vars := []Var{
		{Name: varKind, Value: kindClaude},
		{Name: varState, Value: state},
	}
	// UserPromptSubmit maps to "working", and carries the prompt. Every new
	// prompt overwrites the last: the picker draws this beside a live spinner,
	// so in a long session the opening request is the wrong answer to "what is
	// this agent doing".
	if state == "working" {
		if prompt := normalizePrompt(readHook(w.Stdin)); prompt != "" {
			vars = append(vars, Var{Name: varMsg, Value: prompt})
		}
	}
	_ = w.Transport.Publish(vars)
}

// readHook reads the agent's hook payload from stdin. A character device is
// skipped, because reading a terminal would block a manual run until the user
// pressed ctrl-d. A stat that fails cannot rule out a terminal, so it skips too.
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

// normalizePrompt pulls .prompt out of the hook payload and folds it to one
// line of at most promptLimit runes. Cutting on runes cannot leave a partial
// UTF-8 encoding, which matters because `kitten @ ls` returns JSON and invalid
// bytes there would break the picker's parse.
func normalizePrompt(hook []byte) string {
	var payload struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(hook, &payload); err != nil {
		return ""
	}
	prompt := strings.Join(strings.Fields(payload.Prompt), " ")
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

// ttyTransport writes OSC escapes to the controlling terminal. Claude captures
// hook stdout into the transcript, so the sequence must go to the terminal
// device rather than stdout; without a controlling terminal the open fails and
// the chain moves on.
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

// kittenTransport publishes over kitty remote control, matching by window id so
// the values land on the window the agent runs in, not the active one. It needs
// allow_remote_control and listen_on in kitty.conf.
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
