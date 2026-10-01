package collab

import (
	"context"
	"strings"
	"testing"
)

type seen struct{ lines, glog int64 }

func (f seen) Seen() (int64, int64) { return f.lines, f.glog }

// If agy's log is missing or not in the format relay knows, relay stops
// waiting for records it will never recognise, and says so.
func TestWatchAgyLogFallsBackToTheScreen(t *testing.T) {
	for _, c := range []struct {
		f    seen
		fall bool
		why  string
	}{
		{seen{0, 0}, true, "wrote nothing"},
		{seen{40, 0}, true, "not in the format"},
		{seen{40, 40}, false, ""},
	} {
		s := New()
		s.tool = "agy"
		s.gateClosed.Store(true)
		s.watchAgyLog(context.Background(), c.f, 0)
		if got := !s.gateClosed.Load(); got != c.fall {
			t.Fatalf("%+v: gate opened=%v, want %v", c.f, got, c.fall)
		}
		w := s.Warnings()
		if c.fall != (len(w) == 1) || (c.fall && !strings.Contains(w[0], c.why)) {
			t.Fatalf("%+v: warnings %q", c.f, w)
		}
	}
}
