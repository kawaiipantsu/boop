package app

import (
	"fmt"

	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/execution"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/tools"
	"github.com/kawaiipantsu/boop/internal/webclient"
)

// ToolDeps are the collaborators the tool registry needs.
type ToolDeps struct {
	Workspace *tools.Workspace
	Executor  execution.Executor
	Web       *webclient.Client
}

// BuildTools assembles the tool registry for a session.
//
// The command-running tools are given the permission engine's real classifier
// rather than their conservative built-in fallback. This matters for more than
// accuracy: the classifier also reports the permission category and whether a
// command reaches production, and without it every command would be filed as
// shell.execute with production false — which would silently skip the
// production gate for something like `terraform apply` (§15).
func BuildTools(cfg *config.Config, deps ToolDeps) (*tools.Registry, error) {
	if deps.Workspace == nil {
		return nil, fmt.Errorf("tools: a workspace is required")
	}
	if deps.Executor == nil {
		return nil, fmt.Errorf("tools: an executor is required")
	}

	classify := func(command string) permissions.Classification {
		return permissions.ClassifyCommand(command)
	}

	reg := tools.NewRegistry()

	// Filesystem, all confined to the workspace.
	reg.Register(tools.NewReadTool(deps.Workspace))
	reg.Register(tools.NewWriteTool(deps.Workspace))
	reg.Register(tools.NewEditTool(deps.Workspace))
	reg.Register(tools.NewApplyPatchTool(deps.Workspace))
	reg.Register(tools.NewListTool(deps.Workspace))
	reg.Register(tools.NewFindTool(deps.Workspace))
	reg.Register(tools.NewSearchTool(deps.Workspace))
	// attach turns a PDF, DOCX, image or encoded text file into something a
	// model can consume. read deliberately still refuses a binary, so this is
	// the entry point for anything that is not plain text.
	reg.Register(tools.NewAttachTool(deps.Workspace))
	reg.Register(tools.NewMemoryTool(deps.Workspace))
	reg.Register(tools.NewTodoTool(nil))
	reg.Register(tools.NewAskTool(nil))

	// Command execution.
	run := tools.NewRunTool(deps.Executor, deps.Workspace)
	run.Classify = classify
	run.DefaultTimeout = cfg.Execution.CommandTimeout.Std()
	reg.Register(run)

	reg.Register(tools.NewGitTool(deps.Executor, deps.Workspace))

	test := tools.NewTestTool(deps.Executor, deps.Workspace)
	test.Classify = classify
	reg.Register(test)

	build := tools.NewBuildTool(deps.Executor, deps.Workspace)
	build.Classify = classify
	reg.Register(build)

	lint := tools.NewLintTool(deps.Executor, deps.Workspace)
	lint.Classify = classify
	reg.Register(lint)

	format := tools.NewFormatTool(deps.Executor, deps.Workspace)
	format.Classify = classify
	reg.Register(format)

	// User-declared custom tools from config
	reg.RegisterCustomTools(cfg.Tools.Custom)

	// Network tools are registered only when outbound access is enabled, so a
	// model is never offered a tool that is configured to refuse (§2.2).
	if cfg.Network.Enabled && deps.Web != nil {
		httpTool := tools.NewHTTPTool()
		httpTool.AllowPrivateNetworks = cfg.Network.AllowPrivateNetworks
		httpTool.MaxResponseBytes = cfg.Network.MaxResponseBytes
		httpTool.MaxRedirects = cfg.Network.MaxRedirects
		httpTool.Timeout = cfg.Network.Timeout.Std()
		reg.Register(httpTool)

		reg.Register(tools.NewFetchTool(deps.Web))
		reg.Register(tools.NewWebSearchTool(deps.Web))
	}

	return reg, nil
}
