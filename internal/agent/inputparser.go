package agent

import (
	"strconv"
	"strings"
	"time"
)

// escTimeout is how long a lone ESC must sit unfollowed before we treat it as
// the Escape key rather than the start of a sequence.
const escTimeout = 50 * time.Millisecond

// A real terminal writes each sequence in one piece, so a sequence left open
// this long was abandoned (or is garbage). Treat it as finished rather than
// blocking injection forever. Pastes get longer: slow terminals stream them.
const (
	staleTimeout      = time.Second
	staleTimeoutPaste = 5 * time.Second
)

type parseState uint8

const (
	psGround    parseState = iota
	psEsc                  // after ESC
	psEscInter             // ESC + intermediate bytes (0x20-0x2f)
	psCSI                  // ESC [ ...
	psSS3                  // ESC O <final>
	psString               // OSC/DCS/APC/PM/SOS until BEL or ST
	psStringEsc            // ESC seen inside a string (maybe ST)
	psPaste                // inside a bracketed paste
)

// inputParser tracks just enough of the user's outgoing byte stream to answer
// one question: "is the user in the middle of something?" It never modifies
// bytes. Chunk boundaries are arbitrary, so state carries across Feed calls.
type inputParser struct {
	st        parseState
	utf8Left  int    // continuation bytes still expected
	csiParams []byte // params of the CSI being read (to spot 200~)
	pasteEnd  int    // progress matching ESC [ 2 0 1 ~ inside a paste
}

var pasteEndSeq = []byte{0x1b, '[', '2', '0', '1', '~'}

// keyEvent is what a fed byte completed, as far as the draft in the tool's
// input box is concerned.
type keyEvent uint8

const (
	evNone    keyEvent = iota
	evText             // a printable character: the draft is now non-empty
	evPaste            // a paste began: same
	evHistory          // Up/Down: the tool may have recalled history into the box
	evSubmit           // Enter: the draft was sent
	evCancel           // Ctrl+C: the draft was cleared
)

// atGround reports whether the next byte starts a fresh key.
func (p *inputParser) atGround() bool { return p.st == psGround && p.utf8Left == 0 }

func (p *inputParser) feed(b byte) keyEvent {
	if p.st == psPaste {
		if b == pasteEndSeq[p.pasteEnd] {
			p.pasteEnd++
			if p.pasteEnd == len(pasteEndSeq) {
				p.st, p.pasteEnd = psGround, 0
			}
		} else if b == pasteEndSeq[0] {
			p.pasteEnd = 1
		} else {
			p.pasteEnd = 0
		}
		return evNone
	}

	// Continuation of a multi-byte UTF-8 character.
	if p.utf8Left > 0 {
		if b&0xC0 == 0x80 {
			p.utf8Left--
			return evNone
		}
		p.utf8Left = 0 // malformed: fall through and treat b normally
	}
	ev := evNone

	switch p.st {
	case psGround:
		switch {
		case b == 0x1b:
			p.st = psEsc
		case b == '\r':
			ev = evSubmit
		case b == 0x03:
			ev = evCancel
		case b >= 0xF0 && b <= 0xF4:
			p.utf8Left, ev = 3, evText
		case b >= 0xE0:
			p.utf8Left, ev = 2, evText
		case b >= 0xC2:
			p.utf8Left, ev = 1, evText
		case b >= 0x20 && b < 0x7f:
			ev = evText
		}
	case psEsc:
		switch {
		case b == '[':
			p.st, p.csiParams = psCSI, p.csiParams[:0]
		case b == 'O':
			p.st = psSS3
		case b == ']' || b == 'P' || b == '_' || b == '^' || b == 'X':
			p.st = psString
		case b >= 0x20 && b <= 0x2f:
			p.st = psEscInter
		case b == 0x1b:
			// ESC ESC: the first was a key (or Alt+Esc); stay in psEsc.
		default:
			p.st = psGround // ESC <char>: Alt+key
		}
	case psEscInter:
		if b >= 0x30 && b <= 0x7e {
			p.st = psGround
		}
	case psCSI:
		switch {
		case b >= 0x40 && b <= 0x7e:
			switch {
			case b == '~' && string(p.csiParams) == "200":
				p.st, p.pasteEnd = psPaste, 0
				ev = evPaste
			case (b == 'A' || b == 'B') && (len(p.csiParams) == 0 || string(p.csiParams) == "1"):
				p.st, ev = psGround, evHistory
			default:
				p.st = psGround
				if code, mods, ok := encodedKey(p.csiParams, b); ok {
					ev = keyMeaning(code, mods)
				}
			}
		case b == 0x1b:
			p.st = psEsc // aborted sequence
		default:
			p.csiParams = append(p.csiParams, b)
		}
	case psSS3:
		p.st = psGround
		if b == 'A' || b == 'B' {
			ev = evHistory
		}
	case psString:
		switch b {
		case 0x07:
			p.st = psGround
		case 0x1b:
			p.st = psStringEsc
		}
	case psStringEsc:
		if b == '\\' {
			p.st = psGround
		} else if b == 0x1b {
			// stay
		} else {
			p.st = psString
		}
	}
	return ev
}

