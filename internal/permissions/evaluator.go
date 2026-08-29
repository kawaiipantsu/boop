package permissions

import (
	"fmt"
	"sync"
)

// Evaluator applies a Policy to Actions.
//
// It is the single place where "may this happen?" is answered. Every tool
// invocation is expected to pass through it, and it is safe for concurrent
// use because the agent scheduler evaluates actions from several goroutines.
type Evaluator struct {
	mu     sync.RWMutex
	policy Policy
}

// NewEvaluator returns an Evaluator for the given policy. The policy is
// normalised first, so a zero Policy yields safe behaviour (confirm mode with
// the default rule table) rather than an accidental allow-all.
func NewEvaluator(p Policy) *Evaluator {
	return &Evaluator{policy: NormalizePolicy(p)}
}

// Policy returns a copy of the active policy.
func (e *Evaluator) Policy() Policy {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return clonePolicy(e.policy)
}

// SetPolicy replaces the active policy, for runtime changes such as switching
// mode or authorizing production work for the session.
func (e *Evaluator) SetPolicy(p Policy) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.policy = NormalizePolicy(p)
}

// AuthorizeProduction records that the user has explicitly authorized
// production work for this session (§2.9). It is deliberately a separate,
// explicit call: production authorization is never a side effect of changing
// mode or of any single approval.
func (e *Evaluator) AuthorizeProduction(authorized bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.policy.ProductionAuthorized = authorized
}

// Rule returns the rule that applies to a category under the active policy.
func (e *Evaluator) Rule(cat Category) Rule {
	e.mu.RLock()
	defer e.mu.RUnlock()
	rule, _ := resolveRule(e.policy, cat)
	return rule
}

// Evaluate decides what happens to an action.
//
// Precedence, highest first:
//
//  1. An explicit RuleDeny wins over everything, including unrestricted mode.
//     Deny is the user saying "never", and no other setting may override it.
//  2. Production work without explicit authorization confirms, even in auto
//     mode and even when unrestricted, because §2.9 requires a preview and
//     deliberate intent before Boop touches production.
//  3. A critical-risk action never runs unattended unless the deliberately
//     named unrestricted flag is set.
//  4. Unrestricted mode allows everything that survived the rules above.
//  5. Confirm mode allows only categories explicitly set to allow.
//  6. Auto mode follows the rule table, defaulting unknown categories to
//     confirm.
func (e *Evaluator) Evaluate(action Action) Decision {
	e.mu.RLock()
	policy := e.policy
	e.mu.RUnlock()

	rule, explicit := resolveRule(policy, action.Category)
	label := categoryLabel(action.Category)

	// 1. Explicit denial is absolute.
	if rule == RuleDeny {
		return Decision{
			Outcome: OutcomeDeny,
			Rule:    RuleDeny,
			Reason:  fmt.Sprintf("Denied by policy: %s is set to deny.", label),
		}
	}

	// 2. Production work needs explicit authorization.
	if isProduction(action) && !policy.ProductionAuthorized {
		return Decision{
			Outcome: OutcomeConfirm,
			Rule:    rule,
			Reason: "This may affect production. Boop needs a preview of the change, " +
				"the reason for it, the systems it touches and its risks, plus your " +
				"explicit authorization, before it runs.",
		}
	}

	// 3. Critical risk is never automatic.
	if action.Risk == RiskCritical && !policy.Unrestricted {
		return Decision{
			Outcome: OutcomeConfirm,
			Rule:    rule,
			Reason: fmt.Sprintf("Critical risk: %s can cause irreversible damage, so Boop always asks first.",
				label),
		}
	}

	// 4. Unrestricted mode.
	if policy.Unrestricted {
		return Decision{
			Outcome: OutcomeAllow,
			Rule:    rule,
			Reason:  "Unrestricted mode: running without confirmation, as explicitly requested.",
		}
	}

	// 5. Confirm mode: only explicit allows skip the prompt.
	if policy.Mode != ModeAuto {
		if explicit && rule == RuleAllow {
			return Decision{
				Outcome: OutcomeAllow,
				Rule:    RuleAllow,
				Reason:  fmt.Sprintf("Allowed: %s is permitted by policy.", label),
			}
		}
		return Decision{
			Outcome: OutcomeConfirm,
			Rule:    rule,
			Reason:  fmt.Sprintf("Confirm mode: your approval is required for %s.", label),
		}
	}

	// 6. Auto mode follows the rule table.
	if rule == RuleAllow {
		return Decision{
			Outcome: OutcomeAllow,
			Rule:    RuleAllow,
			Reason:  fmt.Sprintf("Allowed: %s is permitted by policy.", label),
		}
	}
	return Decision{
		Outcome: OutcomeConfirm,
		Rule:    RuleConfirm,
		Reason:  fmt.Sprintf("Approval required: %s is set to confirm.", label),
	}
}

