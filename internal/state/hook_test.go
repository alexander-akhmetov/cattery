//go:build unix

package state

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// subprocessState turns the test binary into `cattery state <x>`. The child
// reads the word from here, publishes it, and exits before the test framework
// runs anything.
const subprocessState = "CATTERY_TEST_STATE"

// argvFile is where the fake kitten records the arguments it was called with.
const argvFile = "CATTERY_TEST_ARGV"

// The path a Claude command hook really takes. Those hooks run without a
// controlling terminal, so the OSC write to /dev/tty fails and the batch goes
// over kitty remote control. Only a separate process can be without a terminal,
// so the test re-execs itself in a new session.
func TestPublishFromAProcessWithoutATerminal(t *testing.T) {
	if word := os.Getenv(subprocessState); word != "" {
		Run([]string{word})
		// Exit before the test framework prints its own result. Claude captures
		// a hook's stdout into the transcript, which has to stay empty.
		os.Exit(0)
	}

	cases := []struct {
		name  string
		state string
		stdin string
		want  []string // what the fake kitten saw, or nothing when it must not run
	}{
		{
			name:  "working carries the kind, the state, and the prompt",
			state: "working",
			stdin: `{"prompt":"fix the picker"}`,
			want: []string{"@", "set-user-vars", "--match", "id:7",
				"AGENT_KIND=claude", "AGENT_STATE=working", "AGENT_MSG=fix the picker"},
		},
		{
			name:  "blocked leaves the message alone",
			state: "blocked",
			stdin: `{"prompt":"ignored"}`,
			want: []string{"@", "set-user-vars", "--match", "id:7",
				"AGENT_KIND=claude", "AGENT_STATE=blocked"},
		},
		{
			// Bare names. That is how both kitty and the OSC escape spell
			// "remove this variable"; an empty value means something else.
			name:  "clear deletes the three variables, in order",
			state: "clear",
			want: []string{"@", "set-user-vars", "--match", "id:7",
				"AGENT_STATE", "AGENT_KIND", "AGENT_MSG"},
		},
		{
			name:  "an unknown word publishes nothing",
			state: "sleeping",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			argv := filepath.Join(dir, "argv")
			writeFakeKitten(t, dir, `printf '%s\n' "$@" > "$`+argvFile+`"`)

			cmd := exec.Command(os.Args[0], "-test.run=^TestPublishFromAProcessWithoutATerminal$")
			// PATH holds the fake kitten and nothing else, so a real kitty on
			// this machine cannot answer instead of it.
			cmd.Env = []string{
				subprocessState + "=" + tc.state,
				argvFile + "=" + argv,
				"PATH=" + dir,
				"KITTY_WINDOW_ID=7",
				"KITTY_LISTEN_ON=unix:/tmp/kitty-test",
			}
			cmd.Stdin = strings.NewReader(tc.stdin)
			// A new session has no controlling terminal, so /dev/tty cannot be
			// opened and nothing reaches the terminal running the tests.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

			// Stdout only. Under `go test -cover` the child's coverage runtime
			// warns on stderr when it exits without GOCOVERDIR, and stdout is
			// the stream that matters here.
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			stdout, err := cmd.Output()

			if err != nil {
				t.Fatalf("cattery state %s: %v\n%s", tc.state, err, stderr.String())
			}
			if len(stdout) > 0 {
				t.Errorf("the hook wrote to stdout: %q", stdout)
			}
			recorded, readErr := os.ReadFile(argv)
			if len(tc.want) == 0 {
				if readErr == nil {
					t.Fatalf("kitten ran with %q, want no call at all", recorded)
				}
				return
			}
			if readErr != nil {
				t.Fatalf("the remote-control fallback never ran: %v", readErr)
			}
			got := strings.Split(strings.TrimSuffix(string(recorded), "\n"), "\n")
			if !slices.Equal(got, tc.want) {
				t.Fatalf("kitten arguments:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}
