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
// reads the argv from here, one argument per space, publishes it, and exits
// before the test framework runs anything.
const subprocessState = "CATTERY_TEST_STATE"

// argvFile is where the fake kitten records the arguments it was called with.
const argvFile = "CATTERY_TEST_ARGV"

// The path a Claude command hook really takes. Those hooks run without a
// controlling terminal, so the OSC write to /dev/tty fails and the batch goes
// over kitty remote control. Only a separate process can be without a terminal,
// so the test re-execs itself in a new session.
func TestPublishFromAProcessWithoutATerminal(t *testing.T) {
	if argv := os.Getenv(subprocessState); argv != "" {
		Run(strings.Fields(argv))
		// Exit before the test framework prints its own result. Claude captures
		// a hook's stdout into the transcript, which has to stay empty.
		os.Exit(0)
	}

	cases := []struct {
		name string
		// argv is everything after `cattery state`, so a case can carry the
		// --kind flag the Codex plugin passes.
		argv  string
		stdin string
		want  []string // what the fake kitten saw, or nothing when it must not run
	}{
		{
			// AGENT_STATE comes last in every batch: writing it is what wakes
			// the watcher, and the watcher reads the others out of the window.
			name:  "working carries the kind, the prompt, and the state",
			argv:  "working",
			stdin: `{"prompt":"fix the picker"}`,
			want: []string{"@", "set-user-vars", "--match", "id:7",
				"AGENT_KIND=claude", "AGENT_MSG=fix the picker", "AGENT_STATE=working"},
		},
		{
			name:  "blocked leaves the message alone",
			argv:  "blocked",
			stdin: `{"prompt":"ignored"}`,
			want: []string{"@", "set-user-vars", "--match", "id:7",
				"AGENT_KIND=claude", "AGENT_STATE=blocked"},
		},
		{
			// Bare names. That is how both kitty and the OSC escape spell
			// "remove this variable"; an empty value means something else.
			name: "clear deletes the three variables, in order",
			argv: "clear",
			want: []string{"@", "set-user-vars", "--match", "id:7",
				"AGENT_KIND", "AGENT_MSG", "AGENT_STATE"},
		},
		{
			// The deletes are bare names here too, between the resume command
			// and the state.
			name:  "a session start deletes the prompt and the worked flag",
			argv:  "idle",
			stdin: `{"session_id":"s1","source":"startup"}`,
			want: []string{"@", "set-user-vars", "--match", "id:7",
				"AGENT_KIND=claude", "AGENT_RESUME=claude --resume s1",
				"AGENT_MSG", "AGENT_WORKED", "AGENT_STATE=idle"},
		},
		{
			name:  "a compaction publishes nothing",
			argv:  "idle",
			stdin: `{"session_id":"s1","source":"compact"}`,
		},
		{
			name: "an unknown word publishes nothing",
			argv: "sleeping",
		},
		{
			// What the Codex plugin's UserPromptSubmit hook runs.
			name:  "--kind codex carries the kind and Codex's own resume command",
			argv:  "working --kind codex",
			stdin: `{"session_id":"s1","prompt":"fix the picker"}`,
			want: []string{"@", "set-user-vars", "--match", "id:7",
				"AGENT_KIND=codex", "AGENT_RESUME=codex resume s1",
				"AGENT_MSG=fix the picker", "AGENT_STATE=working"},
		},
		{
			// What the opencode plugin runs at a tool boundary. The tool pair
			// travels on stdin beside the prompt, so the argv is the same shape
			// the other two agents use.
			name:  "--kind opencode carries the tool pair",
			argv:  "working --kind opencode",
			stdin: `{"session_id":"ses_8a3f","prompt":"fix the picker","tool":"bash: go test ./...","tool_since":1755302096}`,
			want: []string{"@", "set-user-vars", "--match", "id:7",
				"AGENT_KIND=opencode", "AGENT_RESUME=opencode --session ses_8a3f",
				"AGENT_MSG=fix the picker", "AGENT_TOOL_SINCE=1755302096",
				"AGENT_TOOL=bash: go test ./...", "AGENT_STATE=working"},
		},
		{
			name: "a Codex clear deletes the same three",
			argv: "clear --kind codex",
			want: []string{"@", "set-user-vars", "--match", "id:7",
				"AGENT_KIND", "AGENT_MSG", "AGENT_STATE"},
		},
		{
			name:  "an unrecognised kind publishes claude",
			argv:  "working --kind gpt",
			stdin: `{"session_id":"s1"}`,
			want: []string{"@", "set-user-vars", "--match", "id:7",
				"AGENT_KIND=claude", "AGENT_RESUME=claude --resume s1", "AGENT_STATE=working"},
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
				subprocessState + "=" + tc.argv,
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
				t.Fatalf("cattery state %s: %v\n%s", tc.argv, err, stderr.String())
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
