//go:build !unix && !windows

package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

// The daemon's process management (file locks, detaching, signals) is Unix
// only. These stand-ins keep the package compiling on other platforms; the CLI
// refuses to run there before it gets this far.
var (
	ErrAlreadyRunning  = errors.New("relay daemon is already running")
	ErrVersionMismatch = errors.New("relay daemon speaks a different protocol version")
	errUnsupported     = errors.New("relay daemon: not supported on this platform (use Linux, macOS or WSL)")
)

func OpenLog(relayhome.Paths) (*os.File, error) { return nil, errUnsupported }

func Run(context.Context, relayhome.Paths, string, *slog.Logger) error { return errUnsupported }

func Query(relayhome.Paths) (*proto.Status, error) { return nil, errUnsupported }

func Ensure(context.Context, relayhome.Paths, string) (*proto.Status, error) {
	return nil, errUnsupported
}

func Stop(relayhome.Paths) error { return errUnsupported }