// encodedKey decodes a key sent in one of the extended keyboard encodings
// agy switches on (confirmed live: kitty's CSI >1u "disambiguate" and
// xterm's modifyOtherKeys CSI >4;2m): CSI code[:alt][;mods[:event][;text]] u
// and CSI 27;mods;code ~. mods is 1 + a bitmask (shift 1, alt 2, ctrl 4);
// a kitty key-release event (event 3) is reported as not a key.
func encodedKey(params []byte, final byte) (code, mods int, ok bool) {
	fields := strings.Split(string(params), ";")
	num := func(s string) (int, bool) {
		s, _, _ = strings.Cut(s, ":")
		n, err := strconv.Atoi(s)
		return n, err == nil
	}
	mods = 1
	switch final {
	case 'u':
		if len(fields) == 0 || strings.HasPrefix(fields[0], "?") || strings.HasPrefix(fields[0], ">") ||
			strings.HasPrefix(fields[0], "<") || strings.HasPrefix(fields[0], "=") {
			return 0, 0, false // a mode query/push/pop, not a key
		}
		if code, ok = num(fields[0]); !ok {
			return 0, 0, false
		}
		if len(fields) > 1 && fields[1] != "" {
			if mods, ok = num(fields[1]); !ok {
				return 0, 0, false
			}
			if _, ev, found := strings.Cut(fields[1], ":"); found && ev == "3" {
				return 0, 0, false
			}
		}
		return code, mods, true
	case '~':
		if len(fields) == 3 && fields[0] == "27" {
			m, ok1 := num(fields[1])
			c, ok2 := num(fields[2])
			return c, m, ok1 && ok2
		}
	}
	return 0, 0, false
}

// keyMeaning is what an encoded key means for the draft.
func keyMeaning(code, mods int) keyEvent {
	bits := mods - 1
	ctrl, alt := bits&4 != 0, bits&2 != 0
	switch {
	case code == 13 && !ctrl && !alt && bits&1 == 0:
		return evSubmit
	case (code == 'c' || code == 'C') && ctrl:
		return evCancel
	case !ctrl && !alt && (code >= 0x20 && code < 0x7f || code >= 0xa0 && code < 0xe000):
		return evText // kitty's private-use range (0xe000+) is function keys
	}
	return evNone
}

// safe reports whether injecting bytes now cannot corrupt what the user is
// typing. A lone trailing ESC counts as safe once escTimeout has elapsed.
func (p *inputParser) safe(sinceLastByte time.Duration) bool {
	stale := staleTimeout
	if p.st == psPaste {
		stale = staleTimeoutPaste
	}
	if sinceLastByte >= stale {
		return true
	}
	if p.utf8Left > 0 {
		return false
	}
	switch p.st {
	case psGround:
		return true
	case psEsc:
		return sinceLastByte >= escTimeout
	default:
		return false
	}
}