// EvaluateCommand classifies a command line and evaluates the result. It is
// the convenient path for the run tool, and keeps classification and
// evaluation from drifting apart in callers.
func (e *Evaluator) EvaluateCommand(tool, cmdline string) (Classification, Decision) {
	c := ClassifyCommand(cmdline)
	action := c.Action(tool, "Run: "+cmdline, cmdline)
	return c, e.Evaluate(action)
}

// isProduction reports whether an action is subject to the production gate.
// Both the flag and the category count, so a tool that sets only one of them
// is still gated.
func isProduction(action Action) bool {
	return action.Production || action.Category == CatProductionChange
}

// resolveRule returns the rule for a category and whether the policy set it
// explicitly. Unknown rule strings resolve to RuleConfirm: a typo in the
// config must not become an allow.
func resolveRule(policy Policy, cat Category) (rule Rule, explicit bool) {
	if r, ok := policy.Rules[cat]; ok {
		switch r {
		case RuleAllow, RuleConfirm, RuleDeny:
			return r, true
		default:
			return RuleConfirm, false
		}
	}
	if r, ok := DefaultRules()[cat]; ok {
		return r, false
	}
	return RuleConfirm, false
}

// categoryLabel renders a category for an approval prompt.
func categoryLabel(cat Category) string {
	if label, ok := categoryLabels[cat]; ok {
		return label
	}
	if cat == "" {
		return "this action"
	}
	return string(cat)
}

var categoryLabels = map[Category]string{
	CatFilesystemRead:   "reading files",
	CatFilesystemWrite:  "writing files",
	CatShellExecute:     "running shell commands",
	CatGitRead:          "reading git state",
	CatGitCommit:        "committing to git",
	CatGitPush:          "pushing to a git remote",
	CatNetworkHTTP:      "network requests",
	CatNetworkFetch:     "fetching a web page",
	CatNetworkSearch:    "searching the web",
	CatProductionChange: "production changes",
}

// DefaultRules returns the default category rule table from §14: reads are
// free, everything that changes something asks first.
func DefaultRules() map[Category]Rule {
	return map[Category]Rule{
		CatFilesystemRead:   RuleAllow,
		CatFilesystemWrite:  RuleConfirm,
		CatShellExecute:     RuleConfirm,
		CatGitRead:          RuleAllow,
		CatGitCommit:        RuleConfirm,
		CatGitPush:          RuleConfirm,
		CatNetworkHTTP:      RuleConfirm,
		CatNetworkFetch:     RuleConfirm,
		CatNetworkSearch:    RuleAllow,
		CatProductionChange: RuleConfirm,
	}
}

// DefaultPolicy returns the shipped default: confirm mode, default rules, no
// unrestricted bypass and no production authorization.
func DefaultPolicy() Policy {
	return Policy{Mode: ModeConfirm, Rules: DefaultRules()}
}

// NewPolicy builds a Policy from a category rule map, filling in defaults for
// any category the caller omitted and rejecting an invalid mode by falling
// back to confirm. The map is copied, so later mutation by the caller cannot
// change the active policy.
func NewPolicy(mode Mode, rules map[Category]Rule) Policy {
	return NormalizePolicy(Policy{Mode: mode, Rules: rules})
}

// NormalizePolicy returns p with a valid mode, sanitised rule values and no
// shared map state. Unknown modes and unknown rule strings fail closed to
// confirm.
//
// Missing categories are deliberately left missing rather than filled from
// DefaultRules: the Evaluator needs to know which allows the user actually
// wrote, because confirm mode only skips the prompt for those. Callers who
// want the shipped table should start from DefaultPolicy.
func NormalizePolicy(p Policy) Policy {
	out := clonePolicy(p)
	if !out.Mode.Valid() {
		out.Mode = ModeConfirm
	}
	for cat, rule := range out.Rules {
		switch rule {
		case RuleAllow, RuleConfirm, RuleDeny:
		default:
			out.Rules[cat] = RuleConfirm
		}
	}
	return out
}

// clonePolicy deep-copies a policy so callers cannot mutate a live rule table.
func clonePolicy(p Policy) Policy {
	out := p
	if p.Rules != nil {
		out.Rules = make(map[Category]Rule, len(p.Rules))
		for k, v := range p.Rules {
			out.Rules[k] = v
		}
	}
	return out
}
