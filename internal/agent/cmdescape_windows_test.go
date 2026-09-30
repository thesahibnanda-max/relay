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
		skipOrFail(t, "echoargs", echoArgsErr)
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

// TestParseNpmCmdShimFindsTheRealTarget uses a synthetic shim shaped exactly
// like npm's own cmd-shim template (confirmed live against a real, unmodified
// codex.cmd) with a fake "node.exe" (echoargs, standing in so the test needs
// no real Node install) living right next to it, matching the common global-
// install layout.
func TestParseNpmCmdShimFindsTheRealTarget(t *testing.T) {
	echoArgs := buildEchoArgs(t)
	dir := t.TempDir()

	fakeNode := filepath.Join(dir, "node.exe")
	if err := copyFile(echoArgs, fakeNode); err != nil {
		t.Fatal(err)
	}
	scriptDir := filepath.Join(dir, "node_modules", "@openai", "codex", "bin")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(scriptDir, "codex.js")
	if err := os.WriteFile(script, []byte("// stand-in"), 0o644); err != nil {
		t.Fatal(err)
	}

	shim := filepath.Join(dir, "codex.cmd")
	body := "@ECHO off\r\n" +
		"GOTO start\r\n" +
		":find_dp0\r\n" +
		"SET dp0=%~dp0\r\n" +
		"EXIT /b\r\n" +
		":start\r\n" +
		"SETLOCAL\r\n" +
		"CALL :find_dp0\r\n\r\n" +
		"IF EXIST \"%dp0%\\node.exe\" (\r\n" +
		"  SET \"_prog=%dp0%\\node.exe\"\r\n" +
		") ELSE (\r\n" +
		"  SET \"_prog=node\"\r\n" +
		")\r\n\r\n" +
		"endLocal & goto #_undefined_# 2>NUL || title %COMSPEC% & set PATHEXT=%PATHEXT:;.JS;=;% & \"%_prog%\"  \"%dp0%\\node_modules\\@openai\\codex\\bin\\codex.js\" %*\r\n"
	if err := os.WriteFile(shim, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	gotNode, gotScript, ok := ParseNpmCmdShim(shim)
	if !ok {
		t.Fatal("ParseNpmCmdShim: ok=false, want true")
	}
	if gotNode != fakeNode {
		t.Errorf("nodeExe = %q, want %q", gotNode, fakeNode)
	}
	if gotScript != script {
		t.Errorf("script = %q, want %q", gotScript, script)
	}
}

// TestNpmShimBypassSurvivesWhatBreaksCmdExeEscaping is the fix for the
// user's exact reported failure - even with WrapForCmdExe's caret-escaping
// applied, a double quote followed later in the same argument by a
// metacharacter like `|` still gets misparsed by cmd.exe (confirmed live,
// and confirmed to be a real, still-open limitation in the current, "fixed"
// version of cross-spawn itself - https://github.com/moxystudio/node-cross-spawn/issues/82 -
// not a porting mistake). Routing through the shim's own real target
// (node.exe + its .js entry point) as a plain argv sidesteps cmd.exe's
// reparsing entirely, which is exactly what relay's own collaboration
// briefing text needs: it embeds an agent name via Go's %q (a real,
// unavoidable double quote) followed later by the example message header's
// `|` characters, in the same argument.
func TestNpmShimBypassSurvivesWhatBreaksCmdExeEscaping(t *testing.T) {
	echoArgs := buildEchoArgs(t)
	dir := t.TempDir()

	fakeNode := filepath.Join(dir, "node.exe")
	if err := copyFile(echoArgs, fakeNode); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "tool.js")
	if err := os.WriteFile(script, []byte("// stand-in"), 0o644); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "tool.cmd")
	body := "@ECHO off\r\nSETLOCAL\r\nSET dp0=%~dp0\r\n\"%dp0%\\tool.js\" %*\r\n"
	if err := os.WriteFile(shim, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	gotNode, gotScript, ok := ParseNpmCmdShim(shim)
	if !ok {
		t.Fatal("ParseNpmCmdShim: ok=false, want true")
	}

	// The exact shape that breaks WrapForCmdExe: a double quote (from Go's
	// %q, e.g. developer_instructions embedding an agent name) followed
	// later, in the SAME argument, by a pipe (the collaboration briefing's
	// example message-header text).
	args := []string{
		`developer_instructions="You are "testdev"; the header looks like ` + "`[relay | from NAME (ROLE) | task | normal | msg ID]`" + `"`,
	}
	cmd := exec.Command(gotNode, append([]string{gotScript}, args...)...)
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
	// echoargs (standing in for node.exe) echoes its own full argv, which
	// includes the script path node itself would normally consume before
	// the script's own arguments start.
	want := append([]string{gotScript}, args...)
	if len(got) != len(want) || got[len(got)-1] != args[0] {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o755)
}
