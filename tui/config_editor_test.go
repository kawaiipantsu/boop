package tui

import (
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// focusField moves the editor cursor to the field with the given label.
func focusField(t *testing.T, m *Model, label string) {
	t.Helper()
	target := -1
	for i, f := range m.editor.fields {
		if f.label == label {
			target = i
			break
		}
	}
	if target < 0 {
		t.Fatalf("no editor field labelled %q", label)
	}
	for m.editor.cursor < target {
		send(m, key("down"))
	}
	for m.editor.cursor > target {
		send(m, key("up"))
	}
}

func openEditor(t *testing.T, m *Model) {
	t.Helper()
	runCommand(t, m, "/config edit")
	if m.editor == nil {
		t.Fatal("/config edit did not open the editor")
	}
}

func TestConfigEditorOpensAndCancelsCleanly(t *testing.T) {
	m := newAttachedModel(t, nil)
	openEditor(t, m)

	// It owns the screen: a keystroke that would otherwise type into the
	// composer moves the cursor instead.
	before := m.editor.cursor
	send(m, key("down"))
	if m.editor.cursor != before+1 {
		t.Fatalf("down did not move the editor cursor")
	}
	if m.input.Value() != "" {
		t.Fatalf("keystrokes leaked into the composer: %q", m.input.Value())
	}

	send(m, key("esc"))
	if m.editor != nil {
		t.Fatal("esc did not close the editor")
	}
	if strings.Contains(transcriptText(m), "discarded") {
		t.Fatal("a no-op close should say nothing about discarded changes")
	}
}

// The editor replaces the transcript body but must not disturb the row budget:
// View still renders exactly one screen at any size.
func TestConfigEditorKeepsTheLayoutExact(t *testing.T) {
	m := newAttachedModel(t, nil)
	openEditor(t, m)
	for _, sz := range []struct{ w, h int }{{100, 40}, {80, 24}, {60, 12}, {40, 8}, {30, 6}} {
		send(m, tea.WindowSizeMsg{Width: sz.w, Height: sz.h})
		if got := len(strings.Split(m.View(), "\n")); got != sz.h {
			t.Errorf("%dx%d: View rendered %d rows, want %d", sz.w, sz.h, got, sz.h)
		}
	}
}

func TestConfigEditorTogglesBoolAndSaves(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BOOP_CONFIG_DIR", dir)
	m := newAttachedModel(t, func(c *config.Config) { c.Agents.Enabled = false })
	openEditor(t, m)

	focusField(t, m, "agents enabled")
	send(m, key("right")) // toggle off -> on

	if got := m.editor.fields[m.editor.cursor].get(m.editor.draft); got != "on" {
		t.Fatalf("draft value = %q, want on", got)
	}

	send(m, key("ctrl+s"))
	if m.editor != nil {
		t.Fatal("ctrl+s did not close the editor")
	}
	if !m.app.Config().Agents.Enabled {
		t.Fatal("the running config was not updated")
	}
	disk, err := config.LoadFrom(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !disk.Agents.Enabled {
		t.Fatal("config.yaml was not written")
	}
	if txt := transcriptText(m); !strings.Contains(txt, "config saved") {
		t.Fatalf("no save confirmation:\n%s", txt)
	}
}

func TestConfigEditorTextFieldValidatesBeforeCommitting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BOOP_CONFIG_DIR", dir)
	m := newAttachedModel(t, nil)
	openEditor(t, m)

	focusField(t, m, "max tool iterations")
	send(m, key("enter")) // enter edit mode
	if !m.editor.editing {
		t.Fatal("enter did not start editing a text field")
	}
	// Clear the seeded buffer and type garbage.
	for m.editor.buf != "" {
		send(m, key("backspace"))
	}
	typeText(m, "abc")
	send(m, key("enter"))
	if !m.editor.editing || m.editor.err == "" {
		t.Fatalf("garbage was accepted: editing=%v err=%q", m.editor.editing, m.editor.err)
	}

	for m.editor.buf != "" {
		send(m, key("backspace"))
	}
	typeText(m, "7")
	send(m, key("enter"))
	if m.editor.editing || m.editor.err != "" {
		t.Fatalf("a valid value was not committed: err=%q", m.editor.err)
	}
	if m.editor.draft.Execution.MaxToolIterations != 7 {
		t.Fatalf("draft iterations = %d, want 7", m.editor.draft.Execution.MaxToolIterations)
	}

	send(m, key("ctrl+s"))
	if m.app.Config().Execution.MaxToolIterations != 7 {
		t.Fatalf("running iterations = %d, want 7", m.app.Config().Execution.MaxToolIterations)
	}
}

func TestConfigEditorCyclesEnumLive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BOOP_CONFIG_DIR", dir)
	m := newAttachedModel(t, func(c *config.Config) { c.Execution.Mode = permissions.ModeConfirm })
	openEditor(t, m)

	focusField(t, m, "execution mode")
	send(m, key("right"))
	if m.editor.fields[m.editor.cursor].get(m.editor.draft) != string(permissions.ModeAuto) {
		t.Fatalf("mode did not cycle to auto")
	}

	send(m, key("ctrl+s"))
	if m.app.Config().Execution.Mode != permissions.ModeAuto {
		t.Fatal("running mode not updated")
	}
	if m.app.Evaluator.Policy().Mode != permissions.ModeAuto {
		t.Fatal("evaluator did not move with the config (mode is live)")
	}
	if txt := transcriptText(m); !strings.Contains(txt, "all live") {
		t.Fatalf("a live-only change should say so:\n%s", txt)
	}
}

