package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// claudeHooks maps each Claude Code hook event to the state cattery publishes
// from it. Order is the order setup writes and reports them in.
var claudeHooks = []struct{ Event, State string }{
	{Event: "Notification", State: "blocked"},
	{Event: "UserPromptSubmit", State: "working"},
	{Event: "Stop", State: "idle"},
	{Event: "SessionEnd", State: "clear"},
}

// hookCommand is what one Claude hook runs.
func hookCommand(binary, state string) string {
	return shellQuote(binary) + " state " + state
}

// hookEntry is one command inside a hook group.
type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// object is a JSON object that remembers the order its keys arrived in, and
// keeps every value it does not need as raw bytes. Claude's settings.json is
// the user's file: a merge that reshuffled its keys, or rewrote the text of a
// command it never touched, would make the diff unreadable.
type object struct {
	keys   []string
	values map[string]json.RawMessage
}

func (o *object) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('{') {
		return fmt.Errorf("expected a JSON object, got %v", tok)
	}
	o.keys = nil
	o.values = map[string]json.RawMessage{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("expected an object key, got %v", keyTok)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		o.set(key, raw)
	}
	_, err = dec.Token() // the closing brace
	return err
}

func (o object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, key := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		encoded, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		b.Write(encoded)
		b.WriteByte(':')
		b.Write(o.values[key])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (o *object) set(key string, value json.RawMessage) {
	if o.values == nil {
		o.values = map[string]json.RawMessage{}
	}
	if _, seen := o.values[key]; !seen {
		o.keys = append(o.keys, key)
	}
	if len(value) == 0 {
		value = json.RawMessage("null")
	}
	o.values[key] = value
}

// child decodes one nested object, returning an empty one when the key is
// absent. A key holding something that is not an object is an error: replacing
// it would throw the user's value away.
func (o object) child(key string) (object, error) {
	raw, ok := o.values[key]
	if !ok {
		return object{}, nil
	}
	var out object
	if err := json.Unmarshal(raw, &out); err != nil {
		return object{}, fmt.Errorf("%q: %w", key, err)
	}
	return out, nil
}

// mergeClaudeHooks adds cattery's four hooks to Claude's settings and returns
// the whole file. Existing entries stay: the arrays under hooks.<Event> hold
// unrelated tools too, so the merge appends rather than replaces, and it
// rewrites only the one command cattery already owns, so a re-run updates the
// binary path instead of adding a second copy.
func mergeClaudeHooks(settings []byte, binary string) ([]byte, error) {
	root := object{}
	if len(bytes.TrimSpace(settings)) > 0 {
		if err := json.Unmarshal(settings, &root); err != nil {
			return nil, err
		}
	}

	hooks, err := root.child("hooks")
	if err != nil {
		return nil, err
	}
	for _, h := range claudeHooks {
		var groups []json.RawMessage
		if raw, ok := hooks.values[h.Event]; ok {
			if err := json.Unmarshal(raw, &groups); err != nil {
				return nil, fmt.Errorf("hooks.%s: %w", h.Event, err)
			}
		}
		command := hookCommand(binary, h.State)
		updated, err := updateCatteryHook(groups, h.State, command)
		if err != nil {
			return nil, fmt.Errorf("hooks.%s: %w", h.Event, err)
		}
		if !updated {
			// A struct, not a map: a map's keys are sorted, which would write
			// "command" before "type" and read as a different shape from every
			// hook already in the file.
			group, err := encode(struct {
				Hooks []hookEntry `json:"hooks"`
			}{Hooks: []hookEntry{{Type: "command", Command: command}}})
			if err != nil {
				return nil, err
			}
			groups = append(groups, group)
		}
		raw, err := encode(groups)
		if err != nil {
			return nil, err
		}
		hooks.set(h.Event, raw)
	}

	raw, err := encode(hooks)
	if err != nil {
		return nil, err
	}
	root.set("hooks", raw)

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// encode is json.Marshal with HTML escaping off. Marshal rewrites <, > and & as
// their \u escapes, even inside a value it was handed to copy, which would
// change the text of a command cattery never touched: `make fmt && make lint`
// and `2>/dev/null` would come back as something the user did not type. Every
// step of the merge goes through this, because the escaping happens wherever a
// raw value is copied, not only at the end.
func encode(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// updateCatteryHook points the hook cattery already owns at the current binary,
// and reports whether it found one. Only that one command string changes: the
// group around it is the user's, holding a matcher and other tools' commands,
// and replacing the whole group would delete them.
func updateCatteryHook(groups []json.RawMessage, state, command string) (bool, error) {
	encoded, err := encode(command)
	if err != nil {
		return false, err
	}
	for gi, rawGroup := range groups {
		var group object
		if err := json.Unmarshal(rawGroup, &group); err != nil {
			continue // not a shape we understand, so not ours to edit
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(group.values["hooks"], &entries); err != nil {
			continue
		}
		for ei, rawEntry := range entries {
			var entry object
			if err := json.Unmarshal(rawEntry, &entry); err != nil {
				continue
			}
			var current string
			if err := json.Unmarshal(entry.values["command"], &current); err != nil {
				continue
			}
			if !catteryCommand(current, state) {
				continue
			}
			entry.set("command", encoded)
			if entries[ei], err = encode(entry); err != nil {
				return false, err
			}
			rawEntries, err := encode(entries)
			if err != nil {
				return false, err
			}
			group.set("hooks", rawEntries)
			if groups[gi], err = encode(group); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

// catteryCommand reports whether a hook command is one cattery wrote for the
// given state: the current `<binary> state <x>` form, or the `cattery-state <x>`
// an older install has. It reads the end of the command instead of looking for
// "cattery" anywhere in it, because a hook like
// `cd ~/projects/cattery && make lint` is not cattery's to rewrite.
func catteryCommand(command, state string) bool {
	fields := strings.Fields(command)
	if len(fields) < 2 || fields[len(fields)-1] != state {
		return false
	}
	if name := binaryName(fields[len(fields)-2]); name != "state" {
		return name == "cattery-state"
	}
	return len(fields) > 2 && binaryName(fields[len(fields)-3]) == "cattery"
}

// binaryName is the command a hook field names, without its directory or the
// quotes shellQuote puts around a path holding a space.
func binaryName(field string) string {
	return filepath.Base(strings.Trim(field, `'"`))
}
