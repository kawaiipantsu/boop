package tools

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/execution"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

func TestGitToolPermissionClassification(t *testing.T) {
	tool := NewGitTool(&execFakeExecutor{}, execTestWorkspace(t))

	tests := []struct {
		name         string
		sub          string
		args         []string
		wantCategory permissions.Category
		wantRisk     permissions.Risk
		summaryHas   string
	}{
		{"status is a low-risk read", "status", []string{"--short"}, permissions.CatGitRead, permissions.RiskLow, "inspect"},
		{"log is a low-risk read", "log", []string{"-n", "5"}, permissions.CatGitRead, permissions.RiskLow, "history"},
		{"diff is a low-risk read", "diff", nil, permissions.CatGitRead, permissions.RiskLow, "changes"},
		{"show is a low-risk read", "show", []string{"HEAD"}, permissions.CatGitRead, permissions.RiskLow, "object"},
		{"branch listing is a low-risk read", "branch", nil, permissions.CatGitRead, permissions.RiskLow, "branches"},
		{"remote is a low-risk read", "remote", []string{"-v"}, permissions.CatGitRead, permissions.RiskLow, "remotes"},
		{"rev-parse is a low-risk read", "rev-parse", []string{"HEAD"}, permissions.CatGitRead, permissions.RiskLow, "revision"},
		{"fetch touches the network", "fetch", []string{"origin"}, permissions.CatGitRead, permissions.RiskMedium, "fetch"},
		{"add stages", "add", []string{"."}, permissions.CatGitCommit, permissions.RiskLow, "stage"},
		{"commit", "commit", []string{"-m", "feat: x"}, permissions.CatGitCommit, permissions.RiskMedium, "commit"},
		{"push", "push", []string{"origin", "main"}, permissions.CatGitPush, permissions.RiskMedium, "publish"},

		{"force push rewrites remote history", "push", []string{"--force", "origin", "main"}, permissions.CatGitPush, permissions.RiskHigh, "FORCE-PUSH"},
		{"short force push", "push", []string{"-f"}, permissions.CatGitPush, permissions.RiskHigh, "FORCE-PUSH"},
		{"force-with-lease is still a rewrite", "push", []string{"--force-with-lease"}, permissions.CatGitPush, permissions.RiskHigh, "FORCE-PUSH"},
		{"deleting a remote branch", "push", []string{"--delete", "origin", "old"}, permissions.CatGitPush, permissions.RiskHigh, "FORCE-PUSH"},
		{"hard reset discards work", "reset", []string{"--hard", "HEAD~1"}, permissions.CatGitCommit, permissions.RiskHigh, "HARD RESET"},
		{"soft reset is ordinary", "reset", []string{"--soft", "HEAD~1"}, permissions.CatGitCommit, permissions.RiskMedium, "branch pointer"},
		{"clean -fdx deletes files", "clean", []string{"-fdx"}, permissions.CatFilesystemWrite, permissions.RiskHigh, "DELETE untracked"},
		{"clean --dry-run is ordinary", "clean", []string{"-n"}, permissions.CatFilesystemWrite, permissions.RiskMedium, "untracked"},
		{"tag -d deletes a tag", "tag", []string{"-d", "v0.1.0"}, permissions.CatGitCommit, permissions.RiskHigh, "DELETE a tag"},
		{"tag creation is ordinary", "tag", []string{"-a", "v0.1.0", "-m", "x"}, permissions.CatGitCommit, permissions.RiskMedium, "tag"},
		{"branch -D deletes", "branch", []string{"-D", "feature/x"}, permissions.CatGitCommit, permissions.RiskHigh, "DELETE or rename"},
		{"branch creation", "branch", []string{"feature/x"}, permissions.CatGitCommit, permissions.RiskMedium, "create a branch"},
		{"amend rewrites history", "commit", []string{"--amend", "-m", "x"}, permissions.CatGitCommit, permissions.RiskHigh, "AMEND"},
		{"rebase rewrites history", "rebase", []string{"main"}, permissions.CatGitCommit, permissions.RiskHigh, "rewrite local history"},
		{"forced checkout overwrites", "checkout", []string{"-f", "main"}, permissions.CatFilesystemWrite, permissions.RiskHigh, "OVERWRITE"},
		{"checkout of a branch", "checkout", []string{"main"}, permissions.CatFilesystemWrite, permissions.RiskMedium, "switch or restore"},
		{"stash drop discards", "stash", []string{"drop"}, permissions.CatGitCommit, permissions.RiskHigh, "DISCARD"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			action, err := tool.Permission(execTestCall(t, "git", execGitArgs{Subcommand: tc.sub, Args: tc.args}))
			if err != nil {
				t.Fatalf("Permission: %v", err)
			}
			if action.Category != tc.wantCategory {
				t.Errorf("category = %q, want %q", action.Category, tc.wantCategory)
			}
			if action.Risk != tc.wantRisk {
				t.Errorf("risk = %q, want %q", action.Risk, tc.wantRisk)
			}
			if !strings.Contains(action.Summary, tc.summaryHas) {
				t.Errorf("summary %q does not mention %q", action.Summary, tc.summaryHas)
			}
			if !strings.HasPrefix(action.Detail, "git --no-pager "+tc.sub) {
				t.Errorf("detail = %q, want it to show the real command line", action.Detail)
			}
		})
	}
}