func TestConfigEditorReportsRestartOnlyChanges(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BOOP_CONFIG_DIR", dir)
	m := newAttachedModel(t, func(c *config.Config) { c.Web.Enabled = false })
	openEditor(t, m)

	focusField(t, m, "WebUI enabled")
	send(m, key(" ")) // space toggles

	send(m, key("ctrl+s"))
	txt := transcriptText(m)
	if !strings.Contains(txt, "restart to pick up") || !strings.Contains(txt, "web") {
		t.Fatalf("restart-only change was not reported as such:\n%s", txt)
	}
}

func TestConfigEditorDiscardsOnCancel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BOOP_CONFIG_DIR", dir)
	m := newAttachedModel(t, func(c *config.Config) { c.Agents.Enabled = false })
	openEditor(t, m)

	focusField(t, m, "agents enabled")
	send(m, key("right"))
	send(m, key("esc"))

	if m.editor != nil {
		t.Fatal("esc did not close the editor")
	}
	if m.app.Config().Agents.Enabled {
		t.Fatal("a cancelled edit was applied anyway")
	}
	if txt := transcriptText(m); !strings.Contains(txt, "discarded") {
		t.Fatalf("a cancelled edit with changes should say so:\n%s", txt)
	}
}

func TestConfigEditorNeverExposesCredentials(t *testing.T) {
	const secret = "sk-editor-must-not-show-this"
	t.Setenv("BOOP_EDITOR_SECRET_ENV", secret)
	m := newAttachedModel(t, func(c *config.Config) {
		pc := c.Providers["ollama"]
		pc.APIKeyEnv = "BOOP_EDITOR_SECRET_ENV" // resolves to the secret at runtime
		c.Providers["ollama"] = pc
	})
	openEditor(t, m)

	send(m, tea.WindowSizeMsg{Width: 100, Height: 40})
	full := strings.Join(m.editorRows(), "\n") + "\n" + m.View()
	if strings.Contains(full, secret) {
		t.Fatalf("the editor rendered a credential:\n%s", full)
	}
	for _, f := range m.editor.fields {
		if strings.Contains(strings.ToLower(f.label), "key") || strings.Contains(strings.ToLower(f.label), "token") {
			t.Fatalf("editor exposes a credential field: %q", f.label)
		}
	}
}
