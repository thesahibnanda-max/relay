//go:build windows

package agent

import (
	"os"
	"testing"
)

// TestWinsizeNeedsAnOutputHandleNotInput locks in, permanently, the exact API
// distinction root-causing the "terminal renders at a tiny fixed size"
// Windows bug: GetConsoleScreenBufferInfo (what winsize calls via
// term.GetSize) requires an output/screen-buffer handle and is documented to
// fail on an input handle - confirmed live. agent.go must query cfg.Out,
// never cfg.In, for terminal size; if this test ever starts failing (or the
// CONIN$ case starts unexpectedly succeeding), that fix needs re-examining.
func TestWinsizeNeedsAnOutputHandleNotInput(t *testing.T) {
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		t.Skip("no real console attached to this test process:", err)
	}
	defer in.Close()
	out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		t.Skip("no real console attached to this test process:", err)
	}
	defer out.Close()

	if _, _, ok := winsize(in); ok {
		t.Error("winsize(CONIN$) unexpectedly succeeded - GetConsoleScreenBufferInfo's " +
			"documented input-handle restriction may have changed; re-examine agent.go's use of cfg.Out")
	}
	if _, _, ok := winsize(out); !ok {
		t.Error("winsize(CONOUT$) failed - a real console output handle should always report a size")
	}
}
