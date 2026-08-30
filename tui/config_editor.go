package tui

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/logging"
	"github.com/kawaiipantsu/boop/internal/permissions"
)

// fieldKind decides how a configField is edited and drawn.
type fieldKind int

const (
	fieldText fieldKind = iota // free text / number, edited in place
	fieldBool                  // toggled with space or ←/→
	fieldEnum                  // cycled through a fixed option list with ←/→
)

// configField is one editable row of the full-screen /config editor.
//
// get renders the current value of a *config.Config as a string; set parses a
// string back onto one, returning an error that rejects the edit. No field ever
// touches a credential — api_key_env, token_env and provider headers are
// deliberately absent, so the editor cannot display or change a secret (§45).
type configField struct {
	label   string
	kind    fieldKind
	options []string
	get     func(*config.Config) string
	set     func(*config.Config, string) error
	// live is true when a running process can pick the change up without a
	// restart, mirroring app.ApplyConfig's groups.
	live bool
}

// configEditor is the modal state for the full-screen editor. While it is
// non-nil it owns the keyboard and replaces the transcript body.
type configEditor struct {
	fields  []configField
	draft   *config.Config // clone of the running config, mutated as edited
	touched map[int]bool   // field indices the user actually changed
	cursor  int
	top     int // first visible field row, for scrolling
	editing bool
	buf     string
	err     string
}

// newConfigEditor builds the editor over a clone of cfg. providerNames is the
// set of providers that are actually configured, used for the provider field
// and to validate a switch.
func newConfigEditor(cfg *config.Config, providerNames []string) *configEditor {
	activeProvider := func(c *config.Config) string { return c.Provider }

	fields := []configField{
		{
			label: "provider", kind: fieldEnum, options: providerNames, live: true,
			get: func(c *config.Config) string { return c.Provider },
			set: func(c *config.Config, v string) error {
				for _, n := range providerNames {
					if n == v {
						c.Provider = v
						return nil
					}
				}
				return fmt.Errorf("no provider named %q is configured", v)
			},
		},
		{
			label: "model", kind: fieldText, live: true,
			get: func(c *config.Config) string {
				if c.Model == "" {
					return "(provider default)"
				}
				return c.Model
			},
			set: func(c *config.Config, v string) error {
				v = strings.TrimSpace(v)
				if v == "default" || v == "(provider default)" {
					v = ""
				}
				c.Model = v
				return nil
			},
		},
		{
			label: "base URL", kind: fieldText, live: false,
			get: func(c *config.Config) string { return c.Providers[activeProvider(c)].BaseURL },
			set: func(c *config.Config, v string) error {
				name := activeProvider(c)
				pc, ok := c.Providers[name]
				if !ok {
					return fmt.Errorf("no provider named %q", name)
				}
				pc.BaseURL = strings.TrimSpace(v)
				c.Providers[name] = pc
				return nil
			},
		},
		{
			label: "execution mode", kind: fieldEnum,
			options: []string{string(permissions.ModeConfirm), string(permissions.ModeAuto)}, live: true,
			get: func(c *config.Config) string { return string(c.Execution.Mode) },
			set: func(c *config.Config, v string) error {
				mode := permissions.Mode(v)
				if !mode.Valid() {
					return fmt.Errorf("mode must be confirm or auto")
				}
				c.Execution.Mode = mode
				return nil
			},
		},
		{
			label: "max tool iterations", kind: fieldText, live: true,
			get: func(c *config.Config) string { return strconv.Itoa(c.Execution.MaxToolIterations) },
			set: intSetter(1, 1000, func(c *config.Config, n int) { c.Execution.MaxToolIterations = n }),
		},
		{
			label: "max retries / command", kind: fieldText, live: true,
			get: func(c *config.Config) string { return strconv.Itoa(c.Execution.MaxRetriesPerCommand) },
			set: intSetter(0, 100, func(c *config.Config, n int) { c.Execution.MaxRetriesPerCommand = n }),
		},
		{
			label: "command timeout", kind: fieldText, live: false,
			get: func(c *config.Config) string { return c.Execution.CommandTimeout.Std().String() },
			set: func(c *config.Config, v string) error {
				d, err := time.ParseDuration(strings.TrimSpace(v))
				if err != nil || d <= 0 {
					return fmt.Errorf("a duration like 90s, 5m, 1h30m")
				}
				c.Execution.CommandTimeout = config.Duration(d)
				return nil
			},
		},
		{
			label: "agents enabled", kind: fieldBool, live: true,
			get: boolGet(func(c *config.Config) bool { return c.Agents.Enabled }),
			set: boolSet(func(c *config.Config, b bool) { c.Agents.Enabled = b }),
		},
		{
			label: "max agents", kind: fieldText, live: true,
			get: func(c *config.Config) string { return strconv.Itoa(c.Agents.Max) },
			set: intSetter(1, 64, func(c *config.Config, n int) { c.Agents.Max = n }),
		},
		{
			label: "outbound web access", kind: fieldBool, live: false,
			get: boolGet(func(c *config.Config) bool { return c.Network.Enabled }),
			set: boolSet(func(c *config.Config, b bool) { c.Network.Enabled = b }),
		},
		{
			label: "WebUI enabled", kind: fieldBool, live: false,
			get: boolGet(func(c *config.Config) bool { return c.Web.Enabled }),
			set: boolSet(func(c *config.Config, b bool) { c.Web.Enabled = b }),
		},
		{
			label: "WebUI listen", kind: fieldText, live: false,
			get: func(c *config.Config) string { return c.Web.Listen },
			set: func(c *config.Config, v string) error {
				v = strings.TrimSpace(v)
				if net.ParseIP(v) == nil {
					return fmt.Errorf("an IP address, e.g. 127.0.0.1 or 0.0.0.0")
				}
				c.Web.Listen = v
				return nil
			},
		},
		{
			label: "WebUI port", kind: fieldText, live: false,
			get: func(c *config.Config) string { return strconv.Itoa(c.Web.Port) },
			set: intSetter(1, 65535, func(c *config.Config, n int) { c.Web.Port = n }),
		},
		{
			label: "log level", kind: fieldEnum,
			options: []string{"trace", "debug", "info", "warn", "error"}, live: false,
			get: func(c *config.Config) string { return c.Logging.Level },
			set: func(c *config.Config, v string) error {
				if _, err := logging.ParseLevel(v); err != nil {
					return fmt.Errorf("trace, debug, info, warn or error")
				}
				c.Logging.Level = v
				return nil
			},
		},
		{
			label: "log format", kind: fieldEnum, options: []string{"text", "json"}, live: false,
			get: func(c *config.Config) string { return c.Logging.Format },
			set: func(c *config.Config, v string) error {
				if v != "text" && v != "json" {
					return fmt.Errorf("text or json")
				}
				c.Logging.Format = v
				return nil
			},
		},
	}

	return &configEditor{
		fields:  fields,
		draft:   cfg.Clone(),
		touched: map[int]bool{},
	}
}

