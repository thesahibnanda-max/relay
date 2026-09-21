// fakeagent is a scripted stand-in for a TUI coding agent (Claude/Codex) used
// to test Relay deterministically. It runs in raw mode on a PTY, enables
// bracketed paste, shows a "> " prompt, treats Enter as "submit", goes busy
// for a while, optionally asks a permission question, then answers.
//
// Environment:
//
//	FAKE_BUSY_MS   how long a turn takes (default 200)
//	FAKE_DIALOG=1  every turn first asks "Allow? (y/n)" and waits for y/n
//	FAKE_LOG       file: JSON lines of ground truth (state changes, keys, submits)
//
// Type /quit (or Ctrl+D) to exit 0; Ctrl+C exits 130.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

var logf *os.File

func record(ev string, kv ...string) {
	if logf == nil {
		return
	}
	m := map[string]string{"ev": ev, "t": time.Now().UTC().Format(time.RFC3339Nano)}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	logf.Write(append(b, '\n'))
}

func main() {
	if p := os.Getenv("FAKE_LOG"); p != "" {
		logf, _ = os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	}
	busy := 200 * time.Millisecond
	if v, err := strconv.Atoi(os.Getenv("FAKE_BUSY_MS")); err == nil {
		busy = time.Duration(v) * time.Millisecond
	}
	dialog := os.Getenv("FAKE_DIALOG") == "1"

	old, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err == nil {
		defer term.Restore(int(os.Stdin.Fd()), old)
	}
	out := os.Stdout
	fmt.Fprint(out, "\x1b[?2004h") // bracketed paste on
	defer fmt.Fprint(out, "\x1b[?2004l")
	fmt.Fprint(out, "fakeagent ready\r\n")

	prompt := func() { record("state", "state", "idle"); fmt.Fprint(out, "> ") }
	prompt()

	var draft []rune
	inPaste := false
	awaitingAnswer := false
	pendingSubmit := ""

	finishTurn := func(text string) {
		record("state", "state", "busy")
		fmt.Fprintf(out, "\r\nthinking...")
		time.Sleep(busy)
		fmt.Fprintf(out, "\r\nreply: %s\r\n", strings.ReplaceAll(text, "\n", "\\n"))
		prompt()
	}

	buf := make([]byte, 4096)
	var esc []byte // sequence being collected
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return
		}
		for _, b := range buf[:n] {
			if len(esc) > 0 {
				esc = append(esc, b)
				if len(esc) == 2 && b != '[' { // ESC <char>: Alt+key
					record("key", "seq", string(esc))
					esc = nil
				} else if len(esc) > 2 && b >= 0x40 && b <= 0x7e { // CSI final byte
					switch s := string(esc); s {
					case "\x1b[200~":
						inPaste = true
					case "\x1b[201~":
						inPaste = false
						record("paste_end")
					default:
						record("key", "seq", s)
					}
					esc = nil
				}
				continue
			}
			switch {
			case b == 0x1b:
				esc = []byte{b}
			case inPaste:
				draft = append(draft, rune(b)) // bytes are ASCII in tests
				if b == '\n' {
					fmt.Fprint(out, "\r\n")
				} else {
					fmt.Fprintf(out, "%c", b)
				}
			case awaitingAnswer && (b == 'y' || b == 'n'):
				awaitingAnswer = false
				record("answer", "value", string(b))
				fmt.Fprintf(out, "%c\r\n", b)
				finishTurn(pendingSubmit)
			case awaitingAnswer:
				// ignore everything else while the dialog is up
			case b == 0x03:
				fmt.Fprint(out, "^C\r\n")
				term.Restore(int(os.Stdin.Fd()), old)
				os.Exit(130)
			case b == 0x04:
				return
			case b == 0x7f:
				if len(draft) > 0 {
					draft = draft[:len(draft)-1]
					fmt.Fprint(out, "\b \b")
				}
			case b == '\r':
				text := string(draft)
				draft = nil
				record("submit", "text", text)
				if text == "/quit" {
					fmt.Fprint(out, "\r\nbye\r\n")
					return
				}
				if dialog {
					pendingSubmit = text
					awaitingAnswer = true
					record("state", "state", "dialog")
					fmt.Fprint(out, "\r\nAllow? (y/n) ")
				} else {
					finishTurn(text)
				}
			default:
				draft = append(draft, rune(b))
				fmt.Fprintf(out, "%c", b)
				record("char", "c", string(rune(b)))
			}
		}
	}
}
