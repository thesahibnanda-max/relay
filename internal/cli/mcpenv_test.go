package cli

import (
	"testing"

	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

// agy spawns relay's one user-global MCP entry from every agy process; only
// the one relay launched (matching RELAY_AGENT_ID and run directory) may act
// as that agent.
func TestMCPDirFromEnv(t *testing.T) {
	paths := relayhome.Paths{Root: t.TempDir()}
	const id = "01K6AAAAAAAAAAAAAAAAAAAAAA"
	dir, err := paths.CreateAgentDir(id)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		runDir, agentID string
		wantDisabled    bool
	}{
		{dir, id, false},
		{dir, "01k6aaaaaaaaaaaaaaaaaaaaaa", false}, // ULIDs are case-insensitive
		{"", "", true}, // an agy relay did not start
		{dir, "", true},
		{"", id, true},
		{dir, "01K6BBBBBBBBBBBBBBBBBBBBBB", true}, // another agent's run directory
		{dir, "not-a-ulid", true},
		{t.TempDir(), id, true}, // not a run directory at all
	}
	for _, c := range cases {
		t.Setenv("RELAY_RUN_DIR", c.runDir)
		t.Setenv("RELAY_AGENT_ID", c.agentID)
		got, disabled := mcpDirFromEnv()
		if disabled != c.wantDisabled || (!disabled && got != dir) {
			t.Errorf("RELAY_RUN_DIR=%q RELAY_AGENT_ID=%q: dir %q disabled %v, want disabled %v", c.runDir, c.agentID, got, disabled, c.wantDisabled)
		}
	}
}
