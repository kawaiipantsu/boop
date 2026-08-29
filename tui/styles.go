package tui

import "github.com/charmbracelet/lipgloss"

// theme holds every style the view uses.
//
// Styles are gathered in one value rather than scattered as package globals so
// the view has a single place to look, and so colour choices can be changed
// without touching layout code. Adaptive colours keep the UI readable on both
// light and dark terminals; nothing relies on colour alone to carry meaning,
// because plenty of terminals have none.
type theme struct {
	headerBar   lipgloss.Style
	headerBrand lipgloss.Style
	headerDim   lipgloss.Style
	statusIdle  lipgloss.Style
	statusBusy  lipgloss.Style
	statusWait  lipgloss.Style
	statusError lipgloss.Style
	webOn       lipgloss.Style
	webOff      lipgloss.Style

	rule lipgloss.Style

	user      lipgloss.Style
	assistant lipgloss.Style
	reasoning lipgloss.Style
	tool      lipgloss.Style
	output    lipgloss.Style
	errorText lipgloss.Style
	system    lipgloss.Style
	approval  lipgloss.Style

	promptTitle      lipgloss.Style
	promptProduction lipgloss.Style
	promptDetail     lipgloss.Style
	promptRisk       lipgloss.Style
	promptReason     lipgloss.Style
	promptHint       lipgloss.Style
	buttonActive     lipgloss.Style
	buttonIdle       lipgloss.Style

	footer    lipgloss.Style
	footerKey lipgloss.Style
	notice    lipgloss.Style
}

// newTheme returns the default palette.
func newTheme() theme {
	var (
		dim    = lipgloss.AdaptiveColor{Light: "#6c6f85", Dark: "#8a8fa3"}
		accent = lipgloss.AdaptiveColor{Light: "#1f6feb", Dark: "#7aa2f7"}
		good   = lipgloss.AdaptiveColor{Light: "#1a7f37", Dark: "#9ece6a"}
		warn   = lipgloss.AdaptiveColor{Light: "#9a6700", Dark: "#e0af68"}
		bad    = lipgloss.AdaptiveColor{Light: "#b42318", Dark: "#f7768e"}
		text   = lipgloss.AdaptiveColor{Light: "#1c1c1c", Dark: "#c0caf5"}
		barBg  = lipgloss.AdaptiveColor{Light: "#e6e9ef", Dark: "#1f2335"}
	)

	return theme{
		headerBar:   lipgloss.NewStyle().Background(barBg).Foreground(text),
		headerBrand: lipgloss.NewStyle().Background(barBg).Foreground(accent).Bold(true),
		headerDim:   lipgloss.NewStyle().Background(barBg).Foreground(dim),
		statusIdle:  lipgloss.NewStyle().Background(barBg).Foreground(dim).Bold(true),
		statusBusy:  lipgloss.NewStyle().Background(barBg).Foreground(accent).Bold(true),
		statusWait:  lipgloss.NewStyle().Background(barBg).Foreground(warn).Bold(true),
		statusError: lipgloss.NewStyle().Background(barBg).Foreground(bad).Bold(true),
		webOn:       lipgloss.NewStyle().Background(barBg).Foreground(warn).Bold(true),
		webOff:      lipgloss.NewStyle().Background(barBg).Foreground(dim),

		rule: lipgloss.NewStyle().Foreground(dim),

		user:      lipgloss.NewStyle().Foreground(accent).Bold(true),
		assistant: lipgloss.NewStyle().Foreground(text),
		reasoning: lipgloss.NewStyle().Foreground(dim).Italic(true),
		tool:      lipgloss.NewStyle().Foreground(good),
		output:    lipgloss.NewStyle().Foreground(dim),
		errorText: lipgloss.NewStyle().Foreground(bad),
		system:    lipgloss.NewStyle().Foreground(dim),
		approval:  lipgloss.NewStyle().Foreground(warn),

		promptTitle:      lipgloss.NewStyle().Foreground(warn).Bold(true),
		promptProduction: lipgloss.NewStyle().Foreground(bad).Bold(true).Underline(true),
		promptDetail:     lipgloss.NewStyle().Foreground(text).Bold(true),
		promptRisk:       lipgloss.NewStyle().Foreground(warn),
		promptReason:     lipgloss.NewStyle().Foreground(dim),
		promptHint:       lipgloss.NewStyle().Foreground(dim),
		buttonActive:     lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(warn).Bold(true),
		buttonIdle:       lipgloss.NewStyle().Foreground(dim),

		footer:    lipgloss.NewStyle().Foreground(dim),
		footerKey: lipgloss.NewStyle().Foreground(accent),
		notice:    lipgloss.NewStyle().Foreground(warn),
	}
}

// entryStyle maps a transcript kind to its style.
func (t theme) entryStyle(kind EntryKind) lipgloss.Style {
	switch kind {
	case EntryUser:
		return t.user
	case EntryAssistant:
		return t.assistant
	case EntryReasoning:
		return t.reasoning
	case EntryTool:
		return t.tool
	case EntryOutput:
		return t.output
	case EntryError:
		return t.errorText
	case EntryApproval:
		return t.approval
	default:
		return t.system
	}
}

// promptStyleFor maps a prompt row tag to its style.
func (t theme) promptStyleFor(s promptStyle) lipgloss.Style {
	switch s {
	case promptTitle:
		return t.promptTitle
	case promptProduction:
		return t.promptProduction
	case promptDetail:
		return t.promptDetail
	case promptRisk:
		return t.promptRisk
	case promptReason:
		return t.promptReason
	case promptHint:
		return t.promptHint
	default:
		return t.assistant
	}
}

// statusStyle maps an activity status to its header style.
func (t theme) statusStyle(s Status) lipgloss.Style {
	switch s {
	case StatusIdle:
		return t.statusIdle
	case StatusWaiting:
		return t.statusWait
	case StatusError:
		return t.statusError
	default:
		return t.statusBusy
	}
}
