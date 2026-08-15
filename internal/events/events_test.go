package events

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKitty stands in for the running kitty. It keeps the registry the watcher
// holds on boss, so a test can watch a path arrive and leave.
type fakeKitty struct {
	mu     sync.Mutex
	subs   []string
	kitten string // the kitten path of the last call
	err    error
}

func (f *fakeKitty) Kitten(_ context.Context, kitten string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kitten = kitten
	if f.err != nil {
		return "", f.err
	}
	if len(args) != 2 {
		return "", fmt.Errorf("kitten called with %v", args)
	}
	switch args[0] {
	case "register":
		f.subs = append(f.subs, args[1])
	case "unregister":
		f.subs = slices.DeleteFunc(f.subs, func(path string) bool { return path == args[1] })
	default:
		return "", fmt.Errorf("unknown action %q", args[0])
	}
	return args[0] + " " + args[1], nil
}

func (f *fakeKitty) registered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.subs)
}

// buffer collects what the subscriber writes. Subscribe writes from the
// goroutine the test started it in, and the test reads while it runs.
type buffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *buffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b.data), "\n"), "\n")
}

// shortDir makes a directory with a given mode, short enough that a socket
// path inside it fits in the 104 bytes a unix address has room for. A macOS
// TempDir carries the test's name and is not always short enough.
func shortDir(t *testing.T, mode os.FileMode) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ct")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.Chmod(dir, mode); err != nil {
		t.Fatal(err)
	}
	return dir
}

// subscriberEnv points the socket at a private directory of this test's own and
// leaves no kitty for the restart check to miss.
func subscriberEnv(t *testing.T) string {
	t.Helper()
	dir := shortDir(t, 0o700)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("TMPDIR", dir)
	t.Setenv("KITTY_LISTEN_ON", "")
	previous := pollInterval
	pollInterval = 5 * time.Millisecond
	t.Cleanup(func() { pollInterval = previous })
	return dir
}

func socketPath(dir string) string {
	return filepath.Join(dir, fmt.Sprintf("cattery-sub-%d.sock", os.Getpid()))
}

// waitFor polls until want is true, or fails the test.
func waitFor(t *testing.T, what string, want func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if want() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// send delivers one datagram the way the watcher does.
func send(t *testing.T, path, event string) {
	t.Helper()
	conn, err := net.Dial("unixgram", path)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(event)); err != nil {
		t.Fatalf("send: %v", err)
	}
}

// The whole path: bind, register, one line per event, and a registration that
// goes away with the command.
func TestSubscribe(t *testing.T) {
	dir := subscriberEnv(t)
	k := &fakeKitty{}
	out := &buffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Subscribe(ctx, k, "/k/cattery_events.py", out) }()

	waitFor(t, "the registration", func() bool { return len(k.registered()) == 1 })
	path := k.registered()[0]
	if path != socketPath(dir) {
		t.Fatalf("registered %q, want %q", path, socketPath(dir))
	}
	if k.kitten != "/k/cattery_events.py" {
		t.Errorf("kitten path: got %q", k.kitten)
	}

	send(t, path, `{"to":"blocked"}`)
	send(t, path, `{"to":"done"}`)
	waitFor(t, "both events", func() bool { return len(out.lines()) == 2 })
	if got := out.lines(); got[0] != `{"to":"blocked"}` || got[1] != `{"to":"done"}` {
		t.Fatalf("lines: %q", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if got := k.registered(); len(got) != 0 {
		t.Errorf("still registered after exit: %v", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
}

// `cattery events | head -1` closes the pipe while the command is still
// running. That is the reader saying it has what it came for.
func TestSubscribeStopsQuietlyOnAClosedPipe(t *testing.T) {
	subscriberEnv(t)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	k := &fakeKitty{}
	done := make(chan error, 1)
	go func() { done <- Subscribe(context.Background(), k, "/k/cattery_events.py", writer) }()

	waitFor(t, "the registration", func() bool { return len(k.registered()) == 1 })
	send(t, k.registered()[0], `{"to":"done"}`)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("subscribe: got %v, want a clean stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscribe did not return")
	}
}

// The registry lives in kitty's process. A restart drops every subscription
// with nothing to notice it by, so the command says so and lets a supervisor
// start it again.
func TestSubscribeReportsARestartedKitty(t *testing.T) {
	subscriberEnv(t)
	t.Setenv("KITTY_LISTEN_ON", "unix:"+filepath.Join(shortDir(t, 0o700), "kitty-gone"))
	k := &fakeKitty{}

	err := Subscribe(context.Background(), k, "/k/cattery_events.py", io.Discard)

	if !errors.Is(err, ErrKittyGone) {
		t.Fatalf("subscribe: got %v, want ErrKittyGone", err)
	}
	if got := k.registered(); len(got) != 0 {
		t.Errorf("still registered: %v", got)
	}
}

// A listening socket that is still there is not a restart, whatever else the
// value looks like.
func TestSubscribeKeepsRunningWhileKittyIsThere(t *testing.T) {
	dir := subscriberEnv(t)
	live := filepath.Join(dir, "kitty-live")
	if err := os.WriteFile(live, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		value string
	}{
		{name: "a socket that exists", value: "unix:" + live},
		{name: "an abstract socket cannot be checked", value: "unix:@mykitty"},
		{name: "a tcp listener cannot be checked", value: "tcp:localhost:4321"},
		{name: "no value at all", value: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KITTY_LISTEN_ON", tc.value)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()

			if err := Subscribe(ctx, &fakeKitty{}, "/k/cattery_events.py", io.Discard); err != nil {
				t.Fatalf("subscribe: got %v, want it to run until the context ended", err)
			}
		})
	}
}

// A run killed before its cleanup leaves the socket file behind, and the pid in
// the name comes back around. bind cannot tell that file from a live socket, so
// the subscriber has to clear it itself or never start again.
func TestSubscribeTakesOverASocketNobodyIsBoundTo(t *testing.T) {
	dir := subscriberEnv(t)
	stale, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: socketPath(dir), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	// Close leaves the file: that is the whole problem.
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}

	k := &fakeKitty{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Subscribe(ctx, k, "/k/cattery_events.py", io.Discard) }()

	waitFor(t, "the registration", func() bool { return len(k.registered()) == 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("subscribe: %v", err)
	}
}

// The other reading of EADDRINUSE, and the one that must not lose anybody's
// events: a subscriber is bound to that path right now.
func TestSubscribeLeavesALiveSocketAlone(t *testing.T) {
	dir := subscriberEnv(t)
	live, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: socketPath(dir), Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()

	err = Subscribe(context.Background(), &fakeKitty{}, "/k/cattery_events.py", io.Discard)

	if err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("subscribe: got %v, want the bind to fail", err)
	}
	if _, err := os.Stat(socketPath(dir)); err != nil {
		t.Errorf("the live socket was removed: %v", err)
	}
}

