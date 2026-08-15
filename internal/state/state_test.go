package state

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alexander-akhmetov/cattery/internal/kitty"
)

// recorder stands in for a kitty window. It keeps the batches a writer sent, so
// a test can check the update order without a terminal or a live kitty.
type recorder struct {
	batches [][]Var
	err     error
}

func (r *recorder) Publish(vars []Var) error {
	r.batches = append(r.batches, vars)
	return r.err
}

func (r *recorder) sent() []Var {
	if len(r.batches) == 0 {
		return nil
	}
	var out []Var
	for _, batch := range r.batches {
		out = append(out, batch...)
	}
	return out
}

// charDevice is stdin attached to a terminal. It fails the test when anything
// reads it, which a manual `cattery state idle` must never do.
type charDevice struct{ t *testing.T }

func (c charDevice) Read([]byte) (int, error) {
	c.t.Fatal("read a character-device stdin, which would hang a manual run")
	return 0, io.EOF
}

func (charDevice) Stat() (fs.FileInfo, error) { return charDeviceInfo{}, nil }

type charDeviceInfo struct{ fs.FileInfo }

func (charDeviceInfo) Mode() fs.FileMode { return fs.ModeCharDevice | 0o620 }

// blockingPipe is the open pipe a fish wrapper can hand to `cattery state
// clear`. It produces no data and never closes.
type blockingPipe struct{}

func (blockingPipe) Read([]byte) (int, error) {
	select {}
}

func set(name, value string) Var { return Var{Name: name, Value: value} }
func del(name string) Var        { return Var{Name: name, Delete: true} }

func TestWriteUpdateOrder(t *testing.T) {
	cases := []struct {
		name  string
		state string
		stdin io.Reader
		want  []Var
	}{
		{
			name:  "working publishes kind, state, then the prompt",
			state: "working",
			stdin: strings.NewReader(`{"prompt":"fix the picker"}`),
			want:  []Var{set(varKind, "claude"), set(varState, "working"), set(varMsg, "fix the picker")},
		},
		{
			name:  "working without a prompt leaves AGENT_MSG alone",
			state: "working",
			stdin: strings.NewReader(`{"session_id":"abc"}`),
			want:  []Var{set(varKind, "claude"), set(varState, "working"), set(varResume, "claude --resume abc")},
		},
		{
			name:  "working with no stdin at all",
			state: "working",
			want:  []Var{set(varKind, "claude"), set(varState, "working")},
		},
		{
			name:  "working never reads a terminal",
			state: "working",
			stdin: charDevice{t: t},
			want:  []Var{set(varKind, "claude"), set(varState, "working")},
		},
		{
			name:  "blocked",
			state: "blocked",
			stdin: strings.NewReader(`{"prompt":"ignored"}`),
			want:  []Var{set(varKind, "claude"), set(varState, "blocked")},
		},
		{
			name:  "idle",
			state: "idle",
			stdin: strings.NewReader(`{"prompt":"ignored"}`),
			want:  []Var{set(varKind, "claude"), set(varState, "idle")},
		},
		{
			name:  "idle from a terminal",
			state: "idle",
			stdin: charDevice{t: t},
			want:  []Var{set(varKind, "claude"), set(varState, "idle")},
		},
		{
			name:  "clear deletes state, kind, then message",
			state: "clear",
			want:  []Var{del(varState), del(varKind), del(varMsg)},
		},
		{
			name:  "unknown word publishes nothing",
			state: "unknown",
			stdin: strings.NewReader(`{"prompt":"fix the picker"}`),
		},
		{
			name:  "empty word publishes nothing",
			state: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			Writer{WindowID: "7", Stdin: tc.stdin, Transport: rec}.Write(tc.state)
			assertVars(t, rec.sent(), tc.want)
		})
	}
}

// A crashed agent's cleanup runs through a shell wrapper, which passes on
// whatever stdin it inherited. Reading that would hang the shell after every
// agent exit, so clear publishes and returns without touching it.
func TestClearReturnsBeforeReadingStdin(t *testing.T) {
	rec := &recorder{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		Writer{WindowID: "7", Stdin: blockingPipe{}, Transport: rec}.Write("clear")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("clear blocked on stdin")
	}
	assertVars(t, rec.sent(), []Var{del(varState), del(varKind), del(varMsg)})
}