func intSetter(lo, hi int, assign func(*config.Config, int)) func(*config.Config, string) error {
	return func(c *config.Config, v string) error {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < lo || n > hi {
			return fmt.Errorf("a whole number from %d to %d", lo, hi)
		}
		assign(c, n)
		return nil
	}
}

func boolGet(read func(*config.Config) bool) func(*config.Config) string {
	return func(c *config.Config) string {
		if read(c) {
			return "on"
		}
		return "off"
	}
}

func boolSet(assign func(*config.Config, bool)) func(*config.Config, string) error {
	return func(c *config.Config, v string) error {
		assign(c, v == "on" || v == "true")
		return nil
	}
}

// anyRestartTouched reports whether a restart-only field was changed.
func (e *configEditor) anyRestartTouched() bool {
	for i := range e.touched {
		if !e.fields[i].live {
			return true
		}
	}
	return false
}

// cycle moves an enum field to its next (dir=+1) or previous (dir=-1) option.
func (f configField) cycle(cur string, dir int) string {
	if len(f.options) == 0 {
		return cur
	}
	idx := 0
	for i, o := range f.options {
		if o == cur {
			idx = i
			break
		}
	}
	idx = (idx + dir + len(f.options)) % len(f.options)
	return f.options[idx]
}

// ---------------------------------------------------------------------------
// opening, key handling and saving
// ---------------------------------------------------------------------------

// openConfigEditor implements /config edit.
func (m *Model) openConfigEditor() tea.Cmd {
	if m.app == nil || m.app.Config() == nil {
		return m.say(EntryError, "no runtime is attached")
	}
	names := []string{}
	if m.app.Router != nil {
		names = m.app.Router.Registry().Names()
	}
	m.editor = newConfigEditor(m.app.Config(), names)
	m.relayout()
	return nil
}

