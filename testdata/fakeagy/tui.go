package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"
)

// The interactive session. Its screen follows real agy's layout closely
// enough for relay's detectors: history, then (while busy) a spinner and
// queued messages, then the input box between two full-width rules, then a
// status line ("? for shortcuts" idle, "esc to cancel" busy); dialogs are a
// title, numbered options and an "↑/↓ Navigate" hint, where Enter picks the
// highlighted option (so text pasted into one is lost and its Enter answers
// it - exactly what real agy does).
//
// Brain: a submitted prompt is answered "ok", or the number when it asks
// "What is A plus|times B"; directive lines act like the model deciding to
// use a tool:
//
//	RELAY_CALL <tool> <json args>   call an MCP tool (asks permission unless allowed)
//	RUN <command>                   ask "Run this command?" (nothing is run)
//	SLEEP <ms>                      stay busy that long

var (
	evMu sync.Mutex
	evW  *os.File
)

func event(ev string, kv ...string) {
	evMu.Lock()
	defer evMu.Unlock()
	if evW == nil {
		return
	}
	m := map[string]string{"ev": ev, "t": time.Now().UTC().Format(time.RFC3339Nano)}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	evW.Write(append(b, '\n'))
}

type dialog struct {
	title   string
	options []string
	sel     int
	choose  chan int // option index chosen, or -1 for Esc
}

type tui struct {
	mu       sync.Mutex
	out      *os.File
	logw     *os.File
	cwd      string
	history  []string
	input    string
	pastes   int
	queue    []string
	busy     bool
	cancel   bool
	dlg      *dialog
	trusting bool
	ready    bool // login finished
	ctrlD    bool
	conv     *conversation
	servers  map[string]*mcpServer
	allowAll bool
	done     chan int
	turnWake chan struct{}
}

var agyValueFlags = map[string]bool{
	"p": true, "print": true, "prompt": true, "i": true, "prompt-interactive": true,
	"conversation": true, "add-dir": true, "agent": true, "effort": true, "input-format": true,
	"json-schema": true, "log-file": true, "mode": true, "model": true, "output-format": true,
	"print-timeout": true, "project": true, "v": true,
}

func runTUI(args []string) int {
	var initial, logFile, resumeID string
	resume := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || !strings.HasPrefix(a, "-") {
			break
		}
		name, value, has := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if agyValueFlags[name] && !has && i+1 < len(args) {
			i++
			value = args[i]
		}
		switch name {
		case "i", "prompt-interactive":
			initial = value
		case "log-file":
			logFile = value
		case "c", "continue":
			resume = true
		case "conversation":
			resume, resumeID = true, value
		}
	}
	if p := os.Getenv("FAKEAGY_LOG"); p != "" {
		evW, _ = os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	}
	t := &tui{out: os.Stdout, done: make(chan int, 1), turnWake: make(chan struct{}, 1)}
	t.cwd, _ = os.Getwd()
	if logFile == "" {
		logFile = filepath.Join(geminiDir(), "antigravity-cli", "log", "cli-"+time.Now().Format("20060102_150405")+".log")
	}
	os.MkdirAll(filepath.Dir(logFile), 0o755)
	t.logw, _ = os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	t.allowAll = mcpAllowed()

	if old, err := term.MakeRaw(int(os.Stdin.Fd())); err == nil {
		defer term.Restore(int(os.Stdin.Fd()), old)
	}
	fmt.Fprint(t.out, "\x1b[?2004h\x1b[>1u\x1b[>4;2m") // bracketed paste, kitty keyboard, modifyOtherKeys - as agy does
	defer fmt.Fprint(t.out, "\x1b[<1u\x1b[>4m\x1b[?2004l")

	t.servers = startMCPServers() // spawned at startup, before trust - as agy does
	for name, s := range t.servers {
		event("mcp_ready", "server", name, "tools", strings.Join(s.tools, ","))
	}
	defer func() {
		for _, s := range t.servers {
			s.stop()
		}
		t.conv.close()
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP)
	go func() { <-sigs; t.done <- 0 }()

	t.trusting = os.Getenv("FAKEAGY_TRUST") == "prompt"
	if resume {
		if c, err := resumeConversation(resumeID); err == nil {
			t.conv = c
			t.log("common.go:401] Resuming conversation " + c.id)
		}
	}
	t.render()
	go t.readInput()
	go t.turnLoop()
	go func() {
		if v, err := strconv.Atoi(os.Getenv("FAKEAGY_LOGIN_MS")); err == nil && v > 0 {
			time.Sleep(time.Duration(v) * time.Millisecond)
		}
		t.mu.Lock()
		t.ready = true
		if initial != "" {
			t.queue = append([]string{"\x00" + initial}, t.queue...) // -i: no HandleUserInput line, as agy
		}
		t.mu.Unlock()
		event("ready")
		t.wake()
	}()
	code := <-t.done
	event("exit")
	return code
}

