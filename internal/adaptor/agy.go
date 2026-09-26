package adaptor

import (
	internalagy "github.com/thesahibnanda-max/relay/internal/adaptor/internal/agy"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

// AgySweepStale removes any leftover relay-*-prefixed MCP entries agy's own
// AgyAdaptor left in agy's persistent, global mcp_config.json after a
// crashed launch (see that adaptor's Prepare, which runs the identical sweep
// before every add). Exported here so `relay gc` can offer the same
// one-command cleanup even for a user who never relaunches agy - internal/cli
// cannot import internal/adaptor/internal/agy directly (Go's internal
// package visibility), so this is the seam.
//
// A no-op, not an error, when agy isn't installed on this machine: there is
// nothing of agy's to clean up.
func AgySweepStale(paths relayhome.Paths) ([]string, error) {
	bin, err := ResolveBinary("agy")
	if err != nil {
		return nil, nil
	}
	return internalagy.SweepStale(paths, bin)
}
