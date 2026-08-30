package app

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

func newConfigTestApp(t *testing.T) *App {
	t.Helper()
	a, err := New(context.Background(), Options{
		Config:       config.Default(),
		WorkingDir:   t.TempDir(),
		DatabasePath: ":memory:",
		LogPath:      ":discard",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// ApplyConfig swaps the live config and re-derives the permission policy, so a
// mode change from PUT /api/config reaches the evaluator (#6).
func TestApplyConfigIsLiveForModeAndRules(t *testing.T) {
	a := newConfigTestApp(t)
	if a.Config().Execution.Mode == permissions.ModeAuto {
		t.Fatal("default fixture is already auto")
	}

	next := a.Config().Clone()
	next.Execution.Mode = permissions.ModeAuto
	next.Permissions.Shell.Execute = permissions.RuleAllow

	restart := a.ApplyConfig(next)
	if len(restart) != 0 {
		t.Errorf("restart-only fields = %v, want none for a mode/rule change", restart)
	}
	if a.Config().Execution.Mode != permissions.ModeAuto {
		t.Error("Config() did not reflect the swap")
	}
	if a.Evaluator.Policy().Mode != permissions.ModeAuto {
		t.Error("evaluator policy did not follow the swap")
	}
	if got := a.Evaluator.Rule(permissions.CatShellExecute); got != permissions.RuleAllow {
		t.Errorf("evaluator shell.execute rule = %q, want allow", got)
	}
}

// Construction-time settings are reported so the caller can still say
// restart_required for them.
func TestApplyConfigReportsRestartOnlyFields(t *testing.T) {
	a := newConfigTestApp(t)

	next := a.Config().Clone()
	next.Web.Port = 4242
	next.Logging.Level = "debug"
	next.Network.Enabled = !next.Network.Enabled

	restart := a.ApplyConfig(next)
	for _, want := range []string{"web", "logging", "network"} {
		if !slices.Contains(restart, want) {
			t.Errorf("restart-only fields %v, want it to include %q", restart, want)
		}
	}
}

// Config() and ApplyConfig() must be safe under concurrent access — the tool
// loop reads while a handler swaps. Run under -race.
func TestConfigAccessIsRaceFree(t *testing.T) {
	a := newConfigTestApp(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = a.Config().Execution.MaxToolIterations
			}
		}()
		go func() {
			defer wg.Done()
			for range 100 {
				a.ApplyConfig(a.Config().Clone())
			}
		}()
	}
	wg.Wait()
}
