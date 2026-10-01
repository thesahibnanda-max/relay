package collab

import (
	"context"
	"strings"
	"time"

	"github.com/thesahibnanda-max/relay/internal/state"
	"github.com/thesahibnanda-max/relay/internal/transcript"
)

// submitWait is how long a tool may take to report an accepted prompt.
var submitWait = 4 * time.Second

const submitRetries = 2 // extra Enter presses before giving up

// submitter is what confirmSubmit needs of the running tool (*agent.Handle).
type submitter interface {
	Screen() []string
	Snapshot() state.Snapshot
	PressEnter(ctx context.Context) error
}

// SetVerifySubmit turns on checking that the tool accepted each injected
// message (only for tools that report accepted prompts - see Accepted).
func (s *Session) SetVerifySubmit(on bool) { s.verifySubmit.Store(on) }

// Accepted is called when the tool reports it accepted a prompt (agy logs
// every one). Relay message headers in it prove the model received them.
func (s *Session) Accepted(text string) {
	s.acceptMu.Lock()
	s.acceptedAt = time.Now()
	s.acceptMu.Unlock()
	s.bus.Confirm(transcript.MsgIDs(text))
}

func (s *Session) acceptedSince(t time.Time) bool {
	s.acceptMu.Lock()
	defer s.acceptMu.Unlock()
	return !s.acceptedAt.Before(t)
}

// confirmSubmit waits for the tool to report the injected prompt accepted. If
// it does not, and the text is still sitting in the tool's input box (typed
// but not submitted), it presses Enter again. A message the tool queued or a
// dialog swallowed is not in the input box, so it is never resubmitted into
// something else - and nothing is pressed while a dialog is open, where Enter
// would answer it.
func (s *Session) confirmSubmit(h submitter, text string, at time.Time) {
	for try := 0; try <= submitRetries; try++ {
		deadline := time.Now().Add(submitWait)
		for time.Now().Before(deadline) {
			if s.acceptedSince(at) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		if try == submitRetries || h.Snapshot().State == state.Dialog || !inputHolds(h.Screen(), text) {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := h.PressEnter(ctx)
		cancel()
		if err != nil {
			return
		}
		at = time.Now()
	}
}

// inputHolds reports whether the tool's input box (agy draws it between the
// last two full-width rules above its status line) still shows text,
// either verbatim or as the "[Pasted text #N ...]" placeholder agy uses for
// long pastes.
func inputHolds(screen []string, text string) bool {
	var rules []int
	for i, l := range screen {
		if t := strings.TrimSpace(l); len(t) >= 20 && strings.Trim(t, "─") == "" {
			rules = append(rules, i)
		}
	}
	if len(rules) < 2 {
		return false
	}
	box := strings.Join(screen[rules[len(rules)-2]+1:rules[len(rules)-1]], "\n")
	box = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(box), ">"))
	if box == "" {
		return false
	}
	if strings.Contains(box, "[Pasted text #") {
		return true
	}
	first := []rune(strings.TrimSpace(strings.SplitN(strings.TrimSpace(text), "\n", 2)[0]))
	if len(first) > 24 {
		first = first[:24]
	}
	return len(first) > 0 && strings.Contains(box, string(first))
}
