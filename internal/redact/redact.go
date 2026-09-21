// Package redact masks secrets in text before it crosses from one agent to
// another (relay_get_context). It is a safety net, not a guarantee: it catches
// the common, recognisable shapes of credentials.
package redact

import (
	"regexp"
	"strings"
)

type rule struct {
	re   *regexp.Regexp
	repl string
}

var rules = []rule{
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(-----END [A-Z ]*PRIVATE KEY-----|$)`), "[REDACTED:private-key]"},
	{regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}`), "[REDACTED:api-key]"},
	{regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{30,}`), "[REDACTED:github-token]"},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`), "[REDACTED:github-token]"},
	{regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{16,}`), "[REDACTED:gitlab-token]"},
	{regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), "[REDACTED:aws-key-id]"},
	{regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), "[REDACTED:slack-token]"},
	{regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), "[REDACTED:google-key]"},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), "[REDACTED:jwt]"},
	{regexp.MustCompile(`(?i)(authorization:\s*(?:bearer|basic)\s+)[A-Za-z0-9._~+/=-]{8,}`), "${1}[REDACTED]"},
	{regexp.MustCompile(`(://[^/\s:@]+:)[^/\s@]{3,}(@)`), "${1}[REDACTED]${2}"},
}

// assign matches key = value assignments for names that hold secrets; it
// keeps the name and hides the value.
var assign = regexp.MustCompile(`(?i)(\b[A-Za-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)[A-Za-z0-9_.-]*["']?\s*[:=]\s*)("[^"\n]{4,}"|'[^'\n]{4,}'|[^\s"',;]{6,})`)

const masked = "[REDACTED"

// Text returns s with recognisable secrets masked. It is idempotent.
func Text(s string) string {
	for _, r := range rules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	return assign.ReplaceAllStringFunc(s, func(m string) string {
		sub := assign.FindStringSubmatch(m)
		if strings.HasPrefix(sub[2], masked) {
			return m // already masked
		}
		return sub[1] + "[REDACTED]"
	})
}
