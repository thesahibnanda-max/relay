package cli

import (
	"os"
	"testing"
)

// TestMain removes the relay and fake-tool binaries the end-to-end tests built.
func TestMain(m *testing.M) {
	code := m.Run()
	if binDir != "" {
		os.RemoveAll(binDir)
	}
	os.Exit(code)
}
