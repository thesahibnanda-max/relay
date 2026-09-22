package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/thesahibnanda-max/relay/internal/daemon"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

func adminClient(paths relayhome.Paths) *http.Client { return proto.HTTPClient(paths.SocketPath()) }

func getJSON(c *http.Client, path string, v any) (int, error) {
	resp, err := c.Get("http://relay" + path)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var ae proto.APIError
		_ = json.NewDecoder(resp.Body).Decode(&ae)
		return resp.StatusCode, fmt.Errorf("%s", ae.Error)
	}
	return resp.StatusCode, json.NewDecoder(resp.Body).Decode(v)
}

func runLs(p Parsed, out, errw io.Writer) int {
	paths, err := relayhome.Resolve()
	if err != nil {
		fmt.Fprintf(errw, "relay: %v\n", err)
		return 1
	}
	// Read-only: never start a daemon just to say there is nothing.
	if _, err := daemon.Query(paths); err != nil {
		fmt.Fprintln(out, "No sessions (the relay daemon is not running).")
		return 0
	}
	c := adminClient(paths)
	var sessions []proto.SessionInfo
	if p.Target != "" {
		var s proto.SessionInfo
		if _, err := getJSON(c, "/v1/admin/sessions/"+p.Target+"?all=1", &s); err != nil {
			fmt.Fprintf(errw, "relay: %v\n", err)
			return 1
		}
		sessions = []proto.SessionInfo{s}
	} else {
		q := ""
		if p.All {
			q = "?all=1"
		}
		if _, err := getJSON(c, "/v1/admin/sessions"+q, &sessions); err != nil {
			fmt.Fprintf(errw, "relay: %v\n", err)
			return 1
		}
	}
	RenderSessions(out, sessions, time.Now())
	return 0
}