// editorKey handles a keystroke while the editor owns the screen.
func (m *Model) editorKey(msg tea.KeyMsg) tea.Cmd {
	e := m.editor
	field := e.fields[e.cursor]

	if e.editing {
		switch msg.String() {
		case "enter":
			if err := field.set(e.draft, e.buf); err != nil {
				e.err = err.Error()
				return nil
			}
			e.touched[e.cursor] = true
			e.editing, e.err = false, ""
			return nil
		case "esc":
			e.editing, e.err, e.buf = false, "", ""
			return nil
		case "backspace":
			if e.buf != "" {
				e.buf = e.buf[:len(e.buf)-1]
			}
			return nil
		default:
			if len(msg.Runes) > 0 {
				e.buf += string(msg.Runes)
			}
			return nil
		}
	}

	switch msg.String() {
	case "esc":
		return m.closeConfigEditor(false)
	case "ctrl+s":
		return m.saveConfigEditor()
	case "up", "ctrl+p", "shift+tab":
		if e.cursor > 0 {
			e.cursor--
		}
	case "down", "ctrl+n", "tab":
		if e.cursor < len(e.fields)-1 {
			e.cursor++
		}
	case "home", "g":
		e.cursor = 0
	case "end", "G":
		e.cursor = len(e.fields) - 1
	case "left", "h":
		m.editorAdjust(-1)
	case "right", "l", " ":
		m.editorAdjust(1)
	case "enter":
		switch field.kind {
		case fieldText:
			e.editing, e.err = true, ""
			e.buf = rawValue(field, e.draft)
		default:
			m.editorAdjust(1)
		}
	}
	e.err = ""
	m.clampEditorScroll()
	return nil
}

// editorAdjust toggles a bool or cycles an enum at the cursor.
func (m *Model) editorAdjust(dir int) {
	e := m.editor
	field := e.fields[e.cursor]
	switch field.kind {
	case fieldBool:
		next := "off"
		if field.get(e.draft) == "off" {
			next = "on"
		}
		_ = field.set(e.draft, next)
		e.touched[e.cursor] = true
	case fieldEnum:
		_ = field.set(e.draft, field.cycle(field.get(e.draft), dir))
		e.touched[e.cursor] = true
	}
}

// rawValue is the string a text field should seed its edit buffer with — the
// stored value rather than a display placeholder.
func rawValue(f configField, c *config.Config) string {
	v := f.get(c)
	if v == "(provider default)" {
		return ""
	}
	return v
}

// closeConfigEditor leaves the editor. When save is false any edits are
// discarded; the transcript says so only if there were any.
func (m *Model) closeConfigEditor(saved bool) tea.Cmd {
	n := len(m.editor.touched)
	m.editor = nil
	m.relayout()
	if saved || n == 0 {
		return nil
	}
	return m.say(EntrySystem, fmt.Sprintf("config editor closed — %s discarded", plural(n, "change")))
}

// saveConfigEditor validates the draft, writes config.yaml and swaps the
// change into the running process, mirroring the single-field set commands.
func (m *Model) saveConfigEditor() tea.Cmd {
	e := m.editor
	if len(e.touched) == 0 {
		return m.closeConfigEditor(false)
	}
	if _, err := e.draft.Validate(); err != nil {
		e.err = firstLine(err.Error())
		return nil
	}

	// Re-apply only the touched fields, in field order, to both the reloaded
	// disk config (so a --flag override or a concurrent edit is not clobbered)
	// and a clone of the running config.
	apply := func(dst *config.Config) error {
		for i := range e.fields {
			if !e.touched[i] {
				continue
			}
			if err := e.fields[i].set(dst, e.fields[i].get(e.draft)); err != nil {
				return fmt.Errorf("%s: %w", e.fields[i].label, err)
			}
		}
		return nil
	}

	path, warnings, err := persistConfigField(func(c *config.Config) { _ = apply(c) })
	if err != nil {
		e.err = "could not save: " + firstLine(err.Error())
		return nil
	}

	next := m.app.Config().Clone()
	if err := apply(next); err != nil {
		e.err = err.Error()
		return nil
	}
	restart := m.app.ApplyConfig(next)

	// Move the live fleet in step with agents.enabled / agents.max.
	if c := m.coordinator(); c != nil {
		c.SetEnabled(next.Agents.Enabled)
		_ = c.SetMax(next.Agents.Max)
	}
	if next.Agents.Enabled {
		m.syncAgentCount()
	} else {
		m.fleet, m.agentsActive = nil, 0
	}

	changed := len(e.touched)
	m.closeConfigEditor(true)

	var b strings.Builder
	fmt.Fprintf(&b, "config saved to %s — %s", path, plural(changed, "field"))
	if len(restart) == 0 {
		b.WriteString(" applied, all live")
	} else {
		fmt.Fprintf(&b, " applied; restart to pick up: %s", strings.Join(restart, ", "))
	}
	for _, w := range warnings {
		fmt.Fprintf(&b, "\nwarning: %s", w)
	}
	return tea.Batch(m.say(EntrySystem, b.String()), m.persistSelection())
}

