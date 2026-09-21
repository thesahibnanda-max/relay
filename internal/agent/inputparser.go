package agent

import "time"

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
