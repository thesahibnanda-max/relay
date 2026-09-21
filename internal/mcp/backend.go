package mcp

import (
	"context"
	"encoding/json"

	"github.com/thesahibnanda-max/relay/internal/ctl"
)

// CtlBackend forwards tool calls to the agent that launched this shim.
type CtlBackend struct{ Dir string }

func (b CtlBackend) Call(ctx context.Context, op string, args json.RawMessage) (json.RawMessage, int, error) {
	var out json.RawMessage
	pending, err := ctl.Call(ctx, b.Dir, op, args, &out)
	return out, pending, err
}
