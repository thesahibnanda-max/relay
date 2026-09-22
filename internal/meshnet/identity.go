package meshnet

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"
)

// Identity is this daemon's stable mesh identity: a node key (whose public
// half is Relay's PeerID for this daemon, unforgeable since only this
// daemon holds the private half) plus the pre-shared key a tailcat server
// bakes into its advertised Addr.
//
// tailcat generates a fresh ephemeral key pair every run by default, which
// would silently invalidate every previously-shared --join blob on daemon
// restart. Persisting and reusing one Identity is what keeps a daemon's
// mesh address stable across restarts.
type Identity struct {
	NodeKey key.NodePrivate
	PSK     tailcat.PresharedKey
}

// identityFile is the on-disk JSON shape. It is Relay's own format, not
// tailcat's `genkey` file format: the two key types already marshal to text
// (key.NodePrivate, tailcat.PresharedKey both implement
// Marshal/UnmarshalText), so plain JSON round-trips them directly, and a
// future tailcat schema change can't affect a file Relay already wrote.
type identityFile struct {
	V       int                  `json:"v"`
	NodeKey key.NodePrivate      `json:"node_key"`
	PSK     tailcat.PresharedKey `json:"psk"`
}

// LoadOrCreateIdentity reads path; if it doesn't exist, a fresh Identity is
// generated and written there (0600, atomic write-then-rename) before
// returning. Called once per daemon process.
func LoadOrCreateIdentity(path string) (*Identity, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		var f identityFile
		if jerr := json.Unmarshal(data, &f); jerr != nil {
			return nil, fmt.Errorf("meshnet: %s is corrupt: %w", path, jerr)
		}
		if f.NodeKey.IsZero() {
			return nil, fmt.Errorf("meshnet: %s has no node key", path)
		}
		return &Identity{NodeKey: f.NodeKey, PSK: f.PSK}, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("meshnet: reading %s: %w", path, err)
	}

	fresh := tailcat.NewPrivateKey()
	id := &Identity{NodeKey: fresh.Private, PSK: fresh.Public.PresharedKey}
	if err := id.save(path); err != nil {
		return nil, err
	}
	return id, nil
}

func (id *Identity) save(path string) error {
	data, err := json.Marshal(identityFile{V: 1, NodeKey: id.NodeKey, PSK: id.PSK})
	if err != nil {
		return fmt.Errorf("meshnet: encoding identity: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("meshnet: creating %s: %w", filepath.Dir(path), err)
	}
	tmp := path + fmt.Sprintf(".tmp-%d", os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("meshnet: writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("meshnet: renaming %s: %w", tmp, err)
	}
	return nil
}

// PeerID is Relay's stable identifier for this daemon: its node public key
// in tailcat's own "nodekey:<hex>" text form. Using tailcat's own format
// (rather than a bare hex string) means it compares equal, byte for byte, to
// the verified identity a Listener.Accept hands back for an inbound
// connection (see RealTransport, which derives that from the tunnel itself,
// not from anything the peer merely claims) - PeerID is never used to
// authenticate a connection *to*; it is compared *against* an
// independently-verified value.
//
// Two daemons can never share a PeerID without sharing a private key, and a
// daemon that regenerates its identity file becomes a new PeerID (a
// stale-but-visible roster entry), never a silently-trusted one.
func (id *Identity) PeerID() string {
	return id.NodeKey.Public().String()
}
