// Package intercept defines the hook points where Relay can observe, modify,
// suppress or inject bytes flowing between the user and the wrapped tool.
package intercept

import "github.com/thesahibnanda-max/relay/internal/eventlog"

// Interceptor sees every chunk of the byte stream. Returning p unchanged is
// exact pass-through. Implementations must not retain p: the caller reuses it.
//
// Note: chunk boundaries are arbitrary, so an escape sequence such as
// ESC [ Z (Shift+Tab) may be split across calls. Anything that needs to match
// key sequences must keep its own state.
type Interceptor interface {
	Input(p []byte) []byte  // user -> tool
	Output(p []byte) []byte // tool -> user
}

// Chain applies interceptors in order.
type Chain []Interceptor

func (c Chain) Input(p []byte) []byte {
	for _, i := range c {
		p = i.Input(p)
	}
	return p
}

func (c Chain) Output(p []byte) []byte {
	for _, i := range c {
		p = i.Output(p)
	}
	return p
}

// Log records every chunk and passes it through untouched.
type Log struct {
	L *eventlog.Logger
}

func (l Log) Input(p []byte) []byte {
	l.L.Data("in", p)
	return p
}

func (l Log) Output(p []byte) []byte {
	l.L.Data("out", p)
	return p
}
