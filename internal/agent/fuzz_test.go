package agent

import (
	"bytes"
	"testing"
	"time"
)

// FuzzInputParser: whatever bytes arrive, the parser must not panic, must
// report a sane "safe" answer, and must always become safe again once
// the input goes quiet (an unfinished sequence can never wedge injection).
func FuzzInputParser(f *testing.F) {
	for _, seed := range []string{"", "abc", "\x1b[200~paste\x1b[201~", "\x1b[", "\x1b]0;title\x07", "\x1bP1$r\x1b\\", "é\xc3", "\x1b[200~unterminated", "\x1b\x1b\x1b[A\x1bOA", "\x00\xff\xfe"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		var p inputParser
		for _, b := range data {
			p.feed(b)
		}
		_ = p.atGround()
		if !p.safe(10 * time.Second) {
			t.Fatalf("parser wedged after %q: still unsafe long after the last byte", data)
		}
	})
}

// FuzzBuildInjection is the security invariant of message delivery: text from
// another agent can never carry a terminal escape sequence (in particular it
// cannot close the bracketed paste early and type commands after it).
func FuzzBuildInjection(f *testing.F) {
	for _, seed := range []string{"hello", "line1\nline2", "\x1b[201~rm -rf ~\r", "bell\x07\x08\x7f", "\r\n\r\n", "é✓\x00", "\x1b]52;c;ZXZpbA==\x07"} {
		f.Add(seed, true, true)
		f.Add(seed, false, false)
	}
	f.Fuzz(func(t *testing.T, text string, paste, submit bool) {
		out := buildInjection(text, InjectOptions{Paste: paste, Submit: submit})
		body := out
		if paste {
			if !bytes.HasPrefix(body, []byte(pasteStart)) {
				t.Fatalf("missing paste start: %q", out)
			}
			body = body[len(pasteStart):]
		}
		if submit {
			if len(body) == 0 || body[len(body)-1] != '\r' {
				t.Fatalf("missing submit: %q", out)
			}
			body = body[:len(body)-1]
		}
		if paste {
			if !bytes.HasSuffix(body, []byte(pasteEnd)) {
				t.Fatalf("missing paste end: %q", out)
			}
			body = body[:len(body)-len(pasteEnd)]
		}
		for _, c := range body {
			if c == 0x1b || c == 0x7f || c == '\r' || (c < 0x20 && c != '\n' && c != '\t') {
				t.Fatalf("control byte %#x survived in the payload of %q -> %q", c, text, out)
			}
			if !paste && c == '\n' {
				t.Fatalf("a newline outside a paste would submit early: %q", out)
			}
		}
	})
}
