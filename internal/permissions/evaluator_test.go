package permissions

import (
	"strings"
	"sync"
	"testing"
)

func TestEvaluateTable(t *testing.T) {
	tests := []struct {
		name    string
		policy  Policy
		action  Action
		outcome Outcome
		rule    Rule
	}{
		{
			name:    "auto mode allows an allowed category",
			policy:  Policy{Mode: ModeAuto, Rules: map[Category]Rule{CatFilesystemRead: RuleAllow}},
			action:  Action{Category: CatFilesystemRead, Risk: RiskLow},
			outcome: OutcomeAllow,
			rule:    RuleAllow,
		},
		{
			name:    "auto mode confirms a confirm category",
			policy:  Policy{Mode: ModeAuto, Rules: map[Category]Rule{CatShellExecute: RuleConfirm}},
			action:  Action{Category: CatShellExecute, Risk: RiskMedium},
			outcome: OutcomeConfirm,
			rule:    RuleConfirm,
		},
		{
			name:    "auto mode falls back to the default table",
			policy:  Policy{Mode: ModeAuto},
			action:  Action{Category: CatFilesystemRead, Risk: RiskLow},
			outcome: OutcomeAllow,
			rule:    RuleAllow,
		},
		{
			name:    "auto mode defaults an unknown category to confirm",
			policy:  Policy{Mode: ModeAuto},
			action:  Action{Category: Category("something.new"), Risk: RiskLow},
			outcome: OutcomeConfirm,
			rule:    RuleConfirm,
		},
		{
			name:    "confirm mode allows only explicit allows",
			policy:  Policy{Mode: ModeConfirm, Rules: map[Category]Rule{CatFilesystemRead: RuleAllow}},
			action:  Action{Category: CatFilesystemRead, Risk: RiskLow},
			outcome: OutcomeAllow,
			rule:    RuleAllow,
		},
		{
			name:    "confirm mode confirms a defaulted allow",
			policy:  Policy{Mode: ModeConfirm},
			action:  Action{Category: CatFilesystemRead, Risk: RiskLow},
			outcome: OutcomeConfirm,
			rule:    RuleAllow,
		},
		{
			name:    "confirm mode confirms shell execution",
			policy:  DefaultPolicy(),
			action:  Action{Category: CatShellExecute, Risk: RiskLow},
			outcome: OutcomeConfirm,
			rule:    RuleConfirm,
		},
		{
			name:    "deny is denied in auto mode",
			policy:  Policy{Mode: ModeAuto, Rules: map[Category]Rule{CatGitPush: RuleDeny}},
			action:  Action{Category: CatGitPush, Risk: RiskLow},
			outcome: OutcomeDeny,
			rule:    RuleDeny,
		},
		{
			name:    "deny beats unrestricted",
			policy:  Policy{Mode: ModeAuto, Unrestricted: true, ProductionAuthorized: true, Rules: map[Category]Rule{CatShellExecute: RuleDeny}},
			action:  Action{Category: CatShellExecute, Risk: RiskLow},
			outcome: OutcomeDeny,
			rule:    RuleDeny,
		},
		{
			name:    "deny beats unrestricted for production too",
			policy:  Policy{Mode: ModeAuto, Unrestricted: true, ProductionAuthorized: true, Rules: map[Category]Rule{CatProductionChange: RuleDeny}},
			action:  Action{Category: CatProductionChange, Risk: RiskCritical, Production: true},
			outcome: OutcomeDeny,
			rule:    RuleDeny,
		},
		{
			name:    "critical never auto-allows",
			policy:  Policy{Mode: ModeAuto, Rules: map[Category]Rule{CatShellExecute: RuleAllow}},
			action:  Action{Category: CatShellExecute, Risk: RiskCritical},
			outcome: OutcomeConfirm,
			rule:    RuleAllow,
		},
		{
			name:    "critical auto-allows only when unrestricted",
			policy:  Policy{Mode: ModeAuto, Unrestricted: true, Rules: map[Category]Rule{CatShellExecute: RuleAllow}},
			action:  Action{Category: CatShellExecute, Risk: RiskCritical},
			outcome: OutcomeAllow,
			rule:    RuleAllow,
		},
		{
			name:    "high risk still follows the rule table",
			policy:  Policy{Mode: ModeAuto, Rules: map[Category]Rule{CatShellExecute: RuleAllow}},
			action:  Action{Category: CatShellExecute, Risk: RiskHigh},
			outcome: OutcomeAllow,
			rule:    RuleAllow,
		},
		{
			name:    "unauthorized production confirms in auto mode",
			policy:  Policy{Mode: ModeAuto, Rules: map[Category]Rule{CatProductionChange: RuleAllow}},
			action:  Action{Category: CatProductionChange, Risk: RiskHigh, Production: true},
			outcome: OutcomeConfirm,
			rule:    RuleAllow,
		},
		{
			name:    "unauthorized production confirms even when unrestricted",
			policy:  Policy{Mode: ModeAuto, Unrestricted: true, Rules: map[Category]Rule{CatShellExecute: RuleAllow}},
			action:  Action{Category: CatShellExecute, Risk: RiskLow, Production: true},
			outcome: OutcomeConfirm,
			rule:    RuleAllow,
		},
		{
			name:    "production flag alone gates a non-production category",
			policy:  Policy{Mode: ModeAuto, Rules: map[Category]Rule{CatGitPush: RuleAllow}},
			action:  Action{Category: CatGitPush, Risk: RiskHigh, Production: true},
			outcome: OutcomeConfirm,
			rule:    RuleAllow,
		},
		{
			name:    "authorized production follows the rule table",
			policy:  Policy{Mode: ModeAuto, ProductionAuthorized: true, Rules: map[Category]Rule{CatProductionChange: RuleAllow}},
			action:  Action{Category: CatProductionChange, Risk: RiskHigh, Production: true},
			outcome: OutcomeAllow,
			rule:    RuleAllow,
		},
		{
			name:    "authorized critical production still confirms",
			policy:  Policy{Mode: ModeAuto, ProductionAuthorized: true, Rules: map[Category]Rule{CatProductionChange: RuleAllow}},
			action:  Action{Category: CatProductionChange, Risk: RiskCritical, Production: true},
			outcome: OutcomeConfirm,
			rule:    RuleAllow,
		},
		{
			name:    "invalid mode fails closed to confirm",
			policy:  Policy{Mode: Mode("yolo"), Rules: map[Category]Rule{CatFilesystemRead: RuleConfirm}},
			action:  Action{Category: CatFilesystemRead, Risk: RiskLow},
			outcome: OutcomeConfirm,
			rule:    RuleConfirm,
		},
		{
			name:    "invalid rule fails closed to confirm",
			policy:  Policy{Mode: ModeAuto, Rules: map[Category]Rule{CatFilesystemRead: Rule("maybe")}},
			action:  Action{Category: CatFilesystemRead, Risk: RiskLow},
			outcome: OutcomeConfirm,
			rule:    RuleConfirm,
		},
		{
			name:    "empty category confirms",
			policy:  Policy{Mode: ModeAuto},
			action:  Action{Risk: RiskLow},
			outcome: OutcomeConfirm,
			rule:    RuleConfirm,
		},
		{
			name:    "zero policy confirms",
			policy:  Policy{},
			action:  Action{Category: CatFilesystemRead, Risk: RiskLow},
			outcome: OutcomeConfirm,
			rule:    RuleAllow,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewEvaluator(tt.policy).Evaluate(tt.action)
			if got.Outcome != tt.outcome {
				t.Errorf("Outcome = %q, want %q (reason: %s)", got.Outcome, tt.outcome, got.Reason)
			}
			if got.Rule != tt.rule {
				t.Errorf("Rule = %q, want %q", got.Rule, tt.rule)
			}
			if strings.TrimSpace(got.Reason) == "" {
				t.Error("Decision.Reason is empty; the approval UI has nothing to show")
			}
			if strings.Contains(got.Reason, "%!") {
				t.Errorf("Decision.Reason has a formatting bug: %q", got.Reason)
			}
		})
	}
}