// A caller in neither kitty nor tmux gets no transport from New, and the writer
// is the last place that can notice: every state word arrives from a hook that
// cannot report a failure. The window id is not the test, because a tmux pane
// can carry one belonging to another window.
func TestWriteWithoutAWayToPublish(_ *testing.T) {
	writer := Writer{WindowID: "7", Stdin: strings.NewReader(`{"prompt":"x"}`)}
	for _, state := range []string{"working", "blocked", "idle", "clear"} {
		writer.Write(state)
	}
}

// A pane publishes with no kitty window id at all, which is what a tmux server
// started outside kitty gives every agent under it.
func TestWritePublishesWithoutAKittyWindow(t *testing.T) {
	rec := &recorder{}
	Writer{Stdin: strings.NewReader(`{"prompt":"fix the picker"}`), Transport: rec}.Write("working")
	assertVars(t, rec.sent(), []Var{set(varKind, "claude"), set(varState, "working"), set(varMsg, "fix the picker")})
}

// AGENT_RESUME tells a snapshot how to reopen this Claude session. Every hook
// payload carries session_id, so every live state publishes it.
func TestWriteResumeCommand(t *testing.T) {
	cases := []struct {
		name   string
		state  string
		prefix string
		stdin  io.Reader
		want   string // the AGENT_RESUME value, or "" for no AGENT_RESUME at all
	}{
		{
			name:  "a session id becomes a resume command",
			state: "working",
			stdin: strings.NewReader(`{"session_id":"abc-123"}`),
			want:  "claude --resume abc-123",
		},
		{
			name:   "the prefix override carries the wrapper and the profile flag",
			state:  "working",
			prefix: "nono run claude --profile personal",
			stdin:  strings.NewReader(`{"session_id":"abc-123"}`),
			want:   "nono run claude --profile personal --resume abc-123",
		},
		{
			name:  "blocked publishes it too",
			state: "blocked",
			stdin: strings.NewReader(`{"session_id":"abc-123"}`),
			want:  "claude --resume abc-123",
		},
		{
			name:  "idle publishes it too",
			state: "idle",
			stdin: strings.NewReader(`{"session_id":"abc-123"}`),
			want:  "claude --resume abc-123",
		},
		{
			name:  "a payload with no session id publishes nothing",
			state: "working",
			stdin: strings.NewReader(`{"prompt":"fix the picker"}`),
		},
		{
			name:  "no payload at all publishes nothing",
			state: "working",
		},
		{
			name:  "a payload that is not json publishes nothing",
			state: "working",
			stdin: strings.NewReader(`not json`),
		},
		{
			// The id is Claude's own UUID, so this never happens in practice.
			// Quoting stays, because restore types the value at a shell prompt
			// where an unquoted space would split the command.
			name:  "an id needing quotes gets them",
			state: "working",
			stdin: strings.NewReader(`{"session_id":"weird id"}`),
			want:  "claude --resume 'weird id'",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			Writer{WindowID: "7", Stdin: tc.stdin, Transport: rec, ResumePrefix: tc.prefix}.Write(tc.state)

			got, found := "", false
			for _, v := range rec.sent() {
				if v.Name == varResume {
					got, found = v.Value, true
				}
			}
			if tc.want == "" {
				if found {
					t.Fatalf("published %s=%q, want none", varResume, got)
				}
				return
			}
			if !found {
				t.Fatalf("no %s published, want %q", varResume, tc.want)
			}
			if got != tc.want {
				t.Fatalf("%s: got %q, want %q", varResume, got, tc.want)
			}
		})
	}
}

// A shell wrapper runs clear after any agent exits, and the window can outlive
// several agents. The resume command makes that window worth restoring, so
// clear leaves it and forgets everything live.
func TestClearKeepsTheResumeCommand(t *testing.T) {
	rec := &recorder{}
	Writer{WindowID: "7", Transport: rec}.Write("clear")

	for _, v := range rec.sent() {
		if v.Name == varResume {
			t.Fatalf("clear touched %s: %+v", varResume, v)
		}
	}
	// The live variables still go, or a dead agent keeps its tab marker.
	assertVars(t, rec.sent(), []Var{del(varState), del(varKind), del(varMsg)})
}

