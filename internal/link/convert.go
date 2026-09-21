package link

import (
	"encoding/json"
	"regexp"

	"github.com/thesahibnanda-max/relay/internal/eventlog"
	"github.com/thesahibnanda-max/relay/internal/proto"
)

// A run of SGR mouse reports (ESC[<btn;col;rowM/m): what a terminal sends
// while the pointer moves over a TUI that enabled mouse tracking. It made up
// nearly all of the raw stream in practice and carries no information worth
// storing, so it is not uploaded (the local event log still has it).
var mouseOnly = regexp.MustCompile(`^(\x1b\[<\d+;\d+;\d+[Mm])+$`)

// FromEventlog converts a logged event for upload; ok is false if it should
// not be uploaded.
func FromEventlog(e eventlog.Event) (ev proto.Event, ok bool) {
	ev = proto.Event{T: e.Time, Type: e.Type}
	switch e.Type {
	case "in", "out", "inject":
		if len(e.Bytes) == 0 || (e.Type == "in" && mouseOnly.Match(e.Bytes)) {
			return ev, false
		}
		ev.B = e.Bytes
	case "start":
		ev.Meta, _ = json.Marshal(map[string]any{"tool": e.Tool, "bin": e.Bin, "args": e.Args, "cwd": e.Cwd})
	case "resize":
		ev.Meta, _ = json.Marshal(map[string]any{"rows": e.Rows, "cols": e.Cols})
	case "exit":
		code := 0
		if e.Code != nil {
			code = *e.Code
		}
		ev.Meta, _ = json.Marshal(map[string]any{"code": code, "dropped": e.Dropped})
	default:
		return ev, false
	}
	return ev, true
}
