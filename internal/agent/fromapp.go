package agent

import (
	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/provider"
)

// NewFromApp builds a coordinator over an assembled runtime.
//
// It lives here rather than on *app.App because this package builds on
// app.Loop: the dependency runs upward, so the core cannot hold the fleet
// without a cycle. Every frontend calls this instead of assembling the
// coordinator's collaborators itself.
//
// It returns nil when agents are disabled in configuration, which callers
// should treat as "delegation is off" rather than as an error — a nil
// Coordinator is not usable, so check before calling.
func NewFromApp(a *app.App, sessionID string) *Coordinator {
	if a == nil || !a.Config().Agents.Enabled {
		return nil
	}
	sel := provider.Selection{Provider: a.Config().Provider, Model: a.Config().Model}
	return NewCoordinator(CoordinatorOptions{
		Runner: &LoopRunner{Loops: a.NewLoop, Tools: a.Tools},
		Planner: &Planner{Decomposer: &ModelDecomposer{
			Caller: &RouterCaller{Router: a.Router, Selection: sel},
		}},
		Scope: &Scope{
			Environment: a.Workspace.Root(),
			Memory:      ProjectMemory(a.Memory()),
		},
		Bus:       a.Bus,
		SessionID: sessionID,
		Provider:  a.Config().Provider,
		Model:     a.Config().Model,
		Max:       a.Config().Agents.Max,
	})
}
