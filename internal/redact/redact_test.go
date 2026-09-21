package redact

import (
	"strings"
	"testing"
)

func TestSecretsAreMasked(t *testing.T) {
	cases := map[string]string{
		"sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789":           "[REDACTED:api-key]",
		"sk-proj-abcdefghijklmnopqrstuvwx":                            "[REDACTED:api-key]",
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789":                    "[REDACTED:github-token]",
		"github_pat_11ABCDEFG0123456789_abcdefghijklmnop":             "[REDACTED:github-token]",
		"AKIAIOSFODNN7EXAMPLE":                                        "[REDACTED:aws-key-id]",
		"xoxb-123456789012-abcdefghij":                                "[REDACTED:slack-token]",
		"AIzaSyA-1234567890abcdefghijklmnopqrstu":                     "[REDACTED:google-key]",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghij": "[REDACTED:jwt]",
	}
	for in, want := range cases {
		got := Text("before " + in + " after")
		if got != "before "+want+" after" {
			t.Errorf("%s -> %q", in, got)
		}
	}
	pem := "x\n-----BEGIN RSA PRIVATE KEY-----\nMIIEow\nabc\n-----END RSA PRIVATE KEY-----\ny"
	if got := Text(pem); got != "x\n[REDACTED:private-key]\ny" {
		t.Errorf("pem: %q", got)
	}
	if got := Text("-----BEGIN PRIVATE KEY-----\ntruncated with no end"); !strings.Contains(got, "[REDACTED:private-key]") || strings.Contains(got, "truncated") {
		t.Errorf("unterminated pem: %q", got)
	}
}

func TestAssignmentsKeepTheNameHideTheValue(t *testing.T) {
	for in, want := range map[string]string{
		`export API_KEY=abcdef123456`:                     `export API_KEY=[REDACTED]`,
		`password: "hunter2hunter2"`:                      `password: [REDACTED]`,
		`{"client_secret": "s3cr3tvalue"}`:                `{"client_secret": [REDACTED]}`,
		`DB_PASSWORD='p@ss w0rd'`:                         `DB_PASSWORD=[REDACTED]`,
		`Authorization: Bearer abcdef0123456789`:          `Authorization: Bearer [REDACTED]`,
		`postgres://admin:sup3rs3cret@db.internal:5432/x`: `postgres://admin:[REDACTED]@db.internal:5432/x`,
		`--token=abcdef123456`:                            `--token=[REDACTED]`,
	} {
		if got := Text(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestOrdinaryTextIsUntouched(t *testing.T) {
	for _, s := range []string{
		"func main() { fmt.Println(\"hello\") }",
		"the token count was 42 and the password field is validated",
		"see https://example.com/path?x=1 and user@example.com",
		"[relay | from bob (qa) | task | normal | msg 01M2XP1EQFJ780GSC4ED8KNECA]",
		"sk-short", "commit 3f2a9c1b7e4d5a6b8c9d0e1f2a3b4c5d6e7f8a9b",
	} {
		if got := Text(s); got != s {
			t.Errorf("changed %q -> %q", s, got)
		}
	}
}
