// Package permissions enforces what Boop is allowed to do.
//
// Enforcement lives here, in code. Prompt instructions are not a security
// control: a model may request any action, and this package decides.
package permissions

import "fmt"

// Mode is the user-facing execution mode.
type Mode string

const (
	// ModeConfirm asks the user to approve any action requiring permission.
	ModeConfirm Mode = "confirm"
	// ModeAuto performs approved categories of work without prompting.
	ModeAuto Mode = "auto"
)

// Valid reports whether m is a recognised mode.
func (m Mode) Valid() bool { return m == ModeConfirm || m == ModeAuto }

// Risk is the internal severity of an operation, tracked independently of Mode.
type Risk string

const (
	RiskLow      Risk = "low"
	RiskMedium   Risk = "medium"
	RiskHigh     Risk = "high"
	RiskCritical Risk = "critical"
)

// Rule is the configured disposition of one action category.
type Rule string

const (
	// RuleAllow permits the action without prompting.
	RuleAllow Rule = "allow"
	// RuleConfirm requires explicit user approval.
	RuleConfirm Rule = "confirm"
	// RuleDeny refuses the action outright.
	RuleDeny Rule = "deny"
)

// Category names a class of privileged operation.
type Category string

const (
	CatFilesystemRead  Category = "filesystem.read"
	CatFilesystemWrite Category = "filesystem.write"
	CatShellExecute    Category = "shell.execute"
	CatGitRead         Category = "git.read"
	CatGitCommit       Category = "git.commit"
	CatGitPush         Category = "git.push"
	CatNetworkHTTP     Category = "network.http"
	// CatNetworkFetch covers retrieving an arbitrary external URL.
	CatNetworkFetch Category = "network.fetch"
	// CatNetworkSearch covers running a web search, which discloses the
	// query text to a third-party search engine.
	CatNetworkSearch    Category = "network.search"
	CatProductionChange Category = "production.change"
	CatMCP              Category = "mcp.call"
)

// Action is a concrete operation awaiting a permission decision.
type Action struct {
	Category Category `json:"category"`
	Risk     Risk     `json:"risk"`
	// Tool is the registered tool requesting the action.
	Tool string `json:"tool"`
	// Summary is a one-line human-readable description for the approval UI.
	Summary string `json:"summary"`
	// Detail carries the specifics under review, such as a command line,
	// a target path, or a diff.
	Detail string `json:"detail,omitempty"`
	// Paths are the filesystem targets involved, where applicable.
	Paths []string `json:"paths,omitempty"`
	// Production marks an action that may affect production infrastructure.
	Production bool `json:"production"`
}

// Outcome is the result of evaluating an Action.
type Outcome string

const (
	// OutcomeAllow permits the action immediately.
	OutcomeAllow Outcome = "allow"
	// OutcomeConfirm requires the frontend to obtain user approval.
	OutcomeConfirm Outcome = "confirm"
	// OutcomeDeny refuses the action.
	OutcomeDeny Outcome = "deny"
)

// Decision is the evaluator's verdict on an Action.
type Decision struct {
	Outcome Outcome `json:"outcome"`
	// Reason explains the verdict for logs and the approval UI.
	Reason string `json:"reason"`
	// Rule is the configured rule that produced this verdict.
	Rule Rule `json:"rule"`
}

func (d Decision) String() string { return fmt.Sprintf("%s (%s)", d.Outcome, d.Reason) }

// Policy is the configured permission table.
//
// Rules maps a Category to its disposition. Unrestricted bypasses confirmation
// entirely and must only be set from an explicit, deliberately named flag.
type Policy struct {
	Mode         Mode              `json:"mode"`
	Rules        map[Category]Rule `json:"rules"`
	Unrestricted bool              `json:"unrestricted"`
	// ProductionAuthorized records that the user has explicitly authorized
	// production work for this session.
	ProductionAuthorized bool `json:"production_authorized"`
}

// Approver obtains a user decision for actions that require confirmation.
//
// Implementations are provided by each frontend (TUI, WebUI, plain CLI) so the
// core never depends on a specific UI.
type Approver interface {
	// Approve blocks until the user decides or ctx is cancelled. It returns
	// true only on explicit approval.
	Approve(action Action) (bool, error)
}
