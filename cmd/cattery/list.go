package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/alexander-akhmetov/cattery/internal/agent"
)

// inventoryTimeout is what one listing gets. Both hosts spawn a process, and
// the picker is not there to say what is taking so long.
const inventoryTimeout = 3 * time.Second

// lister is the inventory this command reads, an interface so a test can print
// without a kitty or a tmux server.
type lister interface {
	ListAgents(ctx context.Context) ([]agent.Agent, error)
}

// listPayload is one snapshot. An object rather than a bare array, because it
// carries two facts no agent row can: which host failed, and which cattery
// answered.
type listPayload struct {
	Cattery string      `json:"cattery"`
	Errors  []string    `json:"errors,omitempty"`
	Agents  []listAgent `json:"agents"`
}

// listAgent is one agent on the wire. A field cattery derives takes cattery's
// own short name, the way the events protocol says "focused"; a field carrying
// host data verbatim keeps the host's name for it.
//
// Only key, host, id and display are always there. Everything else is omitted
// when empty, which is also what keeps "0 is not a timestamp" visible: a zero
// time never reaches the output as 0.
type listAgent struct {
	Key     string `json:"key"`
	Host    string `json:"host"`
	ID      int    `json:"id"`
	Self    bool   `json:"self,omitempty"`
	Kind    string `json:"kind,omitempty"`
	State   string `json:"state,omitempty"`
	Display string `json:"display"`
	Title   string `json:"title,omitempty"`
	CWD     string `json:"cwd,omitempty"`
	Msg     string `json:"msg,omitempty"`

	Tool      string `json:"tool,omitempty"`
	ToolSince int64  `json:"tool_since,omitempty"`
	Since     int64  `json:"since,omitempty"`
	CreatedAt int64  `json:"created_at,omitempty"`

	Target string `json:"target,omitempty"`
	Resume string `json:"resume,omitempty"`

	Project    string `json:"project,omitempty"`
	ProjectKey string `json:"project_key,omitempty"`
	Root       string `json:"root,omitempty"`
	Branch     string `json:"branch,omitempty"`

	PID     int           `json:"pid,omitempty"`
	Command string        `json:"command,omitempty"`
	Procs   []listProcess `json:"foreground_processes,omitempty"`
}

type listProcess struct {
	PID     int      `json:"pid"`
	Cmdline []string `json:"cmdline,omitempty"`
	CWD     string   `json:"cwd,omitempty"`
}

func listFlags() (*flag.FlagSet, *bool) {
	flags := newFlagSet(cmdList)
	asJSON := flags.Bool("json", false, "print the whole inventory as one JSON object instead of columns")
	return flags, asJSON
}

// runList prints the inventory, as columns or as JSON.
//
// A host that failed still returns the other's rows, so those print before the
// failure is reported. With -json the failures go into the payload as well as
// onto stderr: a consumer reading only stdout would otherwise read a broken
// listing as an empty one.
func runList(client lister, out io.Writer, args []string) int {
	flags, asJSON := listFlags()
	if err := flags.Parse(args); err != nil {
		return flagExit(err)
	}
	if flags.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "cattery: list takes no arguments")
		return 2
	}

	if !*asJSON {
		if err := printAgents(client, out); err != nil {
			fmt.Fprintln(os.Stderr, "cattery:", err)
			return 1
		}
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), inventoryTimeout)
	defer cancel()
	found, err := client.ListAgents(ctx)

	payload := listPayload{Cattery: versionString(), Errors: hostErrors(err), Agents: listAgents(found)}
	encoder := json.NewEncoder(out)
	// A prompt or a cmdline carries &&, < and > often enough that the default
	// escaping, which rewrites those three as \u0026, \u003c and \u003e,
	// would change what the agent actually said.
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if encodeErr := encoder.Encode(payload); encodeErr != nil {
		fmt.Fprintln(os.Stderr, "cattery:", encodeErr)
		return 1
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cattery:", err)
		return 1
	}
	return 0
}

// hostErrors splits the joined listing error into one string per host, the way
// ListAgents wrapped them.
func hostErrors(err error) []string {
	if err == nil {
		return nil
	}
	// errors.Join is what ListAgents wraps with, so the multi-error shape is
	// exactly what is being asked about here rather than the chain under it.
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return []string{err.Error()}
	}
	msgs := make([]string, 0, len(joined.Unwrap()))
	for _, e := range joined.Unwrap() {
		msgs = append(msgs, e.Error())
	}
	return msgs
}

func listAgents(found []agent.Agent) []listAgent {
	rows := make([]listAgent, 0, len(found))
	for _, a := range found {
		row := listAgent{
			Key:        a.Key(),
			Host:       a.Host,
			ID:         a.ID,
			Self:       a.Self,
			Kind:       a.Kind,
			State:      a.State,
			Display:    a.Display,
			Title:      a.Title,
			CWD:        a.CWD,
			Msg:        a.Msg,
			Tool:       a.Tool,
			Target:     a.Target,
			Resume:     a.Resume,
			Project:    a.Project,
			ProjectKey: a.ProjectKey,
			Root:       a.Root,
			Branch:     a.Branch,
			PID:        a.PID,
			Command:    a.Command,
			Procs:      listProcs(a.Procs),
		}
		row.Since = unix(a.Since)
		row.ToolSince = unix(a.ToolSince)
		row.CreatedAt = unix(a.CreatedAt)
		rows = append(rows, row)
	}
	return rows
}

// unix is 0 for a time nobody set, which omitempty then drops. time.Unix of a
// zero Time is a large negative number, and a consumer would read it as a date.
func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func listProcs(procs []agent.Process) []listProcess {
	if len(procs) == 0 {
		return nil
	}
	out := make([]listProcess, len(procs))
	for i, p := range procs {
		out[i] = listProcess{PID: p.PID, Cmdline: p.Cmdline, CWD: p.CWD}
	}
	return out
}

// printAgents writes the inventory as one line per agent. A tmux row carries
// the target `cattery attach` takes; a kitty row is reached by window id and
// has none.
//
// A host that failed still returns the other's rows, so they are printed before
// the error goes back.
func printAgents(client lister, out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), inventoryTimeout)
	defer cancel()
	agents, err := client.ListAgents(ctx)
	for _, a := range agents {
		target := ""
		if a.Target != "" {
			target = " target=" + a.Target
		}
		// The kind column is 8 wide because "opencode" is, and a kind that
		// overruns its column shifts the whole rest of that row.
		fmt.Fprintf(out, "%-16s %-7s %-8s host=%-5s id=%-5d %-24s %s%s\n",
			a.Project, a.Display, a.Kind, a.Host, a.ID, a.Branch, a.CWD, target)
	}
	return err
}
