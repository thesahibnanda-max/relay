package agy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// agy asks before every MCP tool call unless its settings allow the tool.
// Relay never writes this file on its own: only when the user says yes to
// the offer made at launch (see OfferToolPermission).

// relayToolsRule is agy's permission rule for every relay tool.
const relayToolsRule = "mcp(relay/*)"

// settingsPath is agy's user settings file under home.
func settingsPath(home string) string {
	return filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
}

// RelayToolsAllowed reports whether agy's settings already let relay's tools
// run without asking.
func RelayToolsAllowed(home string) (bool, error) {
	_, allow, err := readSettings(settingsPath(home))
	if err != nil {
		return false, err
	}
	for _, r := range allow {
		if r == relayToolsRule {
			return true, nil
		}
	}
	return false, nil
}

// AllowRelayTools adds relay's rule to permissions.allow in agy's settings,
// keeping every other setting and its order. A file it cannot read is left
// alone; a symlinked file is edited where it points.
func AllowRelayTools(home string) error {
	path := settingsPath(home)
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	top, allow, err := readSettings(path)
	if err != nil {
		return err
	}
	for _, r := range allow {
		if r == relayToolsRule {
			return nil
		}
	}
	allowRaw, _ := json.Marshal(append(allow, relayToolsRule))
	perms := []member{}
	for _, m := range top {
		if m.key == "permissions" {
			perms, _ = parseObject(m.val) // readSettings checked it
		}
	}
	perms = setMember(perms, "allow", allowRaw)
	top = setMember(top, "permissions", encodeObject(perms))
	var out bytes.Buffer
	if err := json.Indent(&out, encodeObject(top), "", "  "); err != nil {
		return err
	}
	out.WriteByte('\n')
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(path, out.Bytes(), mode)
}

// readSettings parses agy's settings: its top-level members in order, and
// permissions.allow. A missing or blank file is empty settings.
func readSettings(path string) (top []member, allow []string, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(bytes.TrimSpace(data)) == 0) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	bad := func(what string) error { return fmt.Errorf("%s: %s", path, what) }
	if top, err = parseObject(data); err != nil {
		return nil, nil, bad("not a JSON object")
	}
	for _, m := range top {
		if m.key != "permissions" {
			continue
		}
		perms, err := parseObject(m.val)
		if err != nil {
			return nil, nil, bad(`"permissions" is not an object`)
		}
		for _, p := range perms {
			if p.key == "allow" && json.Unmarshal(p.val, &allow) != nil {
				return nil, nil, bad(`"permissions.allow" is not a list of strings`)
			}
		}
	}
	return top, allow, nil
}

// member is one key of a JSON object, its value kept verbatim.
type member struct {
	key string
	val json.RawMessage
}

// parseObject reads a JSON object's members in order.
func parseObject(data []byte) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not an object")
	}
	var out []member
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := t.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		out = append(out, member{key, val})
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return out, nil
}

// setMember replaces key's value, or appends key.
func setMember(ms []member, key string, val json.RawMessage) []member {
	for i := range ms {
		if ms[i].key == key {
			ms[i].val = val
			return ms
		}
	}
	return append(ms, member{key, val})
}

func encodeObject(ms []member) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range ms {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(m.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.val)
	}
	b.WriteByte('}')
	return b.Bytes()
}
