package adaptor

import (
	internalagy "github.com/thesahibnanda-max/relay/internal/adaptor/internal/agy"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

// AgySweepStale finishes the cleanup a crashed agy launch could not: with no
// relay agy agent running, it removes relay's MCP entry from agy's
// persistent, user-global mcp_config.json, restores that file as the user
// had it, and removes entries older relay versions left (see the agy
// adaptor's Prepare). Exported here so `relay gc` can offer it even to a
// user who never relaunches agy - internal/cli cannot import
// internal/adaptor/internal/agy directly (Go's internal package visibility),
// so this is the seam.
//
// A no-op, not an error, when agy isn't installed on this machine: there is
// nothing of agy's to clean up.
func AgySweepStale(paths relayhome.Paths) ([]string, error) {
	bin, err := ResolveBinary("agy")
	if err != nil {
		return nil, nil
	}
	return internalagy.Sweep(internalagy.Options{AgyBin: bin, LegacyMarker: paths.AgyOriginalSnapshotPath()})
}

// AgyStatus is relay's registration with agy, for `relay doctor`.
type AgyStatus = internalagy.Status

// InspectAgy reports relay's registration with agy, for the user whose home
// directory is home, without changing anything.
func InspectAgy(home string) (AgyStatus, error) {
	return internalagy.Inspect(internalagy.Options{Home: home})
}
