package permissions_test

import (
	"testing"

	"github.com/boop-dev/boop/internal/permissions"
)

// Scenario tests lock down end-to-end permission outcomes for real workflows,
// as opposed to the unit behaviour of the classifier and evaluator separately.
// Each one closes a gap that a plausible misconfiguration would otherwise open.

// A force-push must not slip through just because the user allowed pushes.
func TestForcePushIsNotWavedThroughInAutoMode(t *testing.T) {
	cls := permissions.ClassifyCommand("git push --force origin main")
	t.Logf("classified: category=%s risk=%s production=%v", cls.Category, cls.Risk, cls.Production)

	policy := permissions.NewPolicy(permissions.ModeAuto, map[permissions.Category]permissions.Rule{
		permissions.CatGitPush: permissions.RuleAllow,
	})
	ev := permissions.NewEvaluator(policy)
	d := ev.Evaluate(permissions.Action{
		Category:   cls.Category,
		Risk:       cls.Risk,
		Production: cls.Production,
		Tool:       "git",
		Summary:    "force-push to main",
	})
	t.Logf("auto + git.push:allow -> %s (%s)", d.Outcome, d.Reason)
	if d.Outcome == permissions.OutcomeAllow {
		t.Error("force-push to main was allowed without confirmation")
	}
}

// Production intent is tracked separately from execution mode (§15), so even
// the deliberately-named unrestricted flag must not reach production.
func TestUnrestrictedDoesNotBypassProduction(t *testing.T) {
	policy := permissions.NewPolicy(permissions.ModeAuto, permissions.DefaultRules())
	policy.Unrestricted = true
	ev := permissions.NewEvaluator(policy)
	d := ev.Evaluate(permissions.Action{
		Category:   permissions.CatProductionChange,
		Risk:       permissions.RiskHigh,
		Production: true,
		Tool:       "run",
		Summary:    "terraform apply",
	})
	t.Logf("unrestricted + production -> %s (%s)", d.Outcome, d.Reason)
	if d.Outcome == permissions.OutcomeAllow {
		t.Error("unrestricted bypassed the production gate")
	}
}

// Adding a disk and mounting it is a legitimate user request ("i have added sdc
// device, create lvm and mount on /mnt/storage"). It must be classified as
// critical so it prompts, but not as production, so it prompts once rather than
// demanding separate production authorization.
func TestLVMWorkflowIsClassifiedNotBlocked(t *testing.T) {
	for _, cmd := range []string{
		"pvcreate /dev/sdc",
		"vgcreate storage /dev/sdc",
		"lvcreate -l 100%FREE -n data storage",
		"mkfs.ext4 /dev/storage/data",
		"mount /dev/storage/data /mnt/storage",
	} {
		c := permissions.ClassifyCommand(cmd)
		t.Logf("%-45s risk=%-8s cat=%-16s prod=%v", cmd, c.Risk, c.Category, c.Production)
		if c.Risk != permissions.RiskCritical {
			t.Errorf("%q risk = %s, want critical", cmd, c.Risk)
		}
	}
}

// Everyday commands must stay cheap. Training users to click through
// approvals is its own vulnerability.
func TestOrdinaryCommandsStayCheap(t *testing.T) {
	for _, cmd := range []string{"ls -la", "go test ./...", "git status", "cat README.md"} {
		c := permissions.ClassifyCommand(cmd)
		t.Logf("%-20s risk=%-8s cat=%s", cmd, c.Risk, c.Category)
		if c.Risk != permissions.RiskLow {
			t.Errorf("%q risk = %s, want low", cmd, c.Risk)
		}
	}
}