// TestEvaluateMatrix walks every combination of mode, rule, risk, production
// state and bypass flag, asserting the invariants that must hold regardless of
// the rest of the policy.
func TestEvaluateMatrix(t *testing.T) {
	modes := []Mode{ModeConfirm, ModeAuto}
	rules := []Rule{RuleAllow, RuleConfirm, RuleDeny}
	risks := []Risk{RiskLow, RiskMedium, RiskHigh, RiskCritical}
	bools := []bool{false, true}

	for _, mode := range modes {
		for _, rule := range rules {
			for _, risk := range risks {
				for _, production := range bools {
					for _, unrestricted := range bools {
						for _, authorized := range bools {
							policy := Policy{
								Mode:                 mode,
								Rules:                map[Category]Rule{CatShellExecute: rule},
								Unrestricted:         unrestricted,
								ProductionAuthorized: authorized,
							}
							action := Action{
								Category:   CatShellExecute,
								Risk:       risk,
								Production: production,
								Tool:       "run",
								Summary:    "run a command",
							}
							got := NewEvaluator(policy).Evaluate(action)

							switch {
							case rule == RuleDeny:
								if got.Outcome != OutcomeDeny {
									t.Fatalf("deny must win: mode=%s risk=%s prod=%v unrestricted=%v authorized=%v -> %s",
										mode, risk, production, unrestricted, authorized, got)
								}
							case production && !authorized:
								if got.Outcome != OutcomeConfirm {
									t.Fatalf("unauthorized production must confirm: mode=%s rule=%s risk=%s unrestricted=%v -> %s",
										mode, rule, risk, unrestricted, got)
								}
							case risk == RiskCritical && !unrestricted:
								if got.Outcome != OutcomeConfirm {
									t.Fatalf("critical risk must confirm: mode=%s rule=%s prod=%v authorized=%v -> %s",
										mode, rule, production, authorized, got)
								}
							case unrestricted:
								if got.Outcome != OutcomeAllow {
									t.Fatalf("unrestricted should allow the rest: mode=%s rule=%s risk=%s -> %s",
										mode, rule, risk, got)
								}
							case rule == RuleConfirm:
								if got.Outcome != OutcomeConfirm {
									t.Fatalf("confirm rule must confirm: mode=%s risk=%s -> %s", mode, risk, got)
								}
							case rule == RuleAllow:
								if got.Outcome != OutcomeAllow {
									t.Fatalf("allow rule should allow: mode=%s risk=%s -> %s", mode, risk, got)
								}
							}

							if got.Outcome == OutcomeAllow {
								if rule == RuleDeny {
									t.Fatalf("allowed a denied category")
								}
								if production && !authorized {
									t.Fatalf("allowed unauthorized production work")
								}
								if risk == RiskCritical && !unrestricted {
									t.Fatalf("allowed a critical action without the unrestricted flag")
								}
							}
						}
					}
				}
			}
		}
	}
}

