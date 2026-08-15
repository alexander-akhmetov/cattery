// Package events reads the agent state transitions the kitty watcher pushes.
//
// The watcher keeps a registry of unix datagram socket paths on kitty's boss
// object and sends one JSON event to each path on every transition. This binds
// such a socket, registers it through the cattery_events kitten, and copies
// what arrives to a writer, one event per line. It backs `cattery events`.
//
// Nothing is stored and nothing is replayed. An event that fires while nobody
// is subscribed is gone on purpose: durable history stays with the agents,
// which already write their own session files.
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
	"strings"
	"syscall"
	"time"
)

// ErrKittyGone says the kitty instance that took the registration has exited.
// The registry lives in that process, so the subscription died with it, and
// KITTY_LISTEN_ON in this environment still names the instance that is gone.
var ErrKittyGone = errors.New("the kitty this subscribed to has exited, so the registration is gone")

// KittenRunner runs a kitten inside the running kitty. *kitty.Client is one.
type KittenRunner interface {
	Kitten(ctx context.Context, path string, args ...string) (string, error)
}

const (
	// readBuffer is the receive queue. The default is 4096 bytes on macOS,
	// which holds about eleven events; this holds a few thousand, so a
	// subscriber that stalls for a moment costs the watcher nothing.
	readBuffer = 1 << 20

	// maxEvent bounds one datagram. The watcher's event is about 350 bytes
	// with a full prompt in it.
	maxEvent = 64 * 1024

	// unregisterTimeout bounds the last kitten call, which runs after the
	// context that ran the loop is already done.
	unregisterTimeout = 2 * time.Second
)

// pollInterval is how long a read waits before the loop looks up: at the
// context, and at whether kitty is still running. A variable, so a test does
// not wait a second for either.
var pollInterval = time.Second

// Subscribe binds a datagram socket, registers it with the running kitty, and
// writes every event it receives to out as one line. It returns when ctx is
// done, when out reports a closed pipe, or when kitty goes away.
//
// kittenPath is the installed cattery_events.py, which kitty runs by absolute
// path.
func Subscribe(ctx context.Context, k KittenRunner, kittenPath string, out io.Writer) error {
	dir, err := SocketDir()
	if err != nil {
		return err
	}
	// One socket per subscriber. A second bind on a shared path fails
	// EADDRINUSE, and a datagram reaches one reader even when two sockets share
	// a descriptor, so subscribers on one path would steal each other's events.
	// The pid keeps two of them apart; a crash leaves one stale socket, which
	// the watcher prunes the next time it sends.
	path := filepath.Join(dir, fmt.Sprintf("cattery-sub-%d.sock", os.Getpid()))
	conn, err := listen(path)
	if err != nil {
		return fmt.Errorf("bind %s: %w", path, err)
	}
	// Unlike a stream listener, a unixgram connection does not unlink its own
	// socket on Close.
	defer os.Remove(path)
	defer conn.Close()
	// A refusal is not fatal: the queue is then merely the small default.
	_ = conn.SetReadBuffer(readBuffer)

	if _, err := k.Kitten(ctx, kittenPath, "register", path); err != nil {
		return fmt.Errorf("register with kitty: %w", err)
	}
	defer unregister(k, kittenPath, path)

	return receive(ctx, conn, out)
}

