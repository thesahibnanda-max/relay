package redact

import (
	"testing"
	"unicode/utf8"
)

func FuzzRedact(f *testing.F) {
	for _, s := range []string{"", "password=hunter2hunter2", "sk-ant-api03-abcdefghijklmnopqrstuvwxyz", "-----BEGIN PRIVATE KEY-----", "Authorization: Bearer abcdefgh12345678", "https://u:secretpw@host/", "é✓ token: \"a b c d\""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := Text(s)
		if utf8.ValidString(s) && !utf8.ValidString(out) {
			t.Fatalf("redaction broke UTF-8: %q -> %q", s, out)
		}
		if again := Text(out); again != out {
			t.Fatalf("not idempotent: %q -> %q -> %q", s, out, again)
		}
	})
}
