package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain removes the fake tool that buildFake compiled, so test runs do not
// leave a binary in the temp directory every time.
func TestMain(m *testing.M) {
	code := m.Run()
	if fakeBin != "" {
		os.RemoveAll(filepath.Dir(fakeBin))
	}
	os.Exit(code)
}