// New reads the override from the environment, where a shell wrapper sets it.
// The Claude-only name wins, because pi's writer appends different arguments to
// the same prefix. An exported CATTERY_RESUME_PREFIX aimed at one agent would
// otherwise publish that agent's command for the other.
func TestNewReadsTheResumePrefix(t *testing.T) {
	cases := []struct {
		name         string
		shared       string
		claude       string
		want         string
		wantResumeIn string
	}{
		{
			name:         "the shared name",
			shared:       "nono run claude",
			want:         "nono run claude",
			wantResumeIn: "nono run claude --resume abc",
		},
		{
			name:         "the Claude name wins over the shared one",
			shared:       "nono run pi",
			claude:       "nono run claude --profile personal",
			want:         "nono run claude --profile personal",
			wantResumeIn: "nono run claude --profile personal --resume abc",
		},
		{
			name:         "an empty value is not an override",
			shared:       "",
			claude:       "",
			want:         "",
			wantResumeIn: "claude --resume abc",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KITTY_WINDOW_ID", "7")
			t.Setenv(envResumePrefix, tc.shared)
			t.Setenv(envResumePrefixClaude, tc.claude)

			w := New()
			if w.ResumePrefix != tc.want {
				t.Fatalf("ResumePrefix: got %q, want %q", w.ResumePrefix, tc.want)
			}
			if got := w.resumeCommand("abc"); got != tc.wantResumeIn {
				t.Fatalf("resumeCommand: got %q, want %q", got, tc.wantResumeIn)
			}
		})
	}
}

// A transport that cannot reach kitty is not the agent's problem. The writer
// swallows the error, so the hook exits 0 either way.
func TestPublishFailureIsSwallowed(t *testing.T) {
	rec := &recorder{err: errors.New("kitty socket unavailable")}
	Writer{WindowID: "7", Stdin: strings.NewReader(`{"prompt":"x"}`), Transport: rec}.Write("working")
	if len(rec.batches) != 1 {
		t.Fatalf("batches: got %d, want 1", len(rec.batches))
	}
}

func TestNormalizePrompt(t *testing.T) {
	long := strings.Repeat("a", 250)
	multibyte := strings.Repeat("日", 250)

	cases := []struct {
		name string
		hook string
		want string
	}{
		{name: "plain", hook: `{"prompt":"fix the picker"}`, want: "fix the picker"},
		{name: "line breaks fold to spaces", hook: `{"prompt":"line one\nline two"}`, want: "line one line two"},
		{name: "carriage returns and tabs fold too", hook: `{"prompt":"a\r\nb\tc"}`, want: "a b c"},
		{name: "repeated whitespace collapses", hook: `{"prompt":"line one\nline   two"}`, want: "line one line two"},
		{name: "surrounding whitespace goes", hook: `{"prompt":"  padded  "}`, want: "padded"},
		{name: "whitespace only", hook: `{"prompt":"   \n\t "}`, want: ""},
		{name: "absent prompt", hook: `{"session_id":"abc"}`, want: ""},
		{name: "empty payload", hook: "", want: ""},
		{name: "not json", hook: "hello", want: ""},
		{name: "over the limit", hook: `{"prompt":"` + long + `"}`, want: strings.Repeat("a", promptLimit)},
		{
			name: "multibyte runes are counted, not bytes",
			hook: `{"prompt":"` + multibyte + `"}`,
			want: strings.Repeat("日", promptLimit),
		},
		{
			// A two-byte rune sitting on the limit. The cut must fall between
			// runes: the picker parses `kitten @ ls` output as JSON, and a
			// partial encoding breaks that parse.
			name: "a cut lands between runes",
			hook: `{"prompt":"` + strings.Repeat("é", 300) + `"}`,
			want: strings.Repeat("é", promptLimit),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizePrompt(parseHook([]byte(tc.hook)).Prompt)
			if got != tc.want {
				t.Fatalf("normalizePrompt: got %q, want %q", got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("normalizePrompt returned invalid UTF-8: %q", got)
			}
			if n := utf8.RuneCountInString(got); n > promptLimit {
				t.Fatalf("normalizePrompt returned %d runes, max %d", n, promptLimit)
			}
		})
	}
}

func TestOSC(t *testing.T) {
	cases := []struct {
		name string
		v    Var
		want string
	}{
		{
			name: "set encodes the value as base64",
			v:    set(varState, "working"),
			want: "\x1b]1337;SetUserVar=AGENT_STATE=d29ya2luZw==\a",
		},
		{
			name: "set with spaces",
			v:    set(varMsg, "fix the picker"),
			want: "\x1b]1337;SetUserVar=AGENT_MSG=Zml4IHRoZSBwaWNrZXI=\a",
		},
		{
			// A SetUserVar with no value removes the variable.
			name: "delete carries no value",
			v:    del(varState),
			want: "\x1b]1337;SetUserVar=AGENT_STATE\a",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := osc(tc.v); got != tc.want {
				t.Fatalf("osc: got %q, want %q", got, tc.want)
			}
		})
	}
}

