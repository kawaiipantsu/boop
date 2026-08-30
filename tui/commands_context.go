package tui

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kawaiipantsu/boop/internal/documents"
	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/session"
)

// maxSelectedFileBytes bounds one explicitly selected file.
//
// Selection is resent with every request, so a large file is not a one-off
// cost: it is a tax on the rest of the session. Refusing is more honest than
// silently truncating something the user asked for by name.
const maxSelectedFileBytes = 256 << 10

// contextCmd implements /context, /context add <path> and /context clear (§47).
func (m *Model) contextCmd(cmd Command) tea.Cmd {
	switch cmd.Arg(0) {
	case "", "show":
		return m.say(EntrySystem, m.contextText())
	case "clear":
		if m.selection.IsEmpty() {
			return m.say(EntrySystem, "nothing was selected; the conversation itself is untouched — use /reset for that")
		}
		m.selection.Clear()
		return m.say(EntrySystem, "selected context cleared; requests now carry the conversation only")
	case "add":
		return m.contextAdd(strings.Join(cmd.Args[1:], " "))
	default:
		return m.say(EntryError, "usage: /context [add <path>|clear]")
	}
}

// contextAdd pins a file so it is sent with every subsequent request.
//
// The read is direct rather than through the read tool, because this is the
// operator naming a file, not the model asking for one: §64.2 governs actions
// a model initiates. The workspace boundary still applies — Resolve rejects
// anything outside the project, symlinks included — so /context add is not a
// licence to pull /etc/shadow into a prompt.
func (m *Model) contextAdd(path string) tea.Cmd {
	path = strings.TrimSpace(path)
	if path == "" {
		return m.say(EntryError, "usage: /context add <path>")
	}
	if m.app == nil || m.app.Workspace == nil {
		return m.say(EntryError, "no runtime is attached, so there is no workspace to read from")
	}

	abs, err := m.app.Workspace.Resolve(path)
	if err != nil {
		return m.say(EntryError, "cannot add "+path+": "+err.Error())
	}
	info, err := os.Stat(abs)
	switch {
	case err != nil:
		return m.say(EntryError, "cannot add "+path+": "+err.Error())
	case info.IsDir():
		return m.say(EntryError, path+" is a directory; add the files you want individually")
	case info.Size() > maxSelectedFileBytes:
		return m.say(EntryError, fmt.Sprintf(
			"%s is %d bytes, over the %d-byte selection limit; it would be resent with every request",
			path, info.Size(), maxSelectedFileBytes))
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return m.say(EntryError, "cannot read "+path+": "+err.Error())
	}
	if isBinary(data) {
		return m.say(EntryError, path+" looks like a binary file; there is nothing useful to send a text model")
	}
	rel := m.app.Workspace.Rel(abs)
	m.selection.AddFile(rel, string(data))
	return m.say(EntrySystem, fmt.Sprintf("added %s (~%d tokens); it is sent with every request until /context clear",
		rel, estimateTokens(string(data))))
}

// contextText describes what is currently sent to the model (§47).
func (m *Model) contextText() string {
	var b strings.Builder
	b.WriteString("conversation context\n")
	byRole := map[provider.Role]int{}
	tokensByRole := map[provider.Role]int{}
	for _, msg := range m.history {
		byRole[msg.Role]++
		tokensByRole[msg.Role] += estimateTokens(msg.Content)
	}
	roles := []provider.Role{provider.RoleSystem, provider.RoleUser, provider.RoleAssistant, provider.RoleTool}
	for _, role := range roles {
		if byRole[role] == 0 {
			continue
		}
		fmt.Fprintf(&b, "  %-10s %3d messages  ~%d tokens\n", role, byRole[role], tokensByRole[role])
	}
	fmt.Fprintf(&b, "  total      %3d messages  ~%d tokens (estimated at 4 characters per token)\n",
		len(m.history), m.contextTokens())

	files := m.selection.Files()
	if len(files) == 0 {
		b.WriteString("\nno files are explicitly selected; add one with /context add <path>")
		return b.String()
	}
	total := 0
	fmt.Fprintf(&b, "\nselected files (%d), resent with every request\n", len(files))
	for _, f := range files {
		n := estimateTokens(f.Content)
		total += n
		fmt.Fprintf(&b, "  %-40s ~%d tokens\n", f.Path, n)
	}
	fmt.Fprintf(&b, "  total      ~%d tokens\n", total)
	b.WriteString("drop them all with /context clear")
	return b.String()
}

// requestHistory is the conversation as it is sent to the model.
//
// Explicitly selected files ride along as one extra system message inserted
// just before the newest user turn, and are not stored in m.history: they are
// a standing attachment, not something that was said, so they must not be
// duplicated once per turn or written to the session transcript.
//
// The selection is delivered this way rather than through the loop's
// ContextManager because session.Input — the only place a *session.Selection
// is accepted — is built inside app.Loop.prompt, which the TUI cannot reach.
func (m *Model) requestHistory() []provider.Message {
	history := append([]provider.Message(nil), m.history...)
	block := selectionMessage(m.selection)
	if block == "" {
		return history
	}
	msg := provider.Message{Role: provider.RoleSystem, Content: block}
	if n := len(history); n > 0 && history[n-1].Role == provider.RoleUser {
		return append(history[:n-1:n-1], msg, history[n-1])
	}
	return append(history, msg)
}

// selectionMessage renders the selected files, or "" when nothing is selected.
func selectionMessage(sel *session.Selection) string {
	files := sel.Files()
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Files the user has explicitly added to the context. They are provided in full; do not re-read them unless you need a newer version.\n")
	for _, f := range files {
		fmt.Fprintf(&b, "\n### %s\n```%s\n%s\n```\n", f.Path, fenceLanguage(f.Path), strings.TrimRight(f.Content, "\n"))
	}
	return strings.TrimRight(b.String(), "\n")
}

// fenceLanguage guesses a fence hint from the extension. It is cosmetic: a
// wrong guess costs nothing, so the table stays short rather than exhaustive.
func fenceLanguage(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".js", ".mjs", ".cjs":
		return "javascript"
	case ".ts", ".tsx":
		return "typescript"
	case ".rs":
		return "rust"
	case ".sh", ".bash", ".zsh":
		return "bash"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".md":
		return "markdown"
	case ".sql":
		return "sql"
	case ".html":
		return "html"
	case ".css":
		return "css"
	default:
		return ""
	}
}

// isBinary reports whether data is unlikely to be text.
//
// A NUL byte in the first few kilobytes is the same cheap heuristic the read
// tool uses, and it is enough to stop an executable being pinned into every
// prompt for the rest of the session.
func isBinary(data []byte) bool {
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	return bytes.IndexByte(head, 0) >= 0
}

// attachCmd implements /attach <path> (§27).
func (m *Model) attachCmd(cmd Command) tea.Cmd {
	path := strings.TrimSpace(cmd.Rest)
	if path == "" {
		return m.say(EntryError, "usage: /attach <path>")
	}
	if m.app == nil || m.app.Workspace == nil {
		return m.say(EntryError, "no runtime is attached, so there is no workspace to read from")
	}

	abs, err := m.app.Workspace.Resolve(path)
	if err != nil {
		return m.say(EntryError, "cannot attach "+path+": "+err.Error())
	}

	doc, err := documents.Load(abs, documents.Options{})
	if err != nil {
		return m.say(EntryError, "cannot attach "+path+": "+err.Error())
	}

	return m.say(EntrySystem, fmt.Sprintf("attached %s (%s, %d bytes)", doc.Filename, doc.Type.MIMEType, doc.Size))
}
