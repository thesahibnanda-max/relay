package cli

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/adaptor"
	"github.com/thesahibnanda-max/relay/internal/globalid"
	"github.com/thesahibnanda-max/relay/internal/ids"
	"github.com/thesahibnanda-max/relay/internal/proto"
)

var factory = adaptor.NewAdaptorFactory()

func parse(args ...string) (Parsed, error) {
	return Parse(append([]string{"relay"}, args...), &factory)
}

func TestShimModePassesEverythingToTheTool(t *testing.T) {
	p, err := Parse([]string{"/home/u/bin/claude", "--session=NEW", "--model", "haiku", "-p", "x"}, &factory)
	if err != nil || p.Kind != KindAgent || !p.Shim || p.Tool != "claude" || p.Session != "" {
		t.Fatalf("%+v %v", p, err)
	}
	want := []string{"--session=NEW", "--model", "haiku", "-p", "x"}
	if !reflect.DeepEqual(p.ToolArgs, want) {
		t.Errorf("tool args = %v, want %v (relay must not interpret them in shim mode)", p.ToolArgs, want)
	}
}

func TestTheDocumentedInvocations(t *testing.T) {
	id := ids.New()
	// relay claude --session=NEW -- --model=sonnet
	p, err := parse("claude", "--session=NEW", "--", "--model=sonnet")
	if err != nil || p.Session != proto.SessionNew || !reflect.DeepEqual(p.ToolArgs, []string{"--model=sonnet"}) {
		t.Fatalf("%+v %v", p, err)
	}
	// relay codex --session=<ULID> -- --model=gpt6
	p, err = parse("codex", "--session="+strings.ToLower(id), "--", "--model=gpt6")
	if err != nil || p.Session != id || p.Tool != "codex" || !reflect.DeepEqual(p.ToolArgs, []string{"--model=gpt6"}) {
		t.Fatalf("%+v %v", p, err)
	}
	// relay claude orchestrator --session=NEW --name=boss
	p, err = parse("claude", "orchestrator", "--session=NEW", "--name=boss")
	if err != nil || p.Role != "orchestrator" || p.Name != "boss" || len(p.ToolArgs) != 0 {
		t.Fatalf("%+v %v", p, err)
	}
	// role as a file path
	p, err = parse("codex", "./roles/auditor.md", "--session", id)
	if err != nil || p.Role != "./roles/auditor.md" || p.Session != id {
		t.Fatalf("%+v %v", p, err)
	}
	// plain solo launch
	p, err = parse("claude")
	if err != nil || p.Session != "" || p.Record != RecordRaw || p.Role != "" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestToolArgsAfterDoubleDashAreVerbatim(t *testing.T) {
	// even things that look like relay flags, and a second "--"
	p, err := parse("claude", "--session=NEW", "--", "--name", "x", "--session=NEW", "--", "-p")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--name", "x", "--session=NEW", "--", "-p"}
	if !reflect.DeepEqual(p.ToolArgs, want) || p.Name != "" {
		t.Errorf("tool args = %v, name = %q", p.ToolArgs, p.Name)
	}
	p, _ = parse("claude", "--")
	if len(p.ToolArgs) != 0 {
		t.Errorf("empty tool args expected, got %v", p.ToolArgs)
	}
}

func TestFlagForms(t *testing.T) {
	p, err := parse("claude", "--session", "NEW", "--name", "a1", "--role", "qa", "--record", "events", "--approve-inbound")
	if err != nil || p.Name != "a1" || p.Role != "qa" || p.Record != RecordEvents || !p.ApproveInbound {
		t.Fatalf("%+v %v", p, err)
	}
	p, _ = parse("claude", "--session=new", "--approve-inbound=false")
	if p.Session != proto.SessionNew || p.ApproveInbound {
		t.Errorf("%+v", p)
	}
	p, _ = parse("claude", "-session=NEW") // single dash tolerated, like Go's flag package
	if p.Session != proto.SessionNew {
		t.Errorf("%+v", p)
	}
}

func TestResumeAndFreshFlags(t *testing.T) {
	p, err := parse("claude", "--session=NEW", "--name=a1", "--resume")
	if err != nil || !p.Resume || p.Fresh {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = parse("claude", "--session=NEW", "--name=a1", "--fresh")
	if err != nil || !p.Fresh || p.Resume {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestGlobalSessionParsing(t *testing.T) {
	// --session=NEW now means a fresh GLOBAL session.
	p, err := parse("claude", "--session=NEW", "--server=example.com:5555", "--name=alice")
	if err != nil || p.SessionKind != SessionKindGlobalNew || p.Session != proto.SessionNew || p.Server != "example.com:5555" {
		t.Fatalf("%+v %v", p, err)
	}

	// --session=NEW_LOCAL is the escape hatch to today's exact local behavior.
	p, err = parse("claude", "--session=NEW_LOCAL", "--name=alice")
	if err != nil || p.SessionKind != SessionKindLocalNew || p.Session != proto.SessionNew {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = parse("claude", "--session=new_local", "--name=alice") // case-insensitive, like NEW
	if err != nil || p.SessionKind != SessionKindLocalNew {
		t.Fatalf("%+v %v", p, err)
	}

	// a bare ULID still means a local session (backward compatible).
	id := ids.New()
	p, err = parse("claude", "--session="+id, "--name=alice")
	if err != nil || p.SessionKind != SessionKindLocalJoin || p.Session != id {
		t.Fatalf("%+v %v", p, err)
	}

	// <ULID>@host[:port] is a global join token - the shareable string the
	// "others join with" banner prints.
	token := id + "@203.0.113.9:5555"
	p, err = parse("claude", "--session="+token, "--name=bob")
	if err != nil || p.SessionKind != SessionKindGlobalJoin {
		t.Fatalf("%+v %v", p, err)
	}
	want := globalid.Token{ULID: id, HostPort: "203.0.113.9:5555"}
	if p.GlobalToken != want {
		t.Errorf("GlobalToken = %+v, want %+v", p.GlobalToken, want)
	}
	if p.Session != token {
		t.Errorf("Session = %q, want %q", p.Session, token)
	}

	// a join token without an explicit port fills in the default.
	p, err = parse("claude", "--session="+id+"@example.com", "--name=bob")
	if err != nil || p.GlobalToken.HostPort != "example.com:5555" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestUsageErrors(t *testing.T) {
	id := ids.New()
	cases := map[string][]string{
		"unknown tool":            {"gemini"},
		"unknown relay flag":      {"claude", "--model", "haiku"},
		"bad session":             {"claude", "--session=abc"},
		"session needs value":     {"claude", "--session"},
		"session value is --":     {"claude", "--session", "--"},
		"session twice":           {"claude", "--session=NEW", "--session=" + id},
		"bad name":                {"claude", "--session=NEW", "--name=Bad Name"},
		"reserved name":           {"claude", "--session=NEW", "--name=user"},
		"name without session":    {"claude", "--name=x"},
		"approve without session": {"claude", "--approve-inbound"},
		"bad record":              {"claude", "--record=all"},
		"two positionals":         {"claude", "qa", "developer"},
		"role twice":              {"claude", "qa", "--role=developer"},
		"approve bad value":       {"claude", "--session=NEW", "--approve-inbound=maybe"},
		"resume and fresh":        {"claude", "--session=NEW", "--name=a1", "--resume", "--fresh"},
		"resume without name":     {"claude", "--session=NEW", "--resume"},
		"fresh without name":      {"claude", "--session=NEW", "--fresh"},
		"server given twice":      {"claude", "--session=NEW", "--server=a", "--server=b"},
	}
	for name, args := range cases {
		_, err := parse(args...)
		var ue *UsageError
		if !errors.As(err, &ue) {
			t.Errorf("%s: %v is not a UsageError", name, err)
		}
	}
	// the hint for a misplaced tool flag must tell the user what to do
	_, err := parse("claude", "--model", "haiku")
	if err == nil || !strings.Contains(err.Error(), "--") {
		t.Errorf("unhelpful message: %v", err)
	}
}

func TestSubcommands(t *testing.T) {
	id := ids.New()
	check := func(args []string, want Parsed) {
		t.Helper()
		got, err := parse(args...)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%v => %+v %v, want %+v", args, got, err, want)
		}
	}
	check([]string{"ls"}, Parsed{Kind: KindLs})
	check([]string{"ls", "--all"}, Parsed{Kind: KindLs, All: true})
	check([]string{"ls", "--session=" + id}, Parsed{Kind: KindLs, Target: id})
	check([]string{"session", "ls", "-a"}, Parsed{Kind: KindLs, All: true})
	check([]string{"session", "new"}, Parsed{Kind: KindSessionNew})
	check([]string{"session", "new", "--name=demo"}, Parsed{Kind: KindSessionNew, SessionNm: "demo"})
	check([]string{"session", "end", id}, Parsed{Kind: KindSessionEnd, Target: id})
	check([]string{"daemon"}, Parsed{Kind: KindDaemon})
	check([]string{"daemon", "stop"}, Parsed{Kind: KindDaemonStop})
	check([]string{"daemon", "status"}, Parsed{Kind: KindDaemonStatus})
	check([]string{"daemon", "--foreground"}, Parsed{Kind: KindDaemon, Foreground: true})
	check([]string{"--version"}, Parsed{Kind: KindVersion})
	check([]string{"help"}, Parsed{Kind: KindHelp})
	check([]string{}, Parsed{Kind: KindHelp})

	for _, bad := range [][]string{{"session"}, {"session", "wat"}, {"session", "end"}, {"session", "end", "nope"}, {"ls", "--bogus"}, {"ls", "--session=x"}, {"daemon", "restart"}} {
		if _, err := parse(bad...); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestMessagingCommands(t *testing.T) {
	id := ids.New()
	p, err := parse("mcp", "--dir", "/run/x")
	if err != nil || p.Kind != KindMCP || p.Target != "/run/x" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := parse("mcp"); err == nil {
		t.Error("mcp needs --dir")
	}

	p, err = parse("send", "bob", "--session="+id, "--priority=high", "--kind", "question", "what", "is", "up")
	if err != nil || p.Kind != KindSend || p.Words[0] != "bob" || strings.Join(p.Words[1:], " ") != "what is up" ||
		p.Priority != "high" || p.MsgKind != "question" || p.Target != id {
		t.Fatalf("%+v %v", p, err)
	}
	p, err = parse("send", "bob", "--", "--not-a-flag")
	if err != nil || p.Words[1] != "--not-a-flag" {
		t.Fatalf("-- ends flags: %+v %v", p, err)
	}
	for _, bad := range [][]string{
		{"send"}, {"send", "bob"}, {"send", "bob", "hi", "--priority=asap"}, {"send", "bob", "hi", "--kind=control"},
		{"send", "bob", "hi", "--session=nope"}, {"send", "bob", "hi", "--wat"},
	} {
		if _, err := parse(bad...); err == nil {
			t.Errorf("%v should be rejected", bad)
		}
	}

	for args, want := range map[string]string{"": "", "ls": "ls", "list": "ls", "accept 01ABC": "accept", "approve all": "accept", "reject all": "reject", "deny x": "reject"} {
		p, err := parse(append([]string{"approve"}, strings.Fields(args)...)...)
		if err != nil || p.Kind != KindApprove || p.Sub != want {
			t.Errorf("approve %q: %+v %v", args, p, err)
		}
	}
	for _, bad := range []string{"accept", "reject a b", "ls extra", "--wat"} {
		if _, err := parse(append([]string{"approve"}, strings.Fields(bad)...)...); err == nil {
			t.Errorf("approve %q should be rejected", bad)
		}
	}

	p, err = parse("messages", "--session="+id, "--agent=bob", "--state=held", "--limit", "5")
	if err != nil || p.Kind != KindMessages || p.Agent != "bob" || p.State != "held" || p.Limit != 5 {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := parse("messages", "--agent=bob"); err == nil {
		t.Error("--agent needs --session")
	}
	if p, err := parse("gc"); err != nil || p.Kind != KindGC {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := parse("gc", "now"); err == nil {
		t.Error("gc takes only options")
	}
	p, err = parse("gc", "--older-than=30d", "--compress", "--dry-run")
	if err != nil || p.OlderThan != 30*24*time.Hour || !p.Compress || !p.DryRun {
		t.Fatalf("%+v %v", p, err)
	}
	for _, bad := range [][]string{{"gc", "--dry-run"}, {"gc", "--older-than=soon"}, {"gc", "--older-than=0d"}, {"gc", "--older-than"}} {
		if _, err := parse(bad...); err == nil {
			t.Errorf("%v should be rejected", bad)
		}
	}
	for in, want := range map[string]time.Duration{"90m": 90 * time.Minute, "12h": 12 * time.Hour, "30d": 720 * time.Hour, "2w": 336 * time.Hour} {
		if got, err := ParseAge(in); err != nil || got != want {
			t.Errorf("ParseAge(%q) = %v %v", in, got, err)
		}
	}
}