// The chain is what makes a Claude hook work. /dev/tty fails there, and the
// kitten socket behind it carries the state.
func TestChainFallsBackToTheNextTransport(t *testing.T) {
	failing := &recorder{err: errors.New("no controlling terminal")}
	working := &recorder{}
	vars := []Var{set(varKind, "claude")}

	if err := (chain{failing, working}).Publish(vars); err != nil {
		t.Fatalf("chain: got %v, want nil", err)
	}
	if len(failing.batches) != 1 || len(working.batches) != 1 {
		t.Fatalf("batches: first=%d second=%d, want 1 each", len(failing.batches), len(working.batches))
	}

	// The first transport that works ends the chain.
	first := &recorder{}
	second := &recorder{}
	if err := (chain{first, second}).Publish(vars); err != nil {
		t.Fatalf("chain: got %v, want nil", err)
	}
	if len(second.batches) != 0 {
		t.Fatalf("second transport ran after the first succeeded: %v", second.batches)
	}

	// Every transport failing reports the last reason. Nothing above acts on
	// it, and it keeps the chain honest.
	if err := (chain{failing, failing}).Publish(vars); err == nil {
		t.Fatal("chain with no working transport: got nil error")
	}
}

// `cattery state` with no argument must not panic on args[0].
func TestRunWithoutAState(_ *testing.T) {
	Run(nil)
	Run([]string{})
}

// --- transports -------------------------------------------------------------

// New picks the transport from the environment. tmux comes first, then the
// terminal, then the remote-control socket behind it.
func TestNewPicksTheTransport(t *testing.T) {
	cases := []struct {
		name     string
		tmux     string
		pane     string
		windowID string
		listenOn string
		// want is the chain length, or 0 for no transport at all. wantTmux
		// overrides it: the tmux transport is not a chain.
		want     int
		wantTmux string
	}{
		{name: "outside kitty and tmux"},
		{name: "inside kitty, no remote control", windowID: "7", want: 1},
		{name: "inside kitty with remote control", windowID: "7", listenOn: "unix:/tmp/kitty-1", want: 2},
		{name: "a window id that is not a number", windowID: "w7", listenOn: "unix:/tmp/kitty-1", want: 1},
		{
			// The tmux server inherits the environment of whatever started it,
			// so a detached pane can carry a kitty window id that belongs to an
			// unrelated window. The pane wins.
			name:     "a pane with a leaked kitty window id",
			tmux:     "/private/tmp/tmux-501/default,69427,0",
			pane:     "%17",
			windowID: "7",
			listenOn: "unix:/tmp/kitty-1",
			wantTmux: "%17",
		},
		{name: "a pane outside kitty", tmux: "/private/tmp/tmux-501/default,1,0", pane: "%17", wantTmux: "%17"},
		{
			// Half the pair is not enough to name a pane to publish to.
			name:     "TMUX without a pane falls back to kitty",
			tmux:     "/private/tmp/tmux-501/default,1,0",
			windowID: "7",
			want:     1,
		},
		{name: "a pane without TMUX is not a pane", pane: "%17", windowID: "7", want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMUX", tc.tmux)
			t.Setenv("TMUX_PANE", tc.pane)
			t.Setenv("KITTY_WINDOW_ID", tc.windowID)
			t.Setenv("KITTY_LISTEN_ON", tc.listenOn)

			w := New()

			if w.WindowID != tc.windowID {
				t.Errorf("window id: got %q, want %q", w.WindowID, tc.windowID)
			}
			if tc.wantTmux != "" {
				got, ok := w.Transport.(tmuxTransport)
				if !ok {
					t.Fatalf("transport: got %T, want the tmux transport", w.Transport)
				}
				if got.pane != tc.wantTmux {
					t.Fatalf("pane: got %q, want %q", got.pane, tc.wantTmux)
				}
				return
			}
			if tc.want == 0 {
				if w.Transport != nil {
					t.Fatalf("transport: got %#v, want none", w.Transport)
				}
				return
			}
			got, ok := w.Transport.(chain)
			if !ok {
				t.Fatalf("transport: got %T, want a chain", w.Transport)
			}
			if len(got) != tc.want {
				t.Fatalf("chain: got %d transports, want %d", len(got), tc.want)
			}
			if _, ok := got[0].(ttyTransport); !ok {
				t.Errorf("first transport: got %T, want the terminal", got[0])
			}
		})
	}
}

