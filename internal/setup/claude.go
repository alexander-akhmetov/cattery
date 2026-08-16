package setup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// claudeHooks maps each Claude Code hook event to the state cattery publishes
// from it. Order is the order the hooks fire, and the order claude/hooks/hooks.json
// lists them in.
//
// SessionStart takes a matcher, because that hook fires for a compaction and a
// fork as well as for a session opening, and an idle mid-turn would mark a
// running agent finished. A fork is either one and the payload does not say
// which, so the matcher drops that word too: see startSources in
// internal/state. The writer checks the payload's source as well, for a Claude
// that predates the matcher.
//
// The plugin manifest is what Claude reads. This list is what the tests hold it
// to, and what the removal below looks for in a settings.json an older release
// merged its hooks into.
var claudeHooks = []struct{ Event, State, Matcher string }{
	{Event: "SessionStart", State: "idle", Matcher: "startup|resume|clear"},
	{Event: "Notification", State: "blocked"},
	{Event: "UserPromptSubmit", State: "working"},
	{Event: "Stop", State: "idle"},
	{Event: "SessionEnd", State: "clear"},
}

// object is a JSON object that remembers the order its keys arrived in, and
// keeps every value it does not need as raw bytes. Claude's settings.json
// belongs to the user. An edit that reshuffled its keys, or rewrote a command
// it never touched, would make the diff unreadable.
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

// remove drops a key, leaving the order of the rest alone.
func (o *object) remove(key string) {
	if _, ok := o.values[key]; !ok {
		return
	}
	delete(o.values, key)
	o.keys = slices.DeleteFunc(o.keys, func(k string) bool { return k == key })
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
// absent. A key holding anything other than an object is an error, because
// replacing it would throw the user's value away.
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

// removeCatteryHooks takes cattery's own hooks back out of Claude's settings and
// returns the whole file with the number of entries it dropped. Releases before
// the plugin merged them in there; the plugin runs them now, and an entry left
// behind publishes every state a second time.
//
// Only the commands catteryCommand recognises go. A group left with no commands
// goes with them, then an event left with no groups, then "hooks" itself. Every
// other key keeps its place and its bytes, so a diff of the file shows the five
// entries leaving and nothing else.
//
// A file holding nothing of cattery's comes back as it arrived, byte for byte.
// Re-encoding reindents the whole file, which is not a change to make on a file
// that needed no edit.
func removeCatteryHooks(settings []byte) ([]byte, int, error) {
	if len(bytes.TrimSpace(settings)) == 0 {
		return settings, 0, nil
	}
	root := object{}
	if err := json.Unmarshal(settings, &root); err != nil {
		return nil, 0, err
	}
	if _, ok := root.values["hooks"]; !ok {
		return settings, 0, nil
	}
	hooks, err := root.child("hooks")
	if err != nil {
		return nil, 0, err
	}

	removed := 0
	for _, h := range claudeHooks {
		raw, ok := hooks.values[h.Event]
		if !ok {
			continue
		}
		var groups []json.RawMessage
		if err := json.Unmarshal(raw, &groups); err != nil {
			return nil, 0, fmt.Errorf("hooks.%s: %w", h.Event, err)
		}
		kept, dropped, err := dropCatteryEntries(groups, h.State)
		if err != nil {
			return nil, 0, fmt.Errorf("hooks.%s: %w", h.Event, err)
		}
		if dropped == 0 {
			continue
		}
		removed += dropped
		if len(kept) == 0 {
			hooks.remove(h.Event)
			continue
		}
		encoded, err := encode(kept)
		if err != nil {
			return nil, 0, err
		}
		hooks.set(h.Event, encoded)
	}
	if removed == 0 {
		return settings, 0, nil
	}

	if len(hooks.keys) == 0 {
		root.remove("hooks")
	} else {
		encoded, err := encode(hooks)
		if err != nil {
			return nil, 0, err
		}
		root.set("hooks", encoded)
	}

	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, 0, err
	}
	return out.Bytes(), removed, nil
}

// dropCatteryEntries takes cattery's commands out of one event's groups and
// reports how many it took. A group holding another tool's command keeps that
// command, its matcher and the raw bytes of both; a group holding nothing else
// goes. A group in a shape this cannot read is somebody else's and stays whole.
func dropCatteryEntries(groups []json.RawMessage, state string) ([]json.RawMessage, int, error) {
	kept := make([]json.RawMessage, 0, len(groups))
	removed := 0
	for _, rawGroup := range groups {
		var group object
		if err := json.Unmarshal(rawGroup, &group); err != nil {
			kept = append(kept, rawGroup)
			continue
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(group.values["hooks"], &entries); err != nil {
			kept = append(kept, rawGroup)
			continue
		}
		keptEntries := make([]json.RawMessage, 0, len(entries))
		for _, rawEntry := range entries {
			if catteryEntry(rawEntry, state) {
				removed++
				continue
			}
			keptEntries = append(keptEntries, rawEntry)
		}
		switch {
		case len(keptEntries) == len(entries):
			kept = append(kept, rawGroup)
		case len(keptEntries) == 0:
		default:
			encoded, err := encode(keptEntries)
			if err != nil {
				return nil, 0, err
			}
			group.set("hooks", encoded)
			regrouped, err := encode(group)
			if err != nil {
				return nil, 0, err
			}
			kept = append(kept, regrouped)
		}
	}
	return kept, removed, nil
}

// catteryEntry reports whether one hook entry runs a cattery command for state.
func catteryEntry(raw json.RawMessage, state string) bool {
	var entry object
	if err := json.Unmarshal(raw, &entry); err != nil {
		return false
	}
	var command string
	if err := json.Unmarshal(entry.values["command"], &command); err != nil {
		return false
	}
	return catteryCommand(command, state)
}

// encode is json.Marshal with HTML escaping off. Marshal rewrites <, > and & as
// \u escapes, even inside a value handed to it for copying, so
// `make fmt && make lint` and `2>/dev/null` would come back as something the
// user did not type. Every step of the merge goes through this, because the
// escaping happens wherever a raw value is copied.
func encode(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// catteryCommand reports whether a hook command is one cattery wrote for the
// given state: the `<binary> state <x>` form releases before the plugin merged
// in, or the `cattery-state <x>` of an older install still. It reads the end of
// the command instead of searching for "cattery" anywhere in it, or it would
// delete a hook like `cd ~/projects/cattery && make lint`.
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

// binaryName is the command a hook field names, without its directory and
// without the quotes an older release put around a path holding a space.
func binaryName(field string) string {
	return filepath.Base(strings.Trim(field, `'"`))
}