func (m *Model) clampEditorScroll() {
	e := m.editor
	rows := m.editorFieldRows()
	if e.cursor < e.top {
		e.top = e.cursor
	}
	if e.cursor >= e.top+rows {
		e.top = e.cursor - rows + 1
	}
	if e.top < 0 {
		e.top = 0
	}
}

// editorFieldRows is how many field lines fit in the body, leaving room for the
// title, the hint line and the footnote.
func (m *Model) editorFieldRows() int {
	return maxInt(1, m.layout.Body-4)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ---------------------------------------------------------------------------
// rendering
// ---------------------------------------------------------------------------

// editorRows renders the editor into exactly m.layout.Body rows.
func (m *Model) editorRows() []string {
	e := m.editor
	width := m.layout.ContentWidth()
	pad := strings.Repeat(" ", framePadding)
	out := make([]string, 0, m.layout.Body)

	title := "edit configuration"
	if e.anyRestartTouched() {
		title += "  (some changes need a restart)"
	}
	out = append(out, pad+m.theme.promptTitle.Render(truncate(title, width)))
	out = append(out, "")

	rows := m.editorFieldRows()
	end := minInt(len(e.fields), e.top+rows)
	labelW := 22
	for i := e.top; i < end; i++ {
		f := e.fields[i]
		cursor := "  "
		if i == e.cursor {
			cursor = m.theme.user.Render("> ")
		}

		var value string
		switch {
		case i == e.cursor && e.editing:
			value = m.theme.buttonActive.Render(e.buf + "▏")
		case f.kind == fieldText:
			v := f.get(e.draft)
			if v == "" {
				v = "—"
			}
			if i == e.cursor {
				value = m.theme.user.Render(v)
			} else {
				value = v
			}
		default: // bool / enum
			token := "[" + f.get(e.draft) + "]"
			if i == e.cursor {
				value = m.theme.buttonActive.Render(token)
			} else {
				value = m.theme.buttonIdle.Render(token)
			}
		}

		tag := m.theme.system.Render("live")
		if !f.live {
			tag = m.theme.approval.Render("restart")
		}
		mark := " "
		if e.touched[i] {
			mark = m.theme.user.Render("*")
		}

		label := f.label
		if len(label) > labelW {
			label = label[:labelW]
		}
		// Values and tags are short and bounded, and styles add no width, so the
		// row is composed directly rather than truncated on a plain copy.
		row := fmt.Sprintf("%s%s%s  %s  %s", cursor, mark, padRight(label, labelW), value, tag)
		out = append(out, pad+row)
		if i == e.cursor && e.err != "" {
			out = append(out, pad+"   "+m.theme.errorText.Render(truncate(e.err, maxInt(1, width-3))))
		}
	}

	// Pad to leave the last two rows for the hint and the footnote.
	for len(out) < m.layout.Body-2 {
		out = append(out, "")
	}
	hint := "↑↓ move · ←→/space change · enter edit · ctrl+s save · esc cancel"
	note := m.theme.system.Render("credentials (api_key_env, headers) are edited in config.yaml")
	if len(out) < m.layout.Body {
		out = append(out, pad+m.theme.footer.Render(truncate(hint, width)))
	}
	if len(out) < m.layout.Body {
		out = append(out, pad+note)
	}
	for len(out) < m.layout.Body {
		out = append(out, "")
	}
	return out[:m.layout.Body]
}
