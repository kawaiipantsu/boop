package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kawaiipantsu/boop/internal/agent"
	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/provider"
	"github.com/kawaiipantsu/boop/internal/stats"
)

// newAttachedModel returns a sized model over a real runtime in a temporary
// workspace. Update is a pure function of (model, message), so a command is
// driven by dispatching it and feeding the resulting message back in.
func newAttachedModel(t *testing.T, mutate func(*config.Config)) *Model {
	t.Helper()
	application, approver := newTestApp(t, mutate)
	m := newModel(context.Background(), application, approver, "session-1", "", "", []provider.Message{
		{Role: provider.RoleSystem, Content: "you are boop"},
	})
	send(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	return m
}

// runCommand dispatches a slash command and pumps the message it produces back
// through Update, which is what the Bubble Tea runtime would do.
func runCommand(t *testing.T, m *Model, line string) {
	t.Helper()
	cmd, ok := ParseCommand(line)
	if !ok {
		t.Fatalf("%q did not parse as a command", line)
	}
	for teaCmd := m.dispatch(cmd); teaCmd != nil; {
		msg := teaCmd()
		if msg == nil {
			return
		}
		teaCmd = send(m, msg)
	}
}

// TestEveryReadyCommandDispatches guards the gap between the /help table and
// the dispatch switch: a command listed as working must actually be routed.
func TestEveryReadyCommandDispatches(t *testing.T) {
	for _, spec := range commandSpecs {
		if spec.Status != cmdReady {
			continue
		}
		t.Run(spec.Name, func(t *testing.T) {
			m := newTestModel(t)
			m.dispatch(Command{Name: spec.Name})
			if got := transcriptText(m); strings.Contains(got, "is listed but not dispatched") {
				t.Fatalf("/%s is advertised but not dispatched", spec.Name)
			}
		})
	}
}

// TestNoReadyCommandClaimsToBeWaiting keeps /help honest: only /gui is allowed
// to report that it is not built.
func TestNoReadyCommandClaimsToBeWaiting(t *testing.T) {
	for _, spec := range commandSpecs {
		if spec.Status == cmdPending && spec.Name != "gui" {
			t.Errorf("/%s is still pending: %s", spec.Name, spec.Blocker)
		}
	}
	m := newTestModel(t)
	m.dispatch(Command{Name: "gui"})
	if got := transcriptText(m); !strings.Contains(got, "milestone 13") {
		t.Fatalf("/gui should still say the native GUI is not built, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// /prep
// ---------------------------------------------------------------------------

func TestPrepSurveysTheProjectAndWritesBoopMd(t *testing.T) {
	m := newAttachedModel(t, nil)
	root := m.app.Workspace.Root()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/demo\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	runCommand(t, m, "/prep")

	got := transcriptText(m)
	for _, want := range []string{"project survey", "root", "Go", "memory", "Boop.md"} {
		if !strings.Contains(got, want) {
			t.Errorf("prep output is missing %q:\n%s", want, got)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "Boop.md")); err != nil {
		t.Fatalf("prep did not write Boop.md: %v", err)
	}
}

func TestInitIsAnAliasForPrep(t *testing.T) {
	m := newAttachedModel(t, nil)
	runCommand(t, m, "/init")
	if got := transcriptText(m); !strings.Contains(got, "project survey") {
		t.Fatalf("/init did not run a survey:\n%s", got)
	}
}

func TestPrepRejectsADirectoryArgument(t *testing.T) {
	m := newAttachedModel(t, nil)
	runCommand(t, m, "/prep /etc")
	if got := transcriptText(m); !strings.Contains(got, "usage: /prep") {
		t.Fatalf("prep accepted a directory argument:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// /agents
// ---------------------------------------------------------------------------

func TestAgentsReportsDelegationIsOff(t *testing.T) {
	m := newAttachedModel(t, func(c *config.Config) { c.Agents.Enabled = false })
	tests := []string{"/agents", "/agents list", "/agents stop abc"}
	for _, line := range tests {
		t.Run(line, func(t *testing.T) {
			m.transcript.Clear()
			runCommand(t, m, line)
			if got := transcriptText(m); !strings.Contains(got, "delegation is off") {
				t.Fatalf("%s = %q", line, got)
			}
		})
	}
}

func TestAgentsSubcommands(t *testing.T) {
	m := newAttachedModel(t, func(c *config.Config) {
		c.Agents.Enabled = true
		c.Agents.Max = 5
	})

	tests := []struct {
		name string
		line string
		want string
	}{
		{name: "snapshot", line: "/agents", want: "agents on"},
		{name: "empty list", line: "/agents list", want: "no agents have run"},
		{name: "max", line: "/agents max 2", want: "at most 2 agent(s)"},
		{name: "max needs a number", line: "/agents max lots", want: "usage: /agents max"},
		{name: "unknown id", line: "/agents stop nope", want: "cannot stop nope"},
		{name: "stop needs an id", line: "/agents stop", want: "usage: /agents stop"},
		{name: "unknown subcommand", line: "/agents fly", want: "usage: /agents"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m.transcript.Clear()
			runCommand(t, m, tc.line)
			if got := transcriptText(m); !strings.Contains(got, tc.want) {
				t.Fatalf("%s = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
	if got := m.coordinator().Max(); got != 2 {
		t.Fatalf("/agents max 2 left the limit at %d", got)
	}
	if got := m.app.Config().Agents.Max; got != 2 {
		t.Fatalf("/agents max 2 left the configured limit at %d", got)
	}
}

func TestAgentsOffStopsTheFleetAndOnRebuildsIt(t *testing.T) {
	m := newAttachedModel(t, func(c *config.Config) { c.Agents.Enabled = true })

	spawned, err := m.coordinator().Spawn(agent.SpawnSpec{Name: "worker", Task: "do a thing"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if err := spawned.SetStatus(agent.StatusWorking); err != nil {
		t.Fatalf("set status: %v", err)
	}

	m.syncAgentCount()
	if m.agentCount() != 1 {
		t.Fatalf("header reports %d agents, want 1", m.agentCount())
	}
	runCommand(t, m, "/agents off")
	if m.agentCount() != 0 {
		t.Fatalf("header still reports %d agents after /agents off", m.agentCount())
	}
	if m.app.Config().Agents.Enabled {
		t.Fatal("/agents off left agents enabled in configuration")
	}
	if spawned.State() != agent.StatusCancelled {
		t.Fatalf("the running agent was not cancelled, it is %s", spawned.State())
	}

	m.transcript.Clear()
	runCommand(t, m, "/agents on")
	if !m.app.Config().Agents.Enabled {
		t.Fatal("/agents on did not enable agents")
	}
	if m.coordinator() == nil {
		t.Fatal("/agents on did not build a coordinator")
	}
	if got := transcriptText(m); !strings.Contains(got, "agents are on") {
		t.Fatalf("/agents on said %q", got)
	}
}

func TestHeaderShowsTheLiveAgentCount(t *testing.T) {
	m := newAttachedModel(t, func(c *config.Config) { c.Agents.Enabled = true })
	for i := 0; i < 3; i++ {
		a, err := m.coordinator().Spawn(agent.SpawnSpec{Task: "task"})
		if err != nil {
			t.Fatalf("spawn: %v", err)
		}
		if i < 2 {
			if err := a.SetStatus(agent.StatusWorking); err != nil {
				t.Fatalf("set status: %v", err)
			}
		}
	}
	// The clock tick is what refreshes the cached figure.
	send(m, tickMsg(time.Now()))
	if m.agentCount() != 2 {
		t.Fatalf("agentCount = %d, want 2 (the idle one does not occupy a slot)", m.agentCount())
	}
	if !strings.Contains(m.headerPlain(), "agents 2") {
		t.Fatalf("header = %q", m.headerPlain())
	}
}

// ---------------------------------------------------------------------------
// /config
// ---------------------------------------------------------------------------

func TestConfigNamesCredentialsButNeverPrintsThem(t *testing.T) {
	cfg := config.Default()
	cfg.Providers["openai"] = config.ProviderConfig{
		Type:      "openai",
		BaseURL:   "https://api.openai.com/v1",
		APIKeyEnv: "BOOP_TEST_KEY_ENV",
		Headers:   map[string]string{"X-Org": "secret-org-value"},
	}
	cfg.Web.Auth.TokenEnv = "BOOP_TEST_TOKEN_ENV"

	env := map[string]string{"BOOP_TEST_KEY_ENV": "sk-do-not-print-me"}
	lookup := func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}

	got := configText(cfg, lookup)
	for _, want := range []string{
		"$BOOP_TEST_KEY_ENV (set)",
		"$BOOP_TEST_TOKEN_ENV (not set)",
		"values hidden",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("/config output is missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"sk-do-not-print-me", "secret-org-value"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("/config leaked %q:\n%s", forbidden, got)
		}
	}
}

func TestConfigCommandRejectsUnknownField(t *testing.T) {
	m := newAttachedModel(t, nil)
	runCommand(t, m, "/config set provider ollama")
	if got := transcriptText(m); !strings.Contains(got, "usage: /config") {
		t.Fatalf("/config accepted an unknown field: %q", got)
	}
}

func TestConfigModeWritesFileAndMovesTheEvaluator(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BOOP_CONFIG_DIR", dir)
	m := newAttachedModel(t, func(c *config.Config) { c.Execution.Mode = permissions.ModeConfirm })

	runCommand(t, m, "/config mode auto")

	if got := m.app.Config().Execution.Mode; got != permissions.ModeAuto {
		t.Fatalf("running config mode = %q, want auto", got)
	}
	if got := m.app.Evaluator.Policy().Mode; got != permissions.ModeAuto {
		t.Fatalf("evaluator mode = %q, want auto", got)
	}
	disk, err := config.LoadFrom(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if disk.Execution.Mode != permissions.ModeAuto {
		t.Fatalf("persisted mode = %q, want auto", disk.Execution.Mode)
	}
	if txt := transcriptText(m); !strings.Contains(txt, "saved to") {
		t.Fatalf("no save confirmation in:\n%s", txt)
	}
}

func TestConfigModeRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BOOP_CONFIG_DIR", dir)
	m := newAttachedModel(t, nil)
	runCommand(t, m, "/config mode sideways")
	if got := transcriptText(m); !strings.Contains(got, "usage: /config mode") {
		t.Fatalf("expected usage, got:\n%s", got)
	}
}

func TestConfigWebPortPersists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BOOP_CONFIG_DIR", dir)
	m := newAttachedModel(t, nil)

	runCommand(t, m, "/config web port 8585")

	if m.app.Config().Web.Port != 8585 {
		t.Fatalf("running web.port = %d, want 8585", m.app.Config().Web.Port)
	}
	disk, err := config.LoadFrom(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if disk.Web.Port != 8585 {
		t.Fatalf("persisted web.port = %d, want 8585", disk.Web.Port)
	}
}

func TestConfigWebPortRejectsOutOfRange(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BOOP_CONFIG_DIR", dir)
	m := newAttachedModel(t, nil)
	runCommand(t, m, "/config web port 70000")
	if got := transcriptText(m); !strings.Contains(got, "usage: /config web port") {
		t.Fatalf("expected usage, got:\n%s", got)
	}
}

func TestConfigAgentsOnPersistsAndMovesTheFleet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BOOP_CONFIG_DIR", dir)
	m := newAttachedModel(t, func(c *config.Config) { c.Agents.Enabled = false })

	runCommand(t, m, "/config agents on")

	if !m.app.Config().Agents.Enabled {
		t.Fatal("running config still has agents disabled")
	}
	disk, err := config.LoadFrom(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !disk.Agents.Enabled {
		t.Fatal("persisted config still has agents disabled")
	}
}

// ---------------------------------------------------------------------------
// tool-backed commands
// ---------------------------------------------------------------------------

// allowEverything makes the policy auto-approve, so a test can exercise a tool
// without an operator to answer the prompt.
func allowEverything(c *config.Config) {
	c.Execution.Mode = permissions.ModeAuto
	c.Permissions.Filesystem.Read = permissions.RuleAllow
	c.Permissions.Shell.Execute = permissions.RuleAllow
}

func TestListingCommandsRunThroughTheListTool(t *testing.T) {
	m := newAttachedModel(t, allowEverything)
	root := m.app.Workspace.Root()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "thing.go"), []byte("package pkg\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	tests := []struct {
		name string
		line string
		want string
	}{
		{name: "files", line: "/files", want: "pkg"},
		{name: "tree", line: "/tree", want: "thing.go"},
		{name: "tree with depth", line: "/tree . 2", want: "thing.go"},
		{name: "bad depth", line: "/tree . deep", want: "usage: /tree"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m.transcript.Clear()
			runCommand(t, m, tc.line)
			if got := transcriptText(m); !strings.Contains(got, tc.want) {
				t.Fatalf("%s = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestSearchRunsThroughTheSearchTool(t *testing.T) {
	m := newAttachedModel(t, allowEverything)
	root := m.app.Workspace.Root()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("alpha\nneedle here\nomega\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	runCommand(t, m, "/search needle")
	if got := transcriptText(m); !strings.Contains(got, "notes.txt") {
		t.Fatalf("/search found nothing:\n%s", got)
	}

	m.transcript.Clear()
	runCommand(t, m, "/search")
	if got := transcriptText(m); !strings.Contains(got, "usage: /search") {
		t.Fatalf("/search without a pattern = %q", got)
	}
}

func TestRunGoesThroughThePermissionEngine(t *testing.T) {
	m := newAttachedModel(t, func(c *config.Config) {
		c.Execution.Mode = permissions.ModeAuto
		// Both categories are denied because the classifier files a
		// read-only command such as echo under filesystem.read, not
		// shell.execute; the subject here is that the verdict is obeyed.
		c.Permissions.Shell.Execute = permissions.RuleDeny
		c.Permissions.Filesystem.Read = permissions.RuleDeny
	})
	runCommand(t, m, "/run echo hello")
	got := transcriptText(m)
	if !strings.Contains(got, "denied by policy") {
		t.Fatalf("a denied command should say so, got:\n%s", got)
	}
	if strings.Contains(got, "hello\n") {
		t.Fatal("the command ran despite being denied")
	}
	if m.turnActive {
		t.Fatal("the model is still marked busy after a denied command")
	}
	if m.stats.ToolFailures != 1 {
		t.Fatalf("tool failures = %d, want 1", m.stats.ToolFailures)
	}
}

func TestRunExecutesAnApprovedCommand(t *testing.T) {
	m := newAttachedModel(t, allowEverything)
	runCommand(t, m, "/run echo boop-was-here")
	got := transcriptText(m)
	if !strings.Contains(got, "boop-was-here") {
		t.Fatalf("/run produced no output:\n%s", got)
	}
	if m.status != StatusIdle {
		t.Fatalf("status = %s after a successful command", m.status)
	}
}

func TestToolCommandsRefuseToOverlap(t *testing.T) {
	m := newAttachedModel(t, allowEverything)
	m.turnActive = true
	if cmd := m.dispatch(Command{Name: "run", Args: []string{"echo"}, Rest: "echo hi"}); cmd != nil {
		t.Fatal("a second command should not start while one is running")
	}
	if !strings.Contains(m.notice, "already running") {
		t.Fatalf("notice = %q", m.notice)
	}
}

// TestRunAsksForApprovalInConfirmMode drives the seam the whole design turns
// on: the tool call parks in the broker on its own goroutine, the UI answers
// on the update goroutine, and the refusal comes back as a result rather than
// an error.
func TestRunAsksForApprovalInConfirmMode(t *testing.T) {
	m := newAttachedModel(t, func(c *config.Config) {
		c.Execution.Mode = permissions.ModeConfirm
		c.Permissions.Shell.Execute = permissions.RuleConfirm
		c.Permissions.Filesystem.Read = permissions.RuleConfirm
	})

	teaCmd := m.dispatch(Command{Name: "run", Args: []string{"echo", "hi"}, Rest: "echo hi"})
	if teaCmd == nil {
		t.Fatal("/run produced no command")
	}
	if !m.turnActive {
		t.Fatal("/run did not mark the UI busy")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- teaCmd() }()

	broker := m.broker()
	pending := waitForApproval(t, broker)
	if pending.Action.Tool != "run" {
		t.Fatalf("the pending approval is for %q", pending.Action.Tool)
	}
	if err := broker.Resolve(pending.ID, false); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	select {
	case msg := <-done:
		send(m, msg)
	case <-time.After(5 * time.Second):
		t.Fatal("the refused call never returned")
	}

	got := transcriptText(m)
	if !strings.Contains(got, "you refused this action") {
		t.Fatalf("a refusal should be reported, got:\n%s", got)
	}
	if m.turnActive {
		t.Fatal("the UI is still busy after a refusal")
	}
}

// waitForApproval blocks until the broker has a queued request.
func waitForApproval(t *testing.T, broker *permissions.Broker) permissions.PendingApproval {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pending := broker.Pending(); len(pending) > 0 {
			return pending[0]
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no approval was ever requested")
	return permissions.PendingApproval{}
}

// TestTestAndBuildReportAnOutcome pins the contract that matters most for a
// command that shells out: whatever happens, something is said about it.
func TestTestAndBuildReportAnOutcome(t *testing.T) {
	for _, name := range []string{"test", "build"} {
		t.Run(name, func(t *testing.T) {
			m := newAttachedModel(t, allowEverything)
			runCommand(t, m, "/"+name)
			got := transcriptText(m)
			if strings.TrimSpace(got) == "" {
				t.Fatalf("/%s said nothing at all", name)
			}
			if !strings.Contains(got, name) {
				t.Fatalf("/%s did not name the tool it ran:\n%s", name, got)
			}
			if m.turnActive {
				t.Fatalf("/%s left the UI busy", name)
			}
		})
	}
}

func TestHelpListsEveryImplementedCommand(t *testing.T) {
	help := helpText()
	for _, spec := range commandSpecs {
		if !strings.Contains(help, commandUsage(spec)) {
			t.Errorf("/help does not list %q", commandUsage(spec))
		}
	}
	if !strings.Contains(help, "/gui") {
		t.Error("/help should still list /gui as unbuilt")
	}
}

func TestRunWithoutACommandExplainsItself(t *testing.T) {
	m := newAttachedModel(t, allowEverything)
	runCommand(t, m, "/run")
	if got := transcriptText(m); !strings.Contains(got, "usage: /run") {
		t.Fatalf("/run = %q", got)
	}
}

// ---------------------------------------------------------------------------
// /context
// ---------------------------------------------------------------------------

func TestContextAddSelectsAFileAndSendsIt(t *testing.T) {
	m := newAttachedModel(t, nil)
	root := m.app.Workspace.Root()
	if err := os.WriteFile(filepath.Join(root, "pinned.go"), []byte("package pinned\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	runCommand(t, m, "/context add pinned.go")
	if got := transcriptText(m); !strings.Contains(got, "added pinned.go") {
		t.Fatalf("/context add = %q", got)
	}
	if files := m.selection.Files(); len(files) != 1 || files[0].Path != "pinned.go" {
		t.Fatalf("selection = %+v", files)
	}

	m.history = append(m.history, provider.Message{Role: provider.RoleUser, Content: "what is this?"})
	history := m.requestHistory()
	if n := len(history); n != len(m.history)+1 {
		t.Fatalf("requestHistory returned %d messages, want %d", n, len(m.history)+1)
	}
	if last := history[len(history)-1]; last.Role != provider.RoleUser {
		t.Fatalf("the newest turn is no longer last: %s", last.Role)
	}
	block := history[len(history)-2]
	if block.Role != provider.RoleSystem || !strings.Contains(block.Content, "package pinned") {
		t.Fatalf("the selection was not attached: %+v", block)
	}
	if len(m.history) != 2 {
		t.Fatalf("the selection leaked into the stored conversation: %+v", m.history)
	}

	m.transcript.Clear()
	runCommand(t, m, "/context")
	if got := transcriptText(m); !strings.Contains(got, "selected files (1)") {
		t.Fatalf("/context = %q", got)
	}

	m.transcript.Clear()
	runCommand(t, m, "/context clear")
	if !m.selection.IsEmpty() {
		t.Fatal("/context clear left the selection in place")
	}
	if got := len(m.requestHistory()); got != len(m.history) {
		t.Fatalf("requestHistory still carries a selection: %d messages", got)
	}
}

func TestContextAddRefusesWhatItCannotSend(t *testing.T) {
	m := newAttachedModel(t, nil)
	root := m.app.Workspace.Root()
	if err := os.MkdirAll(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	big := filepath.Join(root, "big.bin")
	if err := os.WriteFile(big, make([]byte, maxSelectedFileBytes+1), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	tests := []struct {
		name string
		line string
		want string
	}{
		{name: "no path", line: "/context add", want: "usage: /context add"},
		{name: "missing file", line: "/context add nope.txt", want: "cannot add nope.txt"},
		{name: "outside the workspace", line: "/context add ../../etc/hosts", want: "cannot add"},
		{name: "a directory", line: "/context add adir", want: "is a directory"},
		{name: "too large", line: "/context add big.bin", want: "selection limit"},
		{name: "unknown subcommand", line: "/context wat", want: "usage: /context"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m.transcript.Clear()
			runCommand(t, m, tc.line)
			if got := transcriptText(m); !strings.Contains(got, tc.want) {
				t.Fatalf("%s = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
	if !m.selection.IsEmpty() {
		t.Fatal("a rejected /context add still selected something")
	}
}

// ---------------------------------------------------------------------------
// /reset
// ---------------------------------------------------------------------------

func TestResetStartsAFreshSession(t *testing.T) {
	m := newAttachedModel(t, nil)
	root := m.app.Workspace.Root()
	if err := os.WriteFile(filepath.Join(root, "pinned.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runCommand(t, m, "/context add pinned.txt")

	m.history = append(m.history, provider.Message{Role: provider.RoleUser, Content: "earlier"})
	m.stats.Turns = 3
	before := m.sessionID

	runCommand(t, m, "/reset")

	if m.sessionID == before {
		t.Fatal("/reset kept the old session")
	}
	if len(m.history) != 1 || m.history[0].Role != provider.RoleSystem {
		t.Fatalf("/reset left the conversation as %+v", m.history)
	}
	if !m.selection.IsEmpty() {
		t.Fatal("/reset kept the selected context")
	}
	if m.stats.Turns != 0 {
		t.Fatalf("/reset left %d turns on the counter", m.stats.Turns)
	}
}

func TestClearOnlyEmptiesTheView(t *testing.T) {
	m := newTestModel(t)
	m.history = append(m.history, provider.Message{Role: provider.RoleUser, Content: "remember this"})
	m.dispatch(Command{Name: "clear"})
	if transcriptText(m) != "" {
		t.Fatal("/clear left rows in the transcript")
	}
	if len(m.history) != 2 {
		t.Fatalf("/clear forgot the conversation: %+v", m.history)
	}
}

// ---------------------------------------------------------------------------
// /web
// ---------------------------------------------------------------------------

func TestWebExplainsHowToStartTheWebUI(t *testing.T) {
	m := newAttachedModel(t, func(c *config.Config) {
		c.Web.Listen = "127.0.0.1"
		c.Web.Port = 8100
	})
	tests := []struct {
		name string
		line string
		want string
	}{
		{name: "status", line: "/web", want: "boop --web"},
		{name: "on", line: "/web on", want: "boop --web"},
		{name: "off", line: "/web off", want: "boop --web"},
		{name: "address", line: "/web", want: "http://127.0.0.1:8100"},
		{name: "unknown", line: "/web sideways", want: "usage: /web"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m.transcript.Clear()
			runCommand(t, m, tc.line)
			if got := transcriptText(m); !strings.Contains(got, tc.want) {
				t.Fatalf("%s = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// /stats and /tokens
// ---------------------------------------------------------------------------

func TestTrackerTextSaysWhenNothingIsRecorded(t *testing.T) {
	got := trackerText(stats.New().Snapshot(), "session-1")
	if !strings.Contains(got, "nothing has been recorded") {
		t.Fatalf("an empty tracker should say so, got:\n%s", got)
	}
}

func TestTrackerTextRendersRecordedWork(t *testing.T) {
	tracker := stats.New()
	tracker.RecordModelCall(stats.ModelCall{
		Scope:    stats.Scope{SessionID: "session-1", Provider: "ollama", Model: "qwen"},
		Usage:    provider.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
		Duration: 2 * time.Second,
	})
	tracker.RecordToolCall(stats.ToolInvocation{
		Scope: stats.Scope{SessionID: "session-1"}, Tool: "run", Duration: time.Second,
	})

	got := trackerText(tracker.Snapshot(), "session-1")
	for _, want := range []string{"this session", "model calls      1", "tool calls       1", "100 prompt"} {
		if !strings.Contains(got, want) {
			t.Errorf("tracker output is missing %q:\n%s", want, got)
		}
	}
}

func TestStatsIncludesTheSharedTracker(t *testing.T) {
	m := newAttachedModel(t, nil)
	runCommand(t, m, "/stats")
	got := transcriptText(m)
	for _, want := range []string{"session statistics", "runtime statistics"} {
		if !strings.Contains(got, want) {
			t.Errorf("/stats is missing %q:\n%s", want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// /status
// ---------------------------------------------------------------------------

func TestStatusReportsTheLiveAgentAndWebState(t *testing.T) {
	m := newAttachedModel(t, func(c *config.Config) { c.Agents.Enabled = true })
	runCommand(t, m, "/status")
	got := transcriptText(m)
	if !strings.Contains(got, "enabled, none started") {
		t.Errorf("/status does not report the fleet:\n%s", got)
	}
	if !strings.Contains(got, "not served from this process") {
		t.Errorf("/status does not report the WebUI:\n%s", got)
	}
}

// assertNoRuntime keeps the "explain the failure" contract: with no runtime
// attached, every command that needs one must say so rather than do nothing.
func TestCommandsWithoutARuntimeExplainThemselves(t *testing.T) {
	lines := []string{
		"/prep", "/config", "/agents", "/run echo hi", "/test", "/build",
		"/files", "/tree", "/search x", "/context add x",
	}
	for _, line := range lines {
		t.Run(line, func(t *testing.T) {
			m := newTestModel(t)
			runCommand(t, m, line)
			got := transcriptText(m)
			if strings.TrimSpace(got) == "" {
				t.Fatalf("%s failed silently", line)
			}
			if !strings.Contains(got, "no runtime") {
				t.Fatalf("%s = %q, want an explanation", line, got)
			}
		})
	}
}