// RenderSessions prints sessions with their agents.
func RenderSessions(w io.Writer, sessions []proto.SessionInfo, now time.Time) {
	if len(sessions) == 0 {
		fmt.Fprintln(w, "No sessions. Start one with: relay claude --session=NEW")
		return
	}
	for i, s := range sessions {
		if i > 0 {
			fmt.Fprintln(w)
		}
		label := s.ID
		if s.Name != "" {
			label += "  " + s.Name
		}
		extra := s.Kind
		if s.Status != "active" {
			extra += ", " + s.Status
		}
		fmt.Fprintf(w, "%s  (%s, started %s)\n", label, extra, ago(now, s.CreatedAt))
		if len(s.Agents) == 0 {
			fmt.Fprintln(w, "  no agents")
			continue
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  AGENT\tTOOL\tROLE\tSTATE\tJOINED")
		for _, a := range s.Agents {
			state := a.Status
			if a.Status == "exited" && a.ExitCode != nil {
				state = fmt.Sprintf("exited (%d)", *a.ExitCode)
			}
			if a.ApproveInbound {
				state += " [approve-inbound]"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", a.Name, a.Tool, a.Role, state, ago(now, a.JoinedAt))
		}
		tw.Flush()
	}
}

func ago(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 0 || d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func runSession(p Parsed, out, errw io.Writer) int {
	paths, err := relayhome.Resolve()
	if err == nil {
		err = paths.Ensure()
	}
	if err != nil {
		fmt.Fprintf(errw, "relay: %v\n", err)
		return 1
	}
	exe, _ := os.Executable()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := daemon.Ensure(ctx, paths, exe); err != nil {
		fmt.Fprintf(errw, "relay: %v\n", err)
		return 1
	}
	c := adminClient(paths)
	switch p.Kind {
	case KindSessionNew:
		body, _ := json.Marshal(proto.CreateSessionRequest{Name: p.SessionNm})
		resp, err := c.Post("http://relay/v1/admin/sessions", "application/json", strings.NewReader(string(body)))
		if err != nil {
			fmt.Fprintf(errw, "relay: %v\n", err)
			return 1
		}
		defer resp.Body.Close()
		var s proto.SessionInfo
		if resp.StatusCode != 201 || json.NewDecoder(resp.Body).Decode(&s) != nil {
			fmt.Fprintf(errw, "relay: could not create session (HTTP %d)\n", resp.StatusCode)
			return 1
		}
		fmt.Fprintln(out, s.ID) // stdout is just the id, so it composes: relay claude --session=$(relay session new)
		fmt.Fprintf(errw, "join with: relay <claude|codex> [role] --session=%s\n", s.ID)
		if p.Host {
			blob, err := invite(c, s.ID)
			if err != nil {
				fmt.Fprintf(errw, "relay: session created, but could not mint an invite: %v\n", err)
				return 1
			}
			fmt.Fprintf(errw, "from another machine, join with: relay <claude|codex> [role] --join=%s\n", proto.EncodeJoinBlob(blob))
		}
	case KindSessionEnd:
		resp, err := c.Post("http://relay/v1/admin/sessions/"+p.Target+"/end", "application/json", nil)
		if err != nil {
			fmt.Fprintf(errw, "relay: %v\n", err)
			return 1
		}
		resp.Body.Close()
		if resp.StatusCode == 404 {
			fmt.Fprintf(errw, "relay: no such session %s\n", p.Target)
			return 1
		}
		fmt.Fprintf(out, "session %s ended\n", p.Target)
	case KindSessionInvite:
		blob, err := invite(c, p.Target)
		if err != nil {
			fmt.Fprintf(errw, "relay: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, proto.EncodeJoinBlob(blob))
	case KindSessionPeers:
		var peers []proto.MeshPeerView
		if _, err := getJSON(c, "/v1/admin/mesh/peers/"+p.Target, &peers); err != nil {
			fmt.Fprintf(errw, "relay: %v\n", err)
			return 1
		}
		if len(peers) == 0 {
			fmt.Fprintln(out, "no peers (this session has not been joined from, or invited to, another machine)")
			return 0
		}
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PEER\tSTATUS\tLAST SEEN")
		for _, pr := range peers {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", pr.PeerID, pr.Status, pr.LastSeen.Format(time.RFC3339))
		}
		tw.Flush()
	}
	return 0
}

// invite ensures a daemon is running for sessionID and mints a fresh mesh
// join blob for it, via the admin API (never called from meshJoin's side of
// a --join, only from the inviting side: `relay session new --host`,
// `relay session invite`, and nowhere else).
func invite(c *http.Client, sessionID string) (proto.JoinBlob, error) {
	var blob proto.JoinBlob
	if err := postJSON(c, "/v1/admin/mesh/invite", proto.InviteMeshSessionRequest{SessionID: sessionID}, &blob); err != nil {
		return proto.JoinBlob{}, err
	}
	return blob, nil
}

// meshJoin teaches paths' local daemon about a remote session named by blob,
// via the admin API, before the caller registers an agent into it - see
// connect() in agentcmd.go. The daemon must already be running (connect
// ensures this itself before calling meshJoin).
func meshJoin(paths relayhome.Paths, blob proto.JoinBlob) error {
	if err := postJSON(adminClient(paths), "/v1/admin/mesh/join", blob, nil); err != nil {
		return fmt.Errorf("joining session %s: %w", blob.Session, err)
	}
	return nil
}

func runDaemon(p Parsed, out, errw io.Writer) int {
	paths, err := relayhome.Resolve()
	if err != nil {
		fmt.Fprintf(errw, "relay: %v\n", err)
		return 1
	}
	switch p.Kind {
	case KindDaemonStatus:
		st, err := daemon.Query(paths)
		if err != nil {
			fmt.Fprintln(out, "relay daemon is not running")
			return 1
		}
		fmt.Fprintf(out, "relay daemon running: pid %d, version %s, protocol v%d, up %s, %d session(s), %d agent(s) connected\n",
			st.PID, st.Version, st.Proto, time.Since(st.StartedAt).Round(time.Second), st.Sessions, st.Agents)
		return 0
	case KindDaemonStop:
		if err := daemon.Stop(paths); err != nil {
			fmt.Fprintf(errw, "relay: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, "relay daemon stopped")
		return 0
	}

	// run the daemon itself
	lf, err := daemon.OpenLog(paths)
	if err != nil {
		fmt.Fprintf(errw, "relay: %v\n", err)
		return 1
	}
	defer lf.Close()
	var w io.Writer = lf
	if p.Foreground {
		w = io.MultiWriter(lf, errw)
	}
	log := slog.New(slog.NewTextHandler(w, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := daemon.Run(ctx, paths, Version, log); err != nil {
		if err == daemon.ErrAlreadyRunning {
			fmt.Fprintln(errw, "relay: the daemon is already running")
		} else {
			fmt.Fprintf(errw, "relay: %v\n", err)
		}
		log.Error("daemon exited", "err", err)
		return 1
	}
	return 0
}
