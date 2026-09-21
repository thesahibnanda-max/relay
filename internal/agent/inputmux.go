package agent

import (
	"bytes"
	"context"
	"io"
	"sync"
	"time"
)

const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
	injectPoll = 5 * time.Millisecond

	// A draft nobody has touched for this long is assumed abandoned. Until
	// then Relay never types a message on top of it: the paste and Enter
	// would merge with, and submit, what the user was writing.
	draftDecay = 10 * time.Minute

	chordTimeout = 2 * time.Second
)

// InjectOptions controls how Inject presents text to the tool.
type InjectOptions struct {
	// Paste wraps the text in bracketed-paste markers so multi-line text lands
	// in the input box as one atomic insertion (only use when the tool has
	// enabled bracketed paste). Without it newlines are flattened to spaces so
	// they cannot submit early.
	Paste bool
	// Submit presses Enter after the text.
	Submit bool
}

// InputMux is the single point where bytes are written to the tool's PTY.
// User keystrokes always go straight through; injected text is only written
// at a moment when it cannot split a sequence the user is midway through.
type InputMux struct {
	mu   sync.Mutex
	w    io.Writer
	p    inputParser
	last time.Time // last user byte
	now  func() time.Time

	draft   bool // the user has unsent text in the tool's input box (best effort)
	draftAt time.Time

	chord        *chordCfg
	chordPending bool
	chordAt      time.Time

	// OnInject, if set, is told what was injected (for logging). Called with
	// the mutex held; must not block.
	OnInject func(p []byte)
}

func NewInputMux(w io.Writer) *InputMux {
	return &InputMux{w: w, now: time.Now}
}

type chordCfg struct {
	prefix byte
	fn     func(key byte)
}

// SetChord reserves prefix as a command key: prefix followed by a, r or i
// calls fn('a'|'r'|'i') and neither byte reaches the tool; prefix twice sends
// it through literally; prefix then anything else passes both through. It only
// triggers at a key boundary, never inside a paste or escape sequence.
func (m *InputMux) SetChord(prefix byte, fn func(key byte)) {
	m.mu.Lock()
	m.chord = &chordCfg{prefix: prefix, fn: fn}
	m.mu.Unlock()
}

// WriteUser forwards user bytes immediately (minus a reserved chord) and
// updates the parser and the draft estimate.
func (m *InputMux) WriteUser(p []byte) (int, error) {
	m.mu.Lock()
	now := m.now()
	out := make([]byte, 0, len(p))
	var fire []byte
	for _, b := range p {
		if m.chord != nil {
			if m.chordPending {
				m.chordPending = false
				if now.Sub(m.chordAt) <= chordTimeout {
					switch b {
					case m.chord.prefix:
						out = m.forward(out, b, now)
						continue
					case 'a', 'r', 'i', 'A', 'R', 'I':
						fire = append(fire, b|0x20)
						continue
					default:
						out = m.forward(out, m.chord.prefix, now)
					}
				} // else: the prefix expired unanswered; drop it and treat b normally
			} else if b == m.chord.prefix && m.p.atGround() {
				m.chordPending, m.chordAt = true, now
				continue
			}
		}
		out = m.forward(out, b, now)
	}
	m.last = now
	var err error
	if len(out) > 0 {
		_, err = m.w.Write(out)
	}
	fn := (*chordCfg)(nil)
	if len(fire) > 0 {
		fn = m.chord
	}
	m.mu.Unlock()
	if fn != nil {
		for _, k := range fire {
			go fn.fn(k)
		}
	}
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// forward appends a byte that will reach the tool, tracking what it means for the draft.
func (m *InputMux) forward(out []byte, b byte, now time.Time) []byte {
	switch m.p.feed(b) {
	case evText, evPaste, evHistory:
		m.draft, m.draftAt = true, now
	case evSubmit, evCancel:
		m.draft = false
	}
	return append(out, b)
}

// DraftDirty reports whether the user probably has unsent text in the tool's
// input box.
func (m *InputMux) DraftDirty() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.draft && m.now().Sub(m.draftAt) < draftDecay
}

// ClearDraft forgets the draft estimate (e.g. keys typed into a dialog were
// answers, not draft text).
func (m *InputMux) ClearDraft() {
	m.mu.Lock()
	m.draft = false
	m.mu.Unlock()
}

// UserQuietFor reports whether the user has sent no bytes for at least d.
func (m *InputMux) UserQuietFor(d time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last.IsZero() || m.now().Sub(m.last) >= d
}

// Inject writes text to the tool as if typed, waiting (until ctx is done) for
// a safe boundary in the user's input. The whole payload is one Write under
// the lock, so user bytes can never interleave with it.
func (m *InputMux) Inject(ctx context.Context, text string, opt InjectOptions) error {
	return m.writeSafe(ctx, buildInjection(text, opt))
}

// Raw writes exact bytes (e.g. a lone Esc) at a safe boundary. Unlike Inject
// nothing is filtered: callers pass only fixed, known sequences.
func (m *InputMux) Raw(ctx context.Context, payload []byte) error {
	return m.writeSafe(ctx, payload)
}

func (m *InputMux) writeSafe(ctx context.Context, payload []byte) error {
	t := time.NewTicker(injectPoll)
	defer t.Stop()
	for {
		m.mu.Lock()
		if m.p.safe(m.now().Sub(m.last)) {
			if m.OnInject != nil {
				m.OnInject(payload)
			}
			_, err := m.w.Write(payload)
			m.mu.Unlock()
			return err
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// buildInjection sanitises text and applies paste/submit framing. All control
// bytes are stripped so injected text can never contain an escape sequence
// (in particular, it cannot close the bracketed paste early).
func buildInjection(text string, opt InjectOptions) []byte {
	text = string(bytes.ReplaceAll([]byte(text), []byte("\r\n"), []byte("\n")))
	clean := make([]byte, 0, len(text)+16)
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c == '\r' || c == '\n':
			if opt.Paste {
				clean = append(clean, '\n')
			} else {
				clean = append(clean, ' ')
			}
		case c == '\t':
			clean = append(clean, '\t')
		case c < 0x20 || c == 0x7f:
			// drop: ESC, BEL, backspace, other C0, DEL
		default:
			clean = append(clean, c)
		}
	}
	var out []byte
	if opt.Paste {
		out = append(out, pasteStart...)
		out = append(out, clean...)
		out = append(out, pasteEnd...)
	} else {
		out = clean
	}
	if opt.Submit {
		out = append(out, '\r')
	}
	return out
}
