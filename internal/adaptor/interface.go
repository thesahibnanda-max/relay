package adaptor

import (
	"strings"

	"github.com/thesahibnanda-max/relay/internal/adaptor/launch"
	"github.com/thesahibnanda-max/relay/internal/state"

	internalagy "github.com/thesahibnanda-max/relay/internal/adaptor/internal/agy"
	internalclaude "github.com/thesahibnanda-max/relay/internal/adaptor/internal/claude"
	internalcodex "github.com/thesahibnanda-max/relay/internal/adaptor/internal/codex"
	internalcopilot "github.com/thesahibnanda-max/relay/internal/adaptor/internal/copilot"
)

// Adaptor holds the per-tool knowledge Relay needs to wrap a CLI.
type Adaptor interface {
	// Name is the tool name used on the command line and as the shim name.
	Name() string
	// Binary is the executable to look up on PATH.
	Binary() string
	// Env returns extra KEY=VALUE entries for the child environment.
	Env() []string
	// Prepare builds the per-launch flags and ephemeral files that give this
	// one process its Relay capabilities (MCP tools, briefing). See package
	// launch for the zero-footprint rules.
	Prepare(spec launch.Spec) (launch.Plan, error)
	// ScreenRules recognise the tool's dialogs from the screen text.
	ScreenRules() []state.ScreenRule
	// Cleanup reverses whatever Prepare registered for this one launch (if
	// anything). It must always be safe to call - including when Prepare was
	// never called, failed, or registered nothing - and must never block
	// indefinitely or panic. It runs on every exit path except a hard kill
	// (kill -9) of relay itself, the same reliability profile RunDir's own
	// cleanup already has today.
	Cleanup(spec launch.Spec) error
}

// Re-exports so callers need only this package.
type (
	LaunchSpec = launch.Spec
	LaunchPlan = launch.Plan
)

type AdaptorFactory struct {
	factory []Adaptor
}

// ByName looks an adaptor up by name, ignoring case and surrounding space.
func (a *AdaptorFactory) ByName(name string) (Adaptor, bool) {
	name = strings.TrimSpace(name)
	for _, f := range a.factory {
		if strings.EqualFold(name, f.Name()) {
			return f, true
		}
	}
	return nil, false
}

// Names lists the supported tool names.
func (a *AdaptorFactory) Names() []string {
	names := make([]string, 0, len(a.factory))
	for _, f := range a.factory {
		names = append(names, f.Name())
	}
	return names
}

func NewAdaptorFactory() AdaptorFactory { // no need of ptr here
	return AdaptorFactory{
		factory: []Adaptor{
			&internalclaude.ClaudeAdaptor{},
			&internalcodex.CodexAdaptor{},
			&internalcopilot.CopilotAdaptor{},
			&internalagy.AgyAdaptor{},
		},
	}
}