// mcpAllowed reports whether agy's own settings allow relay's tools.
func mcpAllowed() bool {
	if os.Getenv("FAKEAGY_ALLOW_MCP") == "1" {
		return true
	}
	data, _ := os.ReadFile(filepath.Join(geminiDir(), "antigravity-cli", "settings.json"))
	return strings.Contains(string(data), `"mcp(relay/*)"`)
}

func (t *tui) log(line string) {
	if t.logw != nil {
		fmt.Fprintf(t.logw, "I%s %6d %s\n", time.Now().Format("0102 15:04:05.000000"), os.Getpid(), line)
	}
}

func (t *tui) wake() {
	select {
	case t.turnWake <- struct{}{}:
	default:
	}
}

const rule = "────────────────────────────────────────────────────────────────────────"

func (t *tui) render() {
	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	if t.trusting {
		b.WriteString("Accessing workspace:\r\n\r\n" + t.cwd + "\r\n\r\nDo you trust the contents of this project?\r\n\r\n> Yes, I trust this folder\r\n  No, exit\r\n\r\n  ↑/↓ Navigate · enter Confirm\r\n")
		t.out.WriteString(b.String())
		return
	}
	b.WriteString("Antigravity CLI 1.2.13 (fake)\r\n\r\n")
	h := t.history
	if len(h) > 14 {
		h = h[len(h)-14:]
	}
	for _, l := range h {
		b.WriteString(l + "\r\n")
	}
	if t.dlg != nil {
		b.WriteString("\r\n" + t.dlg.title + "\r\n")
		for i, o := range t.dlg.options {
			mark := "  "
			if i == t.dlg.sel {
				mark = "> "
			}
			fmt.Fprintf(&b, "%s%d. %s\r\n", mark, i+1, o)
		}
		b.WriteString("\r\n  ↑/↓ Navigate · tab Amend\r\nesc to cancel\r\n")
		t.out.WriteString(b.String())
		return
	}
	if t.busy {
		b.WriteString("⣾  Working...\r\n")
	}
	for _, q := range t.queue {
		if !strings.HasPrefix(q, "\x00") {
			b.WriteString("▸ " + firstLine(q) + "\r\n")
		}
	}
	shown := t.input
	if t.pastes > 0 && strings.Count(t.input, "\n") >= 8 {
		shown = fmt.Sprintf("[Pasted text #%d +%d lines]", t.pastes, strings.Count(t.input, "\n")+1)
	}
	b.WriteString(rule + "\r\n> " + strings.ReplaceAll(shown, "\n", "\r\n  ") + "\r\n" + rule + "\r\n")
	switch {
	case t.ctrlD:
		b.WriteString("press ctrl+d again to exit")
	case t.busy:
		b.WriteString("esc to cancel")
	case len(t.queue) > 0 && t.ready:
		b.WriteString("  Press up to edit queued messages")
	default:
		b.WriteString("? for shortcuts")
	}
	b.WriteString("\r\n")
	t.out.WriteString(b.String())
}

func firstLine(s string) string { return strings.SplitN(s, "\n", 2)[0] }