func TestProductionReasonIsActionable(t *testing.T) {
	e := NewEvaluator(Policy{Mode: ModeAuto, Rules: map[Category]Rule{CatProductionChange: RuleAllow}})
	d := e.Evaluate(Action{Category: CatProductionChange, Risk: RiskHigh, Production: true})
	for _, want := range []string{"production", "authorization"} {
		if !strings.Contains(strings.ToLower(d.Reason), want) {
			t.Errorf("production reason %q should mention %q", d.Reason, want)
		}
	}
}

func TestAuthorizeProduction(t *testing.T) {
	e := NewEvaluator(Policy{Mode: ModeAuto, Rules: map[Category]Rule{CatProductionChange: RuleAllow}})
	action := Action{Category: CatProductionChange, Risk: RiskHigh, Production: true}
	if got := e.Evaluate(action); got.Outcome != OutcomeConfirm {
		t.Fatalf("before authorization: %s", got)
	}
	e.AuthorizeProduction(true)
	if got := e.Evaluate(action); got.Outcome != OutcomeAllow {
		t.Fatalf("after authorization: %s", got)
	}
	e.AuthorizeProduction(false)
	if got := e.Evaluate(action); got.Outcome != OutcomeConfirm {
		t.Fatalf("after revoking authorization: %s", got)
	}
}

func TestSetPolicyAndRule(t *testing.T) {
	e := NewEvaluator(DefaultPolicy())
	if got := e.Rule(CatShellExecute); got != RuleConfirm {
		t.Errorf("default shell rule = %q, want confirm", got)
	}
	e.SetPolicy(Policy{Mode: ModeAuto, Rules: map[Category]Rule{CatShellExecute: RuleAllow}})
	if got := e.Rule(CatShellExecute); got != RuleAllow {
		t.Errorf("updated shell rule = %q, want allow", got)
	}
	if got := e.Evaluate(Action{Category: CatShellExecute, Risk: RiskLow}); got.Outcome != OutcomeAllow {
		t.Errorf("after SetPolicy: %s", got)
	}
}