// listen binds the subscriber's socket, clearing one an earlier run left
// behind.
//
// A unixgram socket is not unlinked by Close, and a run that ends on a signal
// unlinks nothing at all, so the path can hold a file nobody is bound to. bind
// answers EADDRINUSE for that file exactly as it does for a live socket, and
// then never recovers: the name carries this process's pid, so the same pid
// comes back to the same dead file. connect tells the two apart, because a unix
// datagram socket with nobody bound to it refuses.
func listen(path string) (*net.UnixConn, error) {
	addr := &net.UnixAddr{Name: path, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err == nil || !errors.Is(err, syscall.EADDRINUSE) || !refused(path) {
		return conn, err
	}
	if err := os.Remove(path); err != nil {
		return nil, err
	}
	return net.ListenUnixgram("unixgram", addr)
}

// refused reports that nobody is bound to the socket at path.
func refused(path string) bool {
	conn, err := net.Dial("unixgram", path)
	if err != nil {
		return errors.Is(err, syscall.ECONNREFUSED)
	}
	conn.Close()
	return false
}

// receive copies datagrams to out until something ends the subscription.
func receive(ctx context.Context, conn *net.UnixConn, out io.Writer) error {
	buf := make([]byte, maxEvent)
	for {
		select {
		case <-ctx.Done():
			// Ctrl-C, which is how the command normally ends.
			return nil
		default:
		}
		if err := conn.SetReadDeadline(time.Now().Add(pollInterval)); err != nil {
			return err
		}
		n, _, err := conn.ReadFromUnix(buf)
		if err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				return err
			}
			// Nothing arrived, which is also the only moment there is to
			// notice that the far end has gone.
			if kittyGone() {
				return ErrKittyGone
			}
			continue
		}
		if err := writeLine(out, buf[:n]); err != nil {
			// `cattery events | head -1` closes the pipe under us. That is the
			// reader saying it has what it came for, not a failure.
			if errors.Is(err, syscall.EPIPE) {
				return nil
			}
			return err
		}
	}
}

// writeLine sends one event in one write. Two writes can reach a pipe as two
// pieces, and the reader splits on newlines.
func writeLine(out io.Writer, event []byte) error {
	line := make([]byte, 0, len(event)+1)
	line = append(line, event...)
	line = append(line, '\n')
	_, err := out.Write(line)
	return err
}

// unregister drops the path from kitty's registry. The context that ran the
// loop is usually already done by the time this runs, so it makes its own.
//
// A failure goes unreported: the socket is removed a moment later, and the
// watcher prunes a path that answers ENOENT.
func unregister(k KittenRunner, kittenPath, path string) {
	ctx, cancel := context.WithTimeout(context.Background(), unregisterTimeout)
	defer cancel()
	_, _ = k.Kitten(ctx, kittenPath, "unregister", path)
}

// kittyGone reports that the kitty instance named by KITTY_LISTEN_ON has
// exited. Its socket file going away is the cheapest sign of it.
//
// Anything it cannot check reads as "still there": an abstract socket, a TCP
// listener, or no KITTY_LISTEN_ON at all.
func kittyGone() bool {
	addr, ok := strings.CutPrefix(os.Getenv("KITTY_LISTEN_ON"), "unix:")
	if !ok || addr == "" || strings.HasPrefix(addr, "@") {
		return false
	}
	_, err := os.Stat(addr)
	return errors.Is(err, fs.ErrNotExist)
}

// SocketDir is the directory the subscriber's socket goes in: XDG_RUNTIME_DIR,
// else TMPDIR, else /tmp.
//
// The events carry prompt text, so the directory has to be this user's alone.
// XDG_RUNTIME_DIR is, and so is TMPDIR on macOS. A shared /tmp is not, and
// there the socket goes inside a cattery-<uid> directory of mode 0700.
func SocketDir() (string, error) {
	base := "/tmp"
	for _, name := range []string{"XDG_RUNTIME_DIR", "TMPDIR"} {
		if dir := os.Getenv(name); dir != "" {
			base = dir
			break
		}
	}
	if isPrivate(base) {
		return base, nil
	}
	dir := filepath.Join(base, fmt.Sprintf("cattery-%d", os.Getuid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// MkdirAll leaves an existing directory's mode and owner alone, so this
	// also refuses one somebody else put there first.
	if !isPrivate(dir) {
		return "", fmt.Errorf("%s is not private to this user", dir)
	}
	return dir, nil
}

// isPrivate reports whether this user owns dir and nobody else can reach it.
func isPrivate(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}
