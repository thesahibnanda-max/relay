// Package roles defines what an agent is for. A role is a Markdown document
// (delivered to the tool as instructions) with optional frontmatter that sets
// policy. Common roles are embedded; anything else can be a path to a file.
package roles

import (
	"embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

//go:embed builtin/*.md
var builtinFS embed.FS

// MaxFileSize bounds a user-supplied role file.
const MaxFileSize = 64 << 10

type Role struct {
	Name        string
	Source      string // "builtin" or the file path
	Description string
	Body        string // instructions for the model (frontmatter removed)

	// Policy (from frontmatter).
	CanInterrupt   bool
	CanBroadcast   bool
	DefaultPreempt string // never | on-p0 | on-p1
}

// Builtin returns the names of the embedded roles, sorted.
func Builtin() []string {
	entries, _ := builtinFS.ReadDir("builtin")
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".md"))
	}
	sort.Strings(names)
	return names
}

// LooksLikePath reports whether a role spec should be treated as a file.
func LooksLikePath(spec string) bool {
	return strings.ContainsAny(spec, `/\`) || strings.HasSuffix(strings.ToLower(spec), ".md") || strings.HasPrefix(spec, "~")
}

// Resolve turns a role spec (embedded name or file path) into a Role.
// An empty spec yields the neutral "agent" role.
func Resolve(spec string) (Role, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Role{Name: "agent", Source: "builtin", Description: "general-purpose agent", DefaultPreempt: "never"}, nil
	}
	if !LooksLikePath(spec) {
		data, err := builtinFS.ReadFile("builtin/" + strings.ToLower(spec) + ".md")
		if err != nil {
			return Role{}, fmt.Errorf("unknown role %q: use one of %s, or a path to a .md file", spec, strings.Join(Builtin(), ", "))
		}
		return parse(strings.ToLower(spec), "builtin", string(data))
	}

	path := spec
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return Role{}, err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	f, err := os.Open(path)
	if err != nil {
		return Role{}, fmt.Errorf("role file: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil {
		return Role{}, fmt.Errorf("role file: %w", err)
	}
	if len(data) > MaxFileSize {
		return Role{}, fmt.Errorf("role file %s is larger than %d KB", path, MaxFileSize>>10)
	}
	if !utf8.Valid(data) {
		return Role{}, fmt.Errorf("role file %s is not valid UTF-8 text", path)
	}
	abs, _ := filepath.Abs(path)
	name := SanitizeName(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	return parse(name, abs, string(data))
}

// parse splits optional "---" frontmatter (simple key: value lines) from the body.
func parse(name, source, text string) (Role, error) {
	r := Role{Name: name, Source: source, DefaultPreempt: "never"}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	body := text
	if strings.HasPrefix(text, "---\n") {
		if end := strings.Index(text[4:], "\n---"); end >= 0 {
			front := text[4 : 4+end]
			body = strings.TrimPrefix(text[4+end+4:], "\n")
			for _, line := range strings.Split(front, "\n") {
				k, v, ok := strings.Cut(line, ":")
				if !ok {
					continue
				}
				k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
				switch k {
				case "description":
					r.Description = v
				case "can_interrupt":
					r.CanInterrupt = v == "true"
				case "can_broadcast":
					r.CanBroadcast = v == "true"
				case "default_preempt":
					switch v {
					case "never", "on-p0", "on-p1":
						r.DefaultPreempt = v
					default:
						return Role{}, fmt.Errorf("role %s: default_preempt must be never, on-p0 or on-p1 (got %q)", name, v)
					}
				}
			}
		}
	}
	r.Body = strings.TrimSpace(body)
	return r, nil
}

var nonRoleChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// SanitizeName turns a file name into a role name that is safe to show other
// agents (letters, digits, dot, dash, underscore; at most 32 characters).
func SanitizeName(s string) string {
	s = strings.Trim(nonRoleChars.ReplaceAllString(s, "-"), "-._")
	if len(s) > 32 {
		s = strings.Trim(s[:32], "-._")
	}
	if s == "" {
		return "custom"
	}
	return s
}