func TestPolicyIsCopiedNotShared(t *testing.T) {
	rules := map[Category]Rule{CatShellExecute: RuleAllow}
	e := NewEvaluator(Policy{Mode: ModeAuto, Rules: rules})

	// Mutating the caller's map must not change the live policy.
	rules[CatShellExecute] = RuleDeny
	if got := e.Evaluate(Action{Category: CatShellExecute, Risk: RiskLow}); got.Outcome != OutcomeAllow {
		t.Errorf("caller mutation leaked into the policy: %s", got)
	}

	// Nor must mutating the copy returned by Policy().
	snapshot := e.Policy()
	snapshot.Rules[CatShellExecute] = RuleDeny
	if got := e.Evaluate(Action{Category: CatShellExecute, Risk: RiskLow}); got.Outcome != OutcomeAllow {
		t.Errorf("snapshot mutation leaked into the policy: %s", got)
	}
}

func TestDefaultRulesAndNewPolicy(t *testing.T) {
	defaults := DefaultRules()
	want := map[Category]Rule{
		CatFilesystemRead:   RuleAllow,
		CatFilesystemWrite:  RuleConfirm,
		CatShellExecute:     RuleConfirm,
		CatGitRead:          RuleAllow,
		CatGitCommit:        RuleConfirm,
		CatGitPush:          RuleConfirm,
		CatNetworkHTTP:      RuleConfirm,
		CatProductionChange: RuleConfirm,
	}
	if len(defaults) != len(want) {
		t.Fatalf("DefaultRules has %d entries, want %d", len(defaults), len(want))
	}
	for cat, rule := range want {
		if defaults[cat] != rule {
			t.Errorf("DefaultRules()[%q] = %q, want %q", cat, defaults[cat], rule)
		}
	}

	// The table must be a fresh map each call, or one caller edits everyone's.
	defaults[CatShellExecute] = RuleAllow
	if DefaultRules()[CatShellExecute] != RuleConfirm {
		t.Error("DefaultRules returns shared state")
	}

	p := NewPolicy(Mode("nonsense"), map[Category]Rule{CatGitPush: Rule("nope")})
	if p.Mode != ModeConfirm {
		t.Errorf("NewPolicy mode = %q, want confirm", p.Mode)
	}
	if p.Rules[CatGitPush] != RuleConfirm {
		t.Errorf("NewPolicy kept an invalid rule: %q", p.Rules[CatGitPush])
	}
	if p.Unrestricted || p.ProductionAuthorized {
		t.Error("NewPolicy must not enable bypasses")
	}

	def := DefaultPolicy()
	if def.Mode != ModeConfirm || def.Unrestricted || def.ProductionAuthorized {
		t.Errorf("DefaultPolicy is not safe by default: %+v", def)
	}
}

func TestEvaluateCommand(t *testing.T) {
	e := NewEvaluator(Policy{Mode: ModeAuto, Rules: map[Category]Rule{
		CatFilesystemRead: RuleAllow,
		CatShellExecute:   RuleAllow,
	}})

	c, d := e.EvaluateCommand("run", "ls -la")
	if c.Risk != RiskLow || d.Outcome != OutcomeAllow {
		t.Errorf("ls: classification=%+v decision=%s", c, d)
	}

	c, d = e.EvaluateCommand("run", "sudo rm -rf /")
	if c.Risk != RiskCritical || d.Outcome != OutcomeConfirm {
		t.Errorf("rm -rf: classification=%+v decision=%s", c, d)
	}

	c, d = e.EvaluateCommand("run", "kubectl apply -f deploy.yaml")
	if !c.Production || d.Outcome != OutcomeConfirm {
		t.Errorf("kubectl: classification=%+v decision=%s", c, d)
	}
}

func TestEvaluatorIsConcurrencySafe(t *testing.T) {
	e := NewEvaluator(DefaultPolicy())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if i%2 == 0 {
					e.SetPolicy(Policy{Mode: ModeAuto, Rules: map[Category]Rule{CatShellExecute: RuleConfirm}})
				}
				if got := e.Evaluate(Action{Category: CatShellExecute, Risk: RiskHigh}); got.Outcome != OutcomeConfirm {
					t.Errorf("unexpected outcome under concurrency: %s", got)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}
