//go:build windows

package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestIsBatchFile(t *testing.T) {
	cases := map[string]bool{
		`C:\npm\codex.cmd`: true,
		`C:\npm\codex.CMD`: true,
		`C:\npm\codex.bat`: true,
		`C:\npm\codex.BAT`: true,
		`C:\npm\codex.exe`: false,
		`C:\npm\codex.ps1`: false,
		`C:\npm\codex`:     false,
	}
	for path, want := range cases {
		if got := IsBatchFile(path); got != want {
			t.Errorf("IsBatchFile(%q) = %v, want %v", path, got, want)
		}
	}
}

var (
	echoArgsOnce sync.Once
	echoArgsBin  string
	echoArgsErr  error
)

// buildEchoArgs compiles testdata/echoargs once per test binary run.
func buildEchoArgs(t *testing.T) string {
	t.Helper()
	echoArgsOnce.Do(func() {
		dir, err := os.MkdirTemp("", "echoargs")
		if err != nil {
			echoArgsErr = err
			return
		}
		echoArgsBin = filepath.Join(dir, "echoargs.exe")
		out, err := exec.Command("go", "build", "-o", echoArgsBin, "../../testdata/echoargs").CombinedOutput()
		if err != nil {
			echoArgsErr = fmt.Errorf("%s: %w", out, err)
		}
	})
	if echoArgsErr != nil {
		t.Skip("cannot build echoargs (is `go` on PATH?):", echoArgsErr)
	}
	return echoArgsBin
}

// TestBatchShimRoundTripsArgumentsThroughRealCmdExe is the primary
// correctness proof for WrapForCmdExe: cmd.exe's own quoting rules are
// notoriously inconsistent, so this is validated end to end against the
// real Win32 launch path (a real .cmd shim, forwarding %* to a real .exe via
// a real cmd.exe /d /s /c invocation), not by inspecting the escaping logic
// alone. Every metacharacter cmd.exe treats specially is exercised, plus the
// user's exact originally-reported string.
func TestBatchShimRoundTripsArgumentsThroughRealCmdExe(t *testing.T) {
	bin := buildEchoArgs(t)
	dir := t.TempDir()
	shim := filepath.Join(dir, "shim.cmd")
	// Shaped exactly like npm's own global-install .cmd shims: forwards %*
	// verbatim to a real interpreter/.exe.
	body := "@echo off\r\n\"" + bin + "\" %*\r\n"
	if err := os.WriteFile(shim, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	args := []string{
		"plain",
		"has space",
		"pipe|here",
		"amp&here",
		"redirect<here>there",
		"caret^here",
		"percent%here%",
		`quote"here`,
		`trailing\`,
		`back\slash\then"quote`,
		"[relay | from NAME (ROLE) | task | normal | msg ID]", // the user's exact reported failing string
	}

	cmdExe, cmdLine := WrapForCmdExe(shim, args)
	cmd := exec.Command(cmdExe)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: cmdLine}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run: %v\noutput: %s", err, out)
	}

	var got []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		var s string
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatalf("bad output line %q: %v", sc.Text(), err)
		}
		got = append(got, s)
	}
	if len(got) != len(args) {
		t.Fatalf("got %d args, want %d\ngot:  %q\nwant: %q", len(got), len(args), got, args)
	}
	for i := range args {
		if got[i] != args[i] {
			t.Errorf("arg %d: got %q, want %q", i, got[i], args[i])
		}
	}
}
