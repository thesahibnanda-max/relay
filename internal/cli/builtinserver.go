package cli

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// builtinServerURL is empty in any build without the release ldflag (every
// local/from-source build via `make build`/`go build .`) - injected only at
// official release build time via
// "-X github.com/thesahibnanda-max/relay/internal/cli.builtinServerURL=...",
// mirroring Version's own injection exactly (see version.go).
var builtinServerURL string

// builtinServer normalizes builtinServerURL into a bare "host:port" (the
// same shape --server/a join token's HostPort already use), accepting either
// a plain host[:port] or a full URL (a scheme, if present, is stripped - an
// official release binary always dials TLS regardless, see
// resolveGlobalServer). ok is false only when this build has no server
// configured at all, i.e. every local/from-source build.
func builtinServer() (hostPort string, ok bool) {
	raw := strings.TrimSpace(builtinServerURL)
	if raw == "" {
		return "", false
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		raw = u.Host
	}
	if _, _, err := net.SplitHostPort(raw); err != nil {
		raw = net.JoinHostPort(raw, "443")
	}
	return raw, true
}

// resolveGlobalServer decides the actual host:port to dial for a global
// session and whether to force TLS, given what the caller asked for
// (requested == "" means "no preference," e.g. --session=NEW with no
// --server set).
//
// If this build has a configured builtin server (an official release
// binary), it is the ONLY host ever dialed: an explicit request for
// anything else is a clear, rejected error - never silently overridden,
// never silently ignored. If this build has no builtin server configured (a
// local/from-source build), behavior is unchanged from before this feature
// existed: requested is used as-is, untouched, forceTLS always false
// (RELAY_GLOBAL_TLS is still the only way to opt into TLS there).
func resolveGlobalServer(requested string) (hostPort string, forceTLS bool, err error) {
	builtin, ok := builtinServer()
	if !ok {
		return requested, false, nil // today's exact behavior - caller handles requested == ""
	}
	if requested != "" && requested != builtin {
		return "", false, fmt.Errorf("relay: custom global servers are not supported in this build - every global session uses %s (got %q)", builtin, requested)
	}
	return builtin, true, nil
}