// The terminal transport sends the whole batch in one write, and reports the
// open failure every Claude command hook hits. Those hooks run without a
// controlling terminal, which is what sends the batch to the kitten socket.
func TestTTYTransport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tty")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create the stand-in device: %v", err)
	}
	vars := []Var{set(varKind, "claude"), set(varState, "working"), del(varMsg)}

	if err := (ttyTransport{path: path}).Publish(vars); err != nil {
		t.Fatalf("publish: %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if want := osc(vars[0]) + osc(vars[1]) + osc(vars[2]); string(written) != want {
		t.Errorf("written:\n got %q\nwant %q", written, want)
	}

	absent := filepath.Join(t.TempDir(), "no-terminal-here")
	if err := (ttyTransport{path: absent}).Publish(vars); err == nil {
		t.Error("publishing to a device that is not there: got a nil error")
	}
}

// A kitten that cannot reach kitty must say so, or the chain counts the update
// as published and stops.
func TestKittenTransportReportsAFailure(t *testing.T) {
	dir := t.TempDir()
	writeFakeKitten(t, dir, `printf 'no listening socket\n' >&2; exit 1`)
	t.Setenv("PATH", dir)

	err := kittenTransport{client: kitty.NewClient(), id: 7}.Publish([]Var{set(varState, "working")})

	if err == nil {
		t.Fatal("got a nil error")
	}
	for _, want := range []string{"window 7", "no listening socket"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// writeFakeKitten puts a kitten on PATH that runs body instead of talking to
// kitty.
func writeFakeKitten(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "kitten"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write the fake kitten: %v", err)
	}
}

func assertVars(t *testing.T, got, want []Var) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("updates: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("update %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// --- the tmux transport -------------------------------------------------------

// fakeTmux writes a stub tmux that records its argv one line per run, and
// answers `show` with prev. It returns the transport and a reader for the log.
func fakeTmux(t *testing.T, prev string) (tmuxTransport, func() []string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "argv")
	body := "if [ \"$1\" = show ]; then printf '%s\\n' " + strconv.Quote(prev) + "; exit 0; fi\n" +
		"printf '%s\\n' \"$*\" >> " + log + "\n"
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return tmuxTransport{tmux: path, pane: "%17"}, func() []string {
		data, err := os.ReadFile(log)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	}
}

// One process per batch, with tmux's own ";" separator between the commands,
// and "-u" for a deletion.
func TestTmuxTransportPublishesOneCommandLine(t *testing.T) {
	transport, _ := fakeTmux(t, "")
	args := transport.args([]Var{set(varKind, "claude"), set(varState, "idle"), del(varMsg)})

	want := []string{
		"set", "-p", "-t", "%17", "@AGENT_KIND", "claude", ";",
		"set", "-p", "-t", "%17", "@AGENT_STATE", "idle", ";",
		"set", "-p", "-u", "-t", "%17", "@AGENT_MSG",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("argv:\n got %v\nwant %v", args, want)
	}
}

// tmux ends a command at any argument ending in ";", so a prompt ending in one
// would lose that character, and a prompt that is only ";" would swallow every
// update chained behind it.
func TestTmuxTransportEscapesACommandTerminator(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{name: "a plain value is passed as it is", value: "run make test", want: "run make test"},
		{name: "a semicolon inside the value is not a terminator", value: "cd x; make", want: "cd x; make"},
		{name: "a trailing semicolon is escaped", value: "make test;", want: `make test\;`},
		{name: "a value that is only a semicolon", value: ";", want: `\;`},
		{name: "a backslash before the semicolon survives", value: `make\;`, want: `make\\;`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport, _ := fakeTmux(t, "")

			args := transport.args([]Var{set(varMsg, tc.value)})

			if got := args[len(args)-1]; got != tc.want {
				t.Fatalf("value: got %q, want %q", got, tc.want)
			}
		})
	}
}