// A kitty that will not take the registration leaves no socket behind.
func TestSubscribeReportsARegistrationFailure(t *testing.T) {
	dir := subscriberEnv(t)
	k := &fakeKitty{err: errors.New("no listening socket")}

	err := Subscribe(context.Background(), k, "/k/cattery_events.py", io.Discard)

	if err == nil || !strings.Contains(err.Error(), "no listening socket") {
		t.Fatalf("subscribe: got %v, want the reason kitty gave", err)
	}
	if _, err := os.Stat(socketPath(dir)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
}

func TestSocketDir(t *testing.T) {
	private := fmt.Sprintf("cattery-%d", os.Getuid())

	t.Run("a private XDG_RUNTIME_DIR is used as it is", func(t *testing.T) {
		dir := shortDir(t, 0o700)
		t.Setenv("XDG_RUNTIME_DIR", dir)
		t.Setenv("TMPDIR", shortDir(t, 0o700))

		got, err := SocketDir()

		if err != nil || got != dir {
			t.Fatalf("SocketDir: got %q, %v, want %q", got, err, dir)
		}
	})

	t.Run("TMPDIR when there is no XDG_RUNTIME_DIR", func(t *testing.T) {
		dir := shortDir(t, 0o700)
		t.Setenv("XDG_RUNTIME_DIR", "")
		t.Setenv("TMPDIR", dir)

		got, err := SocketDir()

		if err != nil || got != dir {
			t.Fatalf("SocketDir: got %q, %v, want %q", got, err, dir)
		}
	})

	t.Run("a shared directory gets a private one inside it", func(t *testing.T) {
		base := shortDir(t, 0o777)
		t.Setenv("XDG_RUNTIME_DIR", "")
		t.Setenv("TMPDIR", base)

		got, err := SocketDir()

		want := filepath.Join(base, private)
		if err != nil || got != want {
			t.Fatalf("SocketDir: got %q, %v, want %q", got, err, want)
		}
		info, err := os.Stat(got)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("mode: got %v, want 0700", info.Mode().Perm())
		}
	})

	t.Run("a directory anyone can reach is refused", func(t *testing.T) {
		base := shortDir(t, 0o777)
		t.Setenv("XDG_RUNTIME_DIR", "")
		t.Setenv("TMPDIR", base)
		// Somebody else got there first, or an older cattery did with a wider
		// umask. Either way the events must not go through it.
		if err := os.Mkdir(filepath.Join(base, private), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(base, private), 0o755); err != nil {
			t.Fatal(err)
		}

		if _, err := SocketDir(); err == nil {
			t.Fatal("SocketDir: got a nil error")
		}
	})
}