// readInput is agy's input loop: bracketed paste, Enter, Esc, Ctrl+C/D, arrows.
func (t *tui) readInput() {
	buf := make([]byte, 4096)
	var paste *strings.Builder
	var pending []byte
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			for len(pending) > 0 {
				if paste != nil {
					if i := strings.Index(string(pending), "\x1b[201~"); i >= 0 {
						paste.WriteString(string(pending[:i]))
						pending = pending[i+6:]
						t.onPaste(paste.String())
						paste = nil
						continue
					}
					paste.WriteString(string(pending))
					pending = nil
					break
				}
				c := pending[0]
				if c == 0x1b {
					if len(pending) == 1 {
						pending = nil
						t.key("esc")
						break
					}
					if strings.HasPrefix(string(pending), "\x1b[200~") {
						paste = &strings.Builder{}
						pending = pending[6:]
						continue
					}
					if pending[1] == '[' {
						j := 2
						for j < len(pending) && (pending[j] < 0x40 || pending[j] > 0x7e) {
							j++
						}
						if j >= len(pending) {
							break // incomplete sequence: wait for more
						}
						seq := string(pending[:j+1])
						pending = pending[j+1:]
						switch seq {
						case "\x1b[A":
							t.key("up")
						case "\x1b[B":
							t.key("down")
						case "\x1b[13u":
							t.key("enter")
						case "\x1b[99;5u":
							t.key("ctrl+c")
						}
						continue
					}
					pending = pending[1:]
					t.key("esc")
					continue
				}
				pending = pending[1:]
				switch c {
				case '\r', '\n':
					t.key("enter")
				case 0x7f, 0x08:
					t.key("backspace")
				case 0x03:
					t.key("ctrl+c")
				case 0x04:
					t.key("ctrl+d")
				default:
					if c >= 0x20 {
						t.typed(string(c))
					}
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (t *tui) onPaste(text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dlg != nil || t.trusting {
		event("paste_lost", "text", text) // pasted into a dialog: agy drops it
		return
	}
	t.input += text
	t.pastes++
	t.ctrlD = false
	t.render()
}

func (t *tui) typed(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dlg != nil {
		if n, err := strconv.Atoi(s); err == nil && n >= 1 && n <= len(t.dlg.options) {
			t.dlg.sel = n - 1
			t.render()
		}
		return
	}
	if t.trusting {
		return
	}
	t.input += s
	t.ctrlD = false
	t.render()
}

func (t *tui) key(k string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if k != "ctrl+d" {
		t.ctrlD = false
	}
	if t.trusting {
		if k == "enter" {
			t.trusting = false
			event("trusted")
		}
		t.render()
		return
	}
	if d := t.dlg; d != nil {
		switch k {
		case "up":
			if d.sel > 0 {
				d.sel--
			}
		case "down":
			if d.sel < len(d.options)-1 {
				d.sel++
			}
		case "enter":
			t.dlg = nil
			event("choice", "title", d.title, "option", strconv.Itoa(d.sel+1))
			d.choose <- d.sel
		case "esc":
			t.dlg = nil
			event("choice", "title", d.title, "option", "esc")
			d.choose <- -1
		}
		t.render()
		return
	}
	switch k {
	case "enter":
		text := strings.TrimSpace(t.input)
		t.input, t.pastes = "", 0
		if text == "" {
			break
		}
		if text == "/new" || text == "/clear" {
			t.conv.close()
			c, err := createConversation(t.cwd)
			if err == nil {
				t.conv = c
				t.log("server.go:1248] Created conversation " + c.id)
			}
			t.history = append(t.history, "(new conversation)")
			break
		}
		if text == "/quit" {
			t.done <- 0
			return
		}
		t.queue = append(t.queue, text)
		if t.busy || !t.ready {
			event("queued", "text", text)
		}
		t.wake()
	case "backspace":
		if r := []rune(t.input); len(r) > 0 {
			t.input = string(r[:len(r)-1])
		}
	case "esc":
		if t.busy {
			t.cancel = true
		}
	case "ctrl+c":
		switch {
		case t.input != "":
			t.input, t.pastes = "", 0
		case t.busy:
			t.cancel = true
		}
	case "ctrl+d":
		if t.ctrlD {
			t.done <- 0
			return
		}
		t.ctrlD = true
	}
	t.render()
}

// turnLoop runs queued prompts one at a time once agy is ready.
func (t *tui) turnLoop() {
	for range t.turnWake {
		for {
			t.mu.Lock()
			if !t.ready || t.busy || t.trusting || len(t.queue) == 0 {
				t.mu.Unlock()
				break
			}
			text := t.queue[0]
			t.queue = t.queue[1:]
			t.busy, t.cancel = true, false
			t.mu.Unlock()
			t.runTurn(text)
			t.mu.Lock()
			t.busy = false
			t.render()
			t.mu.Unlock()
		}
	}
}

var (
	arith     = regexp.MustCompile(`(?i)what is (-?\d+) (plus|times) (-?\d+)`)
	relayCall = regexp.MustCompile(`(?m)^RELAY_CALL (\S+) (\{.*\})\s*$`)
	runCmd    = regexp.MustCompile(`(?m)^RUN (.+)$`)
	sleepMS   = regexp.MustCompile(`(?m)^SLEEP (\d+)$`)
)

func turnDelay() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("FAKEAGY_TURN_MS")); err == nil && v >= 0 {
		return time.Duration(v) * time.Millisecond
	}
	return 150 * time.Millisecond
}

// runTurn plays one turn, writing exactly the step sequence real agy writes.
func (t *tui) runTurn(text string) {
	initial := strings.HasPrefix(text, "\x00")
	text = strings.TrimPrefix(text, "\x00")
	t.mu.Lock()
	if !initial {
		t.log("input_loop.go:107] HandleUserInput called with text: " + strconv.Quote(text))
	}
	if t.conv == nil {
		c, err := createConversation(t.cwd)
		if err != nil {
			t.mu.Unlock()
			return
		}
		t.conv = c
		t.log("server.go:1248] Created conversation " + c.id)
	}
	conv := t.conv
	t.history = append(t.history, rule, "> "+strings.ReplaceAll(text, "\n", "\r\n  "))
	conv.step(stepUser, statusDone, userPayload(text))
	t.render()
	t.mu.Unlock()
	event("submit", "text", text)

	cancelled := func() bool {
		t.mu.Lock()
		defer t.mu.Unlock()
		return t.cancel
	}
	pause := func(d time.Duration) bool {
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			if cancelled() {
				return false
			}
			time.Sleep(10 * time.Millisecond)
		}
		return !cancelled()
	}
	end := func(outcome int, note string) {
		t.mu.Lock()
		if note != "" {
			t.history = append(t.history, note)
		}
		conv.endTurn(outcome)
		t.mu.Unlock()
		event("turn_end", "outcome", strconv.Itoa(outcome))
	}
	// ask shows a permission dialog with the tool step in status 9 and
	// reports whether it was allowed.
	ask := func(title, name string, opts []string) (int, bool) {
		t.mu.Lock()
		conv.step(stepModel, statusDone, toolPlanPayload(name))
		idx := conv.step(stepTool, statusAsk, nil)
		d := &dialog{title: title, options: opts, choose: make(chan int, 1)}
		t.dlg = d
		t.render()
		t.mu.Unlock()
		event("dialog", "title", title)
		choice := <-d.choose
		if choice < 0 || !strings.HasPrefix(opts[choice], "Yes") {
			t.mu.Lock()
			conv.setStatus(idx, statusCancelled)
			t.mu.Unlock()
			return idx, false
		}
		return idx, true
	}

	if !pause(turnDelay()) {
		end(2, "  ⎿  Interrupted")
		return
	}
	if m := sleepMS.FindStringSubmatch(text); m != nil {
		ms, _ := strconv.Atoi(m[1])
		if !pause(time.Duration(ms) * time.Millisecond) {
			end(2, "  ⎿  Interrupted")
			return
		}
	}
	for _, m := range runCmd.FindAllStringSubmatch(text, -1) {
		idx, ok := ask("Run this command?", "run_command",
			[]string{"Yes, run command", "Yes, and always allow in this conversation", "Yes, and always allow (Persist to settings.json)", "No, cancel"})
		if !ok {
			end(2, "  ⎿  Interrupted")
			return
		}
		t.mu.Lock()
		conv.setStatus(idx, statusDone)
		t.history = append(t.history, "● Bash("+m[1]+")")
		t.mu.Unlock()
	}
	for _, m := range relayCall.FindAllStringSubmatch(text, -1) {
		tool, rawArgs := m[1], m[2]
		srv := t.serverFor(tool)
		var idx int
		if t.allowAll {
			t.mu.Lock()
			conv.step(stepModel, statusDone, toolPlanPayload(tool))
			idx = conv.step(stepTool, statusGenerate, nil)
			t.mu.Unlock()
		} else {
			var ok bool
			idx, ok = ask("Allow calling this tool?", tool, []string{"Yes, allow tool call", "Yes, and always allow tool in this conversation",
				"Yes, and always allow tool (Persist to settings.json)", "No, deny tool call"})
			if !ok {
				end(2, "  ⎿  Interrupted")
				return
			}
		}
		result, isErr := "no MCP server offers "+tool, true
		if srv != nil {
			var args map[string]any
			json.Unmarshal([]byte(rawArgs), &args)
			result, isErr = srv.call(tool, args)
		}
		event("mcp", "tool", tool, "result", result, "error", strconv.FormatBool(isErr))
		t.mu.Lock()
		conv.setStatus(idx, statusDone)
		t.history = append(t.history, "● relay/"+tool+"("+firstLine(rawArgs)+")")
		t.mu.Unlock()
	}
	answer := "ok"
	if m := arith.FindStringSubmatch(text); m != nil {
		a, _ := strconv.Atoi(m[1])
		b, _ := strconv.Atoi(m[3])
		if strings.EqualFold(m[2], "plus") {
			answer = strconv.Itoa(a + b)
		} else {
			answer = strconv.Itoa(a * b)
		}
	}
	t.mu.Lock()
	idx := conv.step(stepModel, statusGenerate, nil)
	t.mu.Unlock()
	if !pause(turnDelay()) {
		t.mu.Lock()
		conv.setStatus(idx, statusCancelled)
		t.mu.Unlock()
		end(2, "  ⎿  Interrupted")
		return
	}
	t.mu.Lock()
	conv.setPayload(idx, answerPayload(answer))
	conv.setStatus(idx, statusDone)
	t.history = append(t.history, "", "  "+answer, "")
	t.mu.Unlock()
	event("answer", "text", answer)
	end(4, "")
}

// serverFor finds the spawned MCP server offering tool.
func (t *tui) serverFor(tool string) *mcpServer {
	for _, s := range t.servers {
		for _, n := range s.tools {
			if n == tool {
				return s
			}
		}
	}
	return nil
}
