package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/thesahibnanda-max/relay/internal/ctl"
	"github.com/thesahibnanda-max/relay/internal/daemon"
	"github.com/thesahibnanda-max/relay/internal/doctor"
	"github.com/thesahibnanda-max/relay/internal/mcp"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

// runningClient returns an admin client, or explains that nothing is running
// (these commands never start a daemon: there would be nothing to show).
func runningClient(errw io.Writer) (*http.Client, bool) {
	paths, err := relayhome.Resolve()
	if err != nil {
		fmt.Fprintf(errw, "relay: %v\n", err)
		return nil, false
	}
	if _, err := daemon.Query(paths); err != nil {
		fmt.Fprintln(errw, "relay: the daemon is not running, so there are no sessions or messages (start one with: relay claude --session=NEW_LOCAL)")
		return nil, false
	}
	return adminClient(paths), true
}

func postJSON(c *http.Client, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	resp, err := c.Post("http://relay"+path, "application/json", r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var ae proto.APIError
		_ = json.NewDecoder(resp.Body).Decode(&ae)
		if ae.Error == "" {
			ae.Error = resp.Status
		}
		return fmt.Errorf("%s", ae.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func runSend(p Parsed, in io.Reader, out, errw io.Writer) int {
	c, ok := runningClient(errw)
	if !ok {
		return 1
	}
	to, words := p.Words[0], p.Words[1:]
	text := strings.Join(words, " ")
	if text == "-" {
		b, err := io.ReadAll(io.LimitReader(in, proto.MaxBodyBytes+1))
		if err != nil {
			fmt.Fprintf(errw, "relay: %v\n", err)
			return 1
		}
		text = strings.TrimSpace(string(b))
	}
	var res proto.SendResult
	err := postJSON(c, "/v1/admin/send", proto.AdminSend{Session: p.Target, To: to, Body: text, Kind: p.MsgKind, Priority: p.Priority}, &res)
	if err != nil {
		fmt.Fprintf(errw, "relay: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "sent %s to %s (%s)\n", res.ID, strings.Join(res.To, ", "), res.State)
	if res.Note != "" {
		fmt.Fprintf(errw, "note: %s\n", res.Note)
	}
	return 0
}

func fetchMessages(c *http.Client, p Parsed, states string) ([]proto.MessageView, error) {
	q := url.Values{}
	if p.Target != "" {
		q.Set("session", p.Target)
	}
	if p.Agent != "" {
		q.Set("agent", p.Agent)
	}
	if states != "" {
		q.Set("state", states)
	}
	if p.Limit > 0 {
		q.Set("limit", fmt.Sprint(p.Limit))
	}
	var msgs []proto.MessageView
	_, err := getJSON(c, "/v1/admin/messages?"+q.Encode(), &msgs)
	return msgs, err
}

func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func runMessages(p Parsed, out, errw io.Writer) int {
	c, ok := runningClient(errw)
	if !ok {
		return 1
	}
	msgs, err := fetchMessages(c, p, p.State)
	if err != nil {
		fmt.Fprintf(errw, "relay: %v\n", err)
		return 1
	}
	if len(msgs) == 0 {
		fmt.Fprintln(out, "No messages.")
		return 0
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tFROM\tTO\tKIND\tPRIO\tSTATE\tAGE\tTEXT")
	now := time.Now()
	for i := len(msgs) - 1; i >= 0; i-- { // oldest first
		m := msgs[i]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", m.ID, m.From, m.To, m.Kind, proto.PriorityName(m.Priority), m.State, ago(now, m.CreatedAt), snippet(m.Body, 60))
	}
	tw.Flush()
	return 0
}

func runApprove(p Parsed, in io.Reader, out, errw io.Writer) int {
	c, ok := runningClient(errw)
	if !ok {
		return 1
	}
	p.Limit = 200
	held, err := fetchMessages(c, p, "held")
	if err != nil {
		fmt.Fprintf(errw, "relay: %v\n", err)
		return 1
	}
	// oldest first
	for i, j := 0, len(held)-1; i < j; i, j = i+1, j-1 {
		held[i], held[j] = held[j], held[i]
	}

	decide := func(m proto.MessageView, accept bool) bool {
		verb := "reject"
		if accept {
			verb = "approve"
		}
		if err := postJSON(c, "/v1/admin/messages/"+m.ID+"/"+verb, nil, nil); err != nil {
			fmt.Fprintf(errw, "relay: %s %s: %v\n", verb, m.ID, err)
			return false
		}
		fmt.Fprintf(out, "%sd %s (%s -> %s)\n", verb, m.ID, m.From, m.To)
		return true
	}

	switch p.Sub {
	case "accept", "reject":
		target := p.Words[0]
		n, failed := 0, 0
		for _, m := range held {
			if strings.EqualFold(target, "all") || strings.EqualFold(target, m.ID) {
				n++
				if !decide(m, p.Sub == "accept") {
					failed++
				}
			}
		}
		if n == 0 {
			fmt.Fprintf(errw, "relay: no held message %q (see: relay approve)\n", target)
			return 1
		}
		if failed > 0 {
			return 1
		}
		return 0
	}

	if len(held) == 0 {
		fmt.Fprintln(out, "Nothing is waiting for approval.")
		return 0
	}
	interactive := false
	if f, ok := in.(*os.File); ok {
		interactive = term.IsTerminal(int(f.Fd()))
	}
	now := time.Now()
	if !interactive || p.Sub == "ls" {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tFROM\tTO\tPRIO\tAGE\tWHY HELD\tTEXT")
		for _, m := range held {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", m.ID, m.From, m.To, proto.PriorityName(m.Priority), ago(now, m.CreatedAt), snippet(m.Detail, 40), snippet(m.Body, 50))
		}
		tw.Flush()
		fmt.Fprintln(out, "\nDecide with: relay approve accept <id|all>   or   relay approve reject <id|all>")
		return 0
	}

	rd := bufio.NewReader(in)
	for i, m := range held {
		fmt.Fprintf(out, "\n[%d/%d] %s  %s -> %s  (%s, %s)\n%s\n\n%s\n", i+1, len(held), m.ID, m.From, m.To, proto.PriorityName(m.Priority), ago(now, m.CreatedAt), m.Detail, m.Body)
		for {
			fmt.Fprint(out, "[a]pprove  [r]eject  [s]kip  [q]uit > ")
			line, err := rd.ReadString('\n')
			if err != nil {
				return 0
			}
			switch strings.ToLower(strings.TrimSpace(line)) {
			case "a", "approve", "y":
				decide(m, true)
			case "r", "reject", "n":
				decide(m, false)
			case "s", "skip", "":
			case "q", "quit":
				return 0
			default:
				continue
			}
			break
		}
	}
	return 0
}

func runGC(p Parsed, out, errw io.Writer) int {
	paths, err := relayhome.Resolve()
	if err != nil {
		fmt.Fprintf(errw, "relay: %v\n", err)
		return 1
	}
	if !p.DryRun { // per-launch leftovers of crashed agents: always safe to collect
		removed, _ := paths.GC(nil)
		for _, d := range removed {
			fmt.Fprintln(out, "removed", d)
		}
		if len(removed) == 0 && p.OlderThan == 0 && !p.Compress {
			fmt.Fprintln(out, "Nothing to clean up.")
		}
	}
	if p.OlderThan == 0 && !p.Compress {
		return 0
	}

	// Pruning and compression touch the database and logs, which only the daemon owns.
	exe, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := daemon.Ensure(ctx, paths, exe); err != nil {
		fmt.Fprintf(errw, "relay: %v\n", err)
		return 1
	}
	c := adminClient(paths)
	c.Timeout = 10 * time.Minute // a big compression can take a while
	var rep proto.GCReport
	if err := postJSON(c, "/v1/admin/gc", proto.GCRequest{OlderThanS: int64(p.OlderThan.Seconds()), Compress: p.Compress, DryRun: p.DryRun}, &rep); err != nil {
		fmt.Fprintf(errw, "relay: %v\n", err)
		return 1
	}
	verb := "removed"
	if rep.DryRun {
		verb = "would remove"
	}
	if p.OlderThan > 0 {
		fmt.Fprintf(out, "%s %d idle session(s) (%d agents, %d messages, %d events), freeing %s of logs; %d local log file(s)\n",
			verb, rep.Sessions, rep.Agents, rep.Messages, rep.Events, humanBytes(rep.RawBytesFreed), rep.LocalLogsRemoved)
	}
	if p.Compress {
		cv := "compressed"
		if rep.DryRun {
			cv = "would compress"
		}
		fmt.Fprintf(out, "%s %d log segment(s), saving about %s\n", cv, rep.SegmentsCompressed, humanBytes(rep.BytesSaved))
	}
	return 0
}

func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%d B", n)
	case n < k*k:
		return fmt.Sprintf("%.1f KiB", float64(n)/k)
	case n < k*k*k:
		return fmt.Sprintf("%.1f MiB", float64(n)/(k*k))
	}
	return fmt.Sprintf("%.2f GiB", float64(n)/(k*k*k))
}

// runMCP serves the relay MCP tools over stdio for the tool that spawned us.
func runMCP(p Parsed) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv := &mcp.Server{
		Version:      Version,
		Backend:      mcp.CtlBackend{Dir: p.Target},
		Instructions: "Relay connects you to other AI coding agents in your session: relay_list_agents to see them, relay_send to hand work over or answer, relay_inbox to read waiting messages.",
	}
	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "relay mcp: %v\n", err)
		return 1
	}
	return 0
}

// runHook is what Claude runs for each registered hook event. It must never
// get in the tool's way: whatever goes wrong (the agent is gone, the socket is
// busy) it says nothing and exits 0.
func runHook(p Parsed, in io.Reader, out io.Writer) int {
	payload, err := io.ReadAll(io.LimitReader(in, 4<<20))
	if err != nil || len(payload) == 0 {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var res struct {
		Output string `json:"output"`
	}
	if _, err := ctl.Call(ctx, p.Target, "hook", json.RawMessage(payload), &res); err != nil {
		return 0
	}
	if res.Output != "" {
		fmt.Fprintln(out, res.Output)
	}
	return 0
}

func runDoctor(out, errw io.Writer) int {
	paths, err := relayhome.Resolve()
	if err != nil {
		fmt.Fprintf(errw, "relay: %v\n", err)
		return 1
	}
	cs := doctor.Run(doctor.DefaultEnv(paths, Version, daemon.Query))
	doctor.Render(out, cs)
	if doctor.Worst(cs) >= doctor.Fail {
		return 1
	}
	return 0
}