func TestGitToolPermissionRefusesUnknownSubcommand(t *testing.T) {
	tool := NewGitTool(&execFakeExecutor{}, execTestWorkspace(t))
	action, err := tool.Permission(execTestCall(t, "git", execGitArgs{Subcommand: "filter-branch"}))
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if action.Risk != permissions.RiskCritical {
		t.Errorf("risk = %q, want critical for an unclassifiable subcommand", action.Risk)
	}
	if !strings.Contains(action.Summary, "not in the boop allowlist") {
		t.Errorf("summary = %q", action.Summary)
	}
}

func TestGitToolPermissionRequiresSubcommand(t *testing.T) {
	tool := NewGitTool(&execFakeExecutor{}, execTestWorkspace(t))
	if _, err := tool.Permission(execTestCall(t, "git", execGitArgs{})); err == nil {
		t.Fatal("expected an error when no subcommand is given")
	}
}

func TestGitToolExecuteBuildsArgv(t *testing.T) {
	ws := execTestWorkspace(t)
	fake := &execFakeExecutor{handler: func(req execution.RunRequest) (execution.RunResult, error) {
		return execution.RunResult{Command: "git", WorkingDir: req.WorkingDir, Stdout: " M internal/tools/git.go\n"}, nil
	}}
	tool := NewGitTool(fake, ws)

	res, err := tool.Execute(context.Background(), execTestCall(t, "git", execGitArgs{
		Subcommand: "status", Args: []string{"--short"},
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	req := fake.last(t)
	if req.Command != "git" {
		t.Errorf("command = %q, want the git binary", req.Command)
	}
	want := []string{"--no-pager", "status", "--short"}
	if !reflect.DeepEqual(req.Args, want) {
		t.Errorf("args = %v, want %v", req.Args, want)
	}
	if req.WorkingDir != ws.Root() {
		t.Errorf("working dir = %q, want %q", req.WorkingDir, ws.Root())
	}
	if req.Env["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("env = %v, want GIT_TERMINAL_PROMPT=0 so credential prompts cannot hang the call", req.Env)
	}
	if res.IsError {
		t.Error("a clean git status must not be an error")
	}
	if !strings.Contains(res.Content, "git --no-pager status --short") {
		t.Errorf("content should show the exact command:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "internal/tools/git.go") {
		t.Errorf("content should include stdout:\n%s", res.Content)
	}
}

func TestGitToolQuotesDisplayedArguments(t *testing.T) {
	fake := &execFakeExecutor{}
	tool := NewGitTool(fake, execTestWorkspace(t))
	res, err := tool.Execute(context.Background(), execTestCall(t, "git", execGitArgs{
		Subcommand: "commit", Args: []string{"-m", "feat: add the run tool"},
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Content, `'feat: add the run tool'`) {
		t.Errorf("commit message should be quoted in the displayed command:\n%s", res.Content)
	}
	if got := fake.last(t).Args; got[len(got)-1] != "feat: add the run tool" {
		t.Errorf("the executed argument must be unquoted: %v", got)
	}
}

func TestGitToolRefusals(t *testing.T) {
	tests := []struct {
		name     string
		args     execGitArgs
		contains string
	}{
		{"unknown subcommand", execGitArgs{Subcommand: "filter-branch"}, "is not permitted"},
		{"no subcommand", execGitArgs{}, "subcommand is required"},
		{"interactive rebase", execGitArgs{Subcommand: "rebase", Args: []string{"-i", "HEAD~3"}}, "cannot be used here"},
		{"interactive add", execGitArgs{Subcommand: "add", Args: []string{"--patch"}}, "cannot be used here"},
		{"escaping working dir", execGitArgs{Subcommand: "status", WorkingDir: "../.."}, "escapes the workspace"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &execFakeExecutor{}
			tool := NewGitTool(fake, execTestWorkspace(t))
			res, err := tool.Execute(context.Background(), execTestCall(t, "git", tc.args))
			if err != nil {
				t.Fatalf("Execute returned a Go error: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected a refusal, got %+v", res)
			}
			if !strings.Contains(res.Content, tc.contains) {
				t.Errorf("content = %q, want it to mention %q", res.Content, tc.contains)
			}
			if len(fake.calls()) != 0 {
				t.Error("a refused git call must not reach the executor")
			}
		})
	}
}

func TestGitToolFailureIsData(t *testing.T) {
	fake := &execFakeExecutor{handler: func(execution.RunRequest) (execution.RunResult, error) {
		return execution.RunResult{ExitCode: 1, Stderr: "fatal: not a git repository\n"}, nil
	}}
	tool := NewGitTool(fake, execTestWorkspace(t))
	res, err := tool.Execute(context.Background(), execTestCall(t, "git", execGitArgs{Subcommand: "status"}))
	if err != nil {
		t.Fatalf("Execute must not return a Go error for a failing git command: %v", err)
	}
	if !res.IsError {
		t.Error("a non-zero git exit must be an error result")
	}
	if !strings.Contains(res.Content, "fatal: not a git repository") {
		t.Errorf("stderr must reach the model:\n%s", res.Content)
	}
	if _, ok := res.Data.(execution.RunResult); !ok {
		t.Errorf("Data = %T, want execution.RunResult", res.Data)
	}
}

func TestGitToolAllowlistCoversSpecifiedSubcommands(t *testing.T) {
	for _, sub := range []string{"status", "log", "diff", "show", "branch", "remote", "rev-parse", "commit", "push"} {
		if _, ok := execGitAllowlist[sub]; !ok {
			t.Errorf("subcommand %q must be in the allowlist", sub)
		}
	}
	for _, sub := range []string{"filter-branch", "reflog", "update-ref", "gc", "config", "daemon"} {
		if _, ok := execGitAllowlist[sub]; ok {
			t.Errorf("subcommand %q must not be in the allowlist", sub)
		}
	}
}

func TestExecHasFlag(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		names []string
		want  bool
	}{
		{"exact long flag", []string{"--force"}, []string{"--force"}, true},
		{"combined short flag", []string{"-fdx"}, []string{"-f"}, true},
		{"short flag not present", []string{"-dn"}, []string{"-f"}, false},
		{"long flag is not matched by letters", []string{"--dry-run"}, []string{"-f"}, false},
		{"operand is not a flag", []string{"force"}, []string{"-f", "--force"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := execHasFlag(tc.args, tc.names...); got != tc.want {
				t.Errorf("execHasFlag(%v, %v) = %v, want %v", tc.args, tc.names, got, tc.want)
			}
		})
	}
}

func TestGitToolImplementsTool(t *testing.T) {
	var tool Tool = NewGitTool(&execFakeExecutor{}, execTestWorkspace(t))
	if tool.Name() != "git" {
		t.Errorf("name = %q", tool.Name())
	}
	schema := tool.Schema()
	props := schema["properties"].(map[string]any)
	if _, ok := props["subcommand"]; !ok {
		t.Error("schema must expose subcommand")
	}
}
