package app

import (
	_ "embed"
	"fmt"
	"strings"
)

// systemPrompt is the versioned prompt from prompts/system.md.
//
// It is embedded rather than read from disk so a released binary behaves
// identically wherever it runs, with no install layout to get wrong (§29).
//
//go:embed prompts/system.md
var systemPrompt string

// DefaultSystemPrompt returns Boop's system prompt.
func DefaultSystemPrompt() string { return strings.TrimSpace(systemPrompt) }

// PromptContext is the runtime detail appended to the system prompt.
//
// The model is told what it is actually working with — the platform, the
// project root, the tools it has and the permission mode in force — because
// guessing at any of those produces confidently wrong commands.
type PromptContext struct {
	OS          string
	Arch        string
	Shell       string
	WorkingDir  string
	Provider    string
	Model       string
	Mode        string
	Tools       []string
	NetworkOn   bool
	ProjectInfo string
}

// Render returns the system prompt with the runtime context appended.
func (p PromptContext) Render(base string) string {
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n\n## Environment\n\n")
	fmt.Fprintf(&b, "- Platform: %s/%s\n", p.OS, p.Arch)
	if p.Shell != "" {
		fmt.Fprintf(&b, "- Shell: %s\n", p.Shell)
	}
	fmt.Fprintf(&b, "- Working directory: %s\n", p.WorkingDir)
	fmt.Fprintf(&b, "- Model: %s via %s\n", p.Model, p.Provider)
	fmt.Fprintf(&b, "- Execution mode: %s\n", p.Mode)
	if len(p.Tools) > 0 {
		fmt.Fprintf(&b, "- Available tools: %s\n", strings.Join(p.Tools, ", "))
	}
	if !p.NetworkOn {
		b.WriteString("- Outbound web access is disabled. Do not claim you looked something up online.\n")
	}
	if strings.TrimSpace(p.ProjectInfo) != "" {
		b.WriteString("\n## Project memory (Boop.md)\n\n")
		b.WriteString(strings.TrimSpace(p.ProjectInfo))
		b.WriteString("\n")
	}
	return b.String()
}