// The picker derives "done" and the elapsed time from three options the writer
// keeps beside the state word.
func TestTmuxTransportMetadata(t *testing.T) {
	cases := []struct {
		name string
		prev string
		vars []Var
		// want is the option names the run has to carry, in order, with a "-u "
		// prefix for the ones it deletes.
		want []string
	}{
		{
			name: "a new state stamps the time and marks the work",
			prev: "idle",
			vars: []Var{set(varKind, "claude"), set(varState, "working")},
			want: []string{"@AGENT_KIND", "@AGENT_STATE", "@AGENT_SINCE", "@AGENT_WORKED", "-u @AGENT_SEEN"},
		},
		{
			name: "blocked marks the work too",
			prev: "working",
			vars: []Var{set(varState, "blocked")},
			want: []string{"@AGENT_STATE", "@AGENT_SINCE", "@AGENT_WORKED", "-u @AGENT_SEEN"},
		},
		{
			// Claude fires a hook per turn, and the state word often repeats.
			// Restamping would restart the picker's elapsed counter each time.
			name: "the same state again leaves the time alone",
			prev: "working",
			vars: []Var{set(varState, "working")},
			want: []string{"@AGENT_STATE", "@AGENT_WORKED", "-u @AGENT_SEEN"},
		},
		{
			// The pane has no state yet, so this is a change.
			name: "the first state of a pane is stamped",
			prev: "",
			vars: []Var{set(varState, "idle")},
			want: []string{"@AGENT_STATE", "@AGENT_SINCE"},
		},
		{
			// Going idle neither marks work nor acknowledges it: that pair is
			// what turns into "done" in the picker.
			name: "idle touches neither marker",
			prev: "working",
			vars: []Var{set(varState, "idle")},
			want: []string{"@AGENT_STATE", "@AGENT_SINCE"},
		},
		{
			// A pane outlives its agents, and the next one starts with nothing
			// finished.
			name: "clear forgets the work",
			prev: "idle",
			vars: []Var{del(varState), del(varKind), del(varMsg)},
			want: []string{"-u @AGENT_STATE", "-u @AGENT_KIND", "-u @AGENT_MSG", "-u @AGENT_WORKED"},
		},
		{
			name: "a batch with no state word gets no metadata",
			prev: "working",
			vars: []Var{set(varResume, "claude --resume abc")},
			want: []string{"@AGENT_RESUME"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport, runs := fakeTmux(t, tc.prev)

			if err := transport.Publish(tc.vars); err != nil {
				t.Fatalf("publish: %v", err)
			}

			lines := runs()
			if len(lines) != 1 {
				t.Fatalf("runs: got %d, want one process: %v", len(lines), lines)
			}
			if got := optionsIn(lines[0]); !slices.Equal(got, tc.want) {
				t.Fatalf("options:\n got %v\nwant %v", got, tc.want)
			}
		})
	}
}

// optionsIn reads the options one chained tmux command line sets, marking the
// deletions with "-u ".
func optionsIn(line string) []string {
	var out []string
	for command := range strings.SplitSeq(line, " ; ") {
		fields := strings.Fields(command)
		for i, f := range fields {
			if !strings.HasPrefix(f, "@") {
				continue
			}
			if slices.Contains(fields[:i], "-u") {
				out = append(out, "-u "+f)
				continue
			}
			out = append(out, f)
		}
	}
	return out
}

// The picker reads @AGENT_SINCE as unix seconds, and shows nothing at all for a
// value it cannot parse.
func TestTmuxTransportStampsUnixSeconds(t *testing.T) {
	transport, runs := fakeTmux(t, "idle")
	before := time.Now().Unix()

	if err := transport.Publish([]Var{set(varState, "working")}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	fields := strings.Fields(runs()[0])
	i := slices.Index(fields, "@AGENT_SINCE")
	if i < 0 || i+1 >= len(fields) {
		t.Fatalf("no @AGENT_SINCE value in %v", fields)
	}
	stamp, err := strconv.ParseInt(fields[i+1], 10, 64)
	if err != nil {
		t.Fatalf("@AGENT_SINCE %q is not unix seconds: %v", fields[i+1], err)
	}
	if stamp < before || stamp > time.Now().Unix() {
		t.Fatalf("@AGENT_SINCE %d is outside the run", stamp)
	}
}

// A pane keeps its resume command after the agent goes, the same as a kitty
// window: `cattery save` reads it long afterwards.
func TestTmuxClearKeepsTheResumeCommand(t *testing.T) {
	transport, runs := fakeTmux(t, "idle")

	Writer{Stdin: blockingPipe{}, Transport: transport}.Write("clear")

	line := runs()[0]
	if strings.Contains(line, "@AGENT_RESUME") {
		t.Fatalf("clear touched the resume command: %q", line)
	}
	for _, want := range []string{"@AGENT_STATE", "@AGENT_KIND", "@AGENT_MSG", "@AGENT_WORKED"} {
		if !strings.Contains(line, "-u -t %17 "+want) {
			t.Errorf("clear did not unset %s: %q", want, line)
		}
	}
}
