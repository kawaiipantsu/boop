package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kawaiipantsu/boop/internal/project"
)

// prepTimeout bounds a project survey.
//
// It is far longer than asyncTimeout because /prep walks the whole tree and
// shells out to git: twenty seconds is generous for a store query and mean for
// a large monorepo.
const prepTimeout = 2 * time.Minute

// maxSensitiveShown bounds the production-sensitive list in the transcript.
// The full list always goes to Boop.md, which is the durable record.
const maxSensitiveShown = 12

// prepCmd runs the §17 initialization sequence over the workspace.
//
// It is asynchronous because Prep walks the project tree; running it inline
// would freeze the UI for as long as the filesystem takes.
func (m *Model) prepCmd(cmd Command) tea.Cmd {
	if m.app == nil || m.app.Workspace == nil {
		return m.say(EntryError, "no runtime is attached, so there is no project to survey")
	}
	if len(cmd.Args) > 0 {
		// Prep writes Boop.md where it looks, so it is confined to the
		// workspace rather than pointed anywhere the user names.
		return m.say(EntryError, fmt.Sprintf(
			"usage: /%s — it always surveys the current project (%s); use `boop prep <dir>` for another directory",
			cmd.Name, m.app.Workspace.Root()))
	}

	root := m.app.Workspace.Root()
	m.say(EntrySystem, "surveying "+root+"…")

	ctx := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, prepTimeout)
		defer cancel()
		report, err := project.Prep(ctx, root)
		if err != nil {
			return infoMsg{entries: []Entry{{Kind: EntryError, Text: "prep failed: " + err.Error()}}}
		}
		if m.app != nil {
			_, _ = m.app.ReloadMemory()
		}
		return infoMsg{entries: []Entry{{Kind: EntrySystem, Text: formatPrepReport(report)}}}
	}
}

// formatPrepReport renders the ready-state summary §17 step 12 asks for.
//
// It deliberately does not call Report.String(): that form is written for a
// terminal transcript of `boop prep`, and the TUI's report also has to say
// what the running process will and will not pick up from the rewritten
// Boop.md.
func formatPrepReport(r *project.Report) string {
	var b strings.Builder
	b.WriteString("project survey\n")
	fmt.Fprintf(&b, "  root          %s\n", r.Root)

	if info := r.Info; info != nil {
		if info.Name != "" {
			fmt.Fprintf(&b, "  name          %s\n", info.Name)
		}
		if len(info.Languages) > 0 {
			names := make([]string, 0, len(info.Languages))
			for i, l := range info.Languages {
				if i == 6 {
					names = append(names, fmt.Sprintf("… and %d more", len(info.Languages)-i))
					break
				}
				names = append(names, fmt.Sprintf("%s (%d)", l.Name, l.Files))
			}
			fmt.Fprintf(&b, "  languages     %s\n", strings.Join(names, ", "))
		}
		if len(info.Frameworks) > 0 {
			fmt.Fprintf(&b, "  frameworks    %s\n", strings.Join(info.Frameworks, ", "))
		}
		if info.Git.Present {
			fmt.Fprintf(&b, "  git           %s\n", gitSummary(info.Git))
		} else {
			b.WriteString("  git           not a repository\n")
		}
		if len(info.Commands) > 0 {
			b.WriteString("  commands\n")
			for _, c := range info.Commands {
				note := ""
				if c.Inferred {
					note = "  (inferred)"
				}
				fmt.Fprintf(&b, "    %-8s %s%s\n", string(c.Kind), c.Line, note)
			}
		} else {
			b.WriteString("  commands      none detected\n")
		}
	}

	action := "updated"
	if r.MemoryCreated {
		action = "created"
	}
	fmt.Fprintf(&b, "  memory        %s %s\n", action, r.MemoryPath)

	if n := len(r.Sensitive); n > 0 {
		fmt.Fprintf(&b, "\nproduction-sensitive files (%d) — changes here need deliberate intent (§15)\n", n)
		for i, f := range r.Sensitive {
			if i == maxSensitiveShown {
				fmt.Fprintf(&b, "  … and %d more, all listed in Boop.md\n", n-i)
				break
			}
			fmt.Fprintf(&b, "  %-8s %s — %s\n", f.Sensitivity, f.Path, f.Reason)
		}
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	b.WriteString("\nBoop.md has been reloaded into project memory and is immediately available for subsequent turns.")
	return b.String()
}

// gitSummary condenses the repository state onto one line.
func gitSummary(g project.GitInfo) string {
	parts := []string{}
	switch {
	case g.Detached:
		parts = append(parts, "detached at "+shortID(g.Head))
	case g.Branch != "":
		parts = append(parts, "branch "+g.Branch)
	default:
		parts = append(parts, "unknown branch")
	}
	if len(g.Remotes) == 0 {
		parts = append(parts, "no remote")
	} else {
		names := make([]string, 0, len(g.Remotes))
		for _, r := range g.Remotes {
			names = append(names, r.Name)
		}
		parts = append(parts, "remote "+strings.Join(names, "/"))
	}
	switch {
	case !g.DirtyKnown:
		parts = append(parts, "state unknown")
	case g.Dirty:
		parts = append(parts, fmt.Sprintf("%d uncommitted change(s)", g.DirtyFiles))
	default:
		parts = append(parts, "clean")
	}
	return strings.Join(parts, ", ")
}
