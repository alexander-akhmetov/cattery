package state

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alexander-akhmetov/cattery/internal/kitty"
)

// recorder stands in for a kitty window: it keeps the batches a writer sent so
// a test can assert the update order without a terminal or a live kitty.
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

// charDevice is stdin attached to a terminal. It fails the test if anything
// reads it, which is what a manual `cattery state idle` must never do.
type charDevice struct{ t *testing.T }

func (c charDevice) Read([]byte) (int, error) {
	c.t.Fatal("read a character-device stdin, which would hang a manual run")
	return 0, io.EOF
}

func (charDevice) Stat() (fs.FileInfo, error) { return charDeviceInfo{}, nil }

type charDeviceInfo struct{ fs.FileInfo }

func (charDeviceInfo) Mode() fs.FileMode { return fs.ModeCharDevice | 0o620 }

// blockingPipe is the open pipe a fish wrapper can hand to `cattery state
// clear`: it never produces data and never closes.
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
			want:  []Var{set(varKind, "claude"), set(varState, "working")},
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
// whatever stdin it inherited. Reading that first would hang the shell after
// every agent exit, so clear must publish and return without touching it.
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

func TestWriteWithoutAWayToPublish(t *testing.T) {
	cases := []struct {
		name   string
		writer Writer
	}{
		{
			// KITTY_WINDOW_ID is set on every child shell inside kitty, so an
			// empty one means the caller is somewhere else entirely.
			name:   "outside kitty",
			writer: Writer{Stdin: strings.NewReader(`{"prompt":"x"}`), Transport: &recorder{}},
		},
		{
			name:   "no transport",
			writer: Writer{WindowID: "7", Stdin: strings.NewReader(`{"prompt":"x"}`)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, state := range []string{"working", "blocked", "idle", "clear"} {
				tc.writer.Write(state)
			}
			if rec, ok := tc.writer.Transport.(*recorder); ok && len(rec.batches) != 0 {
				t.Fatalf("published %v with no window id", rec.batches)
			}
		})
	}
}

// A transport that cannot reach kitty is not the agent's problem: the writer
// swallows it, so the hook exits 0 either way.
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
			// A two-byte rune sitting on the limit: the cut must fall between
			// runes, because the picker parses `kitten @ ls` output as JSON and
			// a partial encoding there breaks the parse.
			name: "a cut lands between runes",
			hook: `{"prompt":"` + strings.Repeat("é", 300) + `"}`,
			want: strings.Repeat("é", promptLimit),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizePrompt([]byte(tc.hook))
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

// The chain is what makes a Claude hook work: /dev/tty fails there, and the
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

	// Every transport failing reports the last reason, which nothing above
	// acts on but which keeps the chain honest.
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

// New wires the transports from the kitty environment: the terminal always, and
// the remote-control socket behind it when kitty published one and the window id
// is a number that socket can match on.
func TestNewBuildsTheTransportChain(t *testing.T) {
	cases := []struct {
		name     string
		windowID string
		listenOn string
		want     int // transports in the chain; 0 means no transport at all
	}{
		{name: "outside kitty"},
		{name: "inside kitty, no remote control", windowID: "7", want: 1},
		{name: "inside kitty with remote control", windowID: "7", listenOn: "unix:/tmp/kitty-1", want: 2},
		{name: "a window id that is not a number", windowID: "w7", listenOn: "unix:/tmp/kitty-1", want: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KITTY_WINDOW_ID", tc.windowID)
			t.Setenv("KITTY_LISTEN_ON", tc.listenOn)

			w := New()

			if w.WindowID != tc.windowID {
				t.Errorf("window id: got %q, want %q", w.WindowID, tc.windowID)
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
// open failure that every Claude command hook hits: those run without a
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

// A kitten that cannot reach kitty has to say so, or the chain would count the
// update as published and stop.
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
