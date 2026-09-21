//go:build !linux && !darwin

package peercred

import (
	"fmt"
	"net"
)

// No peer-credential API here: rely on the socket's 0600 mode in a 0700 dir.
func UID(net.Conn) (uint32, error) { return 0, fmt.Errorf("peer credentials unsupported") }
