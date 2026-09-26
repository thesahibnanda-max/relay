//go:build windows

package peercred

import (
	"fmt"
	"net"
)

// Windows AF_UNIX sockets carry no SO_PEERCRED equivalent: rely on the
// socket's owner-only ACL in its owner-only directory instead (see
// relayhome.SetPrivateACL), exactly as every other non-linux/darwin platform
// already does via peercred_other.go.
func UID(net.Conn) (uint32, error) { return 0, fmt.Errorf("peer credentials unsupported") }
