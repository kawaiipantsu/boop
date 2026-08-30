package tui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kawaiipantsu/boop/internal/app"
	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/provider"
)

// newTestApp assembles a real runtime backed by an in-memory store. It needs
// no network: no turn is ever run against it.
func newTestApp(t *testing.T, mutate func(*config.Config)) (*app.App, *Approver) {
	t.Helper()
	cfg := config.Default()
	if mutate != nil {
		mutate(cfg)
	}
	broker := permissions.NewBroker()
	approver := NewApprover(broker)
	application, err := app.New(context.Background(), app.Options{
		Config:       cfg,
		WorkingDir:   t.TempDir(),
		Approver:     approver,
		DatabasePath: ":memory:",
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() {
		broker.Close()
		_ = application.Close()
	})
	approver.SetEvaluator(application.Evaluator)
	return application, approver
}

func TestRunRequiresAConfiguration(t *testing.T) {
	if err := Run(context.Background(), Options{}); err == nil {
		t.Fatal("Run without a configuration should fail")
	}
}

func TestOpenSessionStartsFresh(t *testing.T) {
	application, _ := newTestApp(t, nil)
	sess, history, resumed, err := openSession(context.Background(), application, "")
	if err != nil {
		t.Fatalf("openSession: %v", err)
	}
	if resumed {
		t.Fatal("a new session should not report as resumed")
	}
	if sess.ID == "" {
		t.Fatal("no session id")
	}
	if len(history) != 1 || history[0].Role != provider.RoleSystem {
		t.Fatalf("history = %+v, want a single system message", history)
	}
	if !strings.Contains(history[0].Content, application.Workspace.Root()) {
		t.Fatal("the system prompt does not mention the working directory")
	}
}

func TestOpenSessionResumesStoredMessages(t *testing.T) {
	application, _ := newTestApp(t, nil)
	ctx := context.Background()

	first, _, _, err := openSession(ctx, application, "")
	if err != nil {
		t.Fatalf("openSession: %v", err)
	}
	if _, err := application.Sessions.AppendMessage(ctx, first.ID,
		provider.Message{Role: provider.RoleUser, Content: "earlier question"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	resumedSession, history, resumed, err := openSession(ctx, application, first.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !resumed || resumedSession.ID != first.ID {
		t.Fatalf("resumed = %v, id = %q", resumed, resumedSession.ID)
	}
	if len(history) != 2 || history[1].Content != "earlier question" {
		t.Fatalf("history = %+v", history)
	}
}

func TestOpenSessionReportsAMissingSession(t *testing.T) {
	application, _ := newTestApp(t, nil)
	if _, _, _, err := openSession(context.Background(), application, "does-not-exist"); err == nil {
		t.Fatal("resuming an unknown session should fail")
	}
}

func TestBuildSystemPromptCarriesTheRuntimeContext(t *testing.T) {
	application, _ := newTestApp(t, nil)
	prompt := buildSystemPrompt(application)
	for _, want := range []string{application.Config().Provider, application.Workspace.Root(), "run"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("system prompt is missing %q", want)
		}
	}
}

func TestGreetAnnouncesTheRiskySettings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{
			name:   "outbound web access",
			mutate: func(c *config.Config) { c.Network.Enabled = true },
			want:   "outbound web access is ON",
		},
		{
			name:   "unrestricted",
			mutate: func(c *config.Config) { c.Execution.Unrestricted = true },
			want:   "running UNRESTRICTED",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			application, approver := newTestApp(t, tc.mutate)
			m := newModel(context.Background(), application, approver, "s", "", "", nil)
			m.greet(application, false, false)
			if got := renderText(m.transcript, 100); !strings.Contains(got, tc.want) {
				t.Fatalf("greeting is missing %q:\n%s", tc.want, got)
			}
		})
	}
}

func TestGreetReplaysAResumedTranscript(t *testing.T) {
	application, approver := newTestApp(t, nil)
	history := []provider.Message{
		{Role: provider.RoleSystem, Content: "sys"},
		{Role: provider.RoleUser, Content: "an earlier question"},
		{Role: provider.RoleAssistant, Content: "an earlier answer"},
	}
	m := newModel(context.Background(), application, approver, "s", "", "", history)
	m.greet(application, true, false)

	got := renderText(m.transcript, 100)
	for _, want := range []string{"resumed session", "an earlier question", "an earlier answer"} {
		if !strings.Contains(got, want) {
			t.Errorf("replay is missing %q:\n%s", want, got)
		}
	}
}

func TestModelReportsAgainstARealRuntime(t *testing.T) {
	application, approver := newTestApp(t, func(c *config.Config) { c.Provider = "ollama" })
	m := newModel(context.Background(), application, approver, "session-9", "", "", nil)
	send(m, tea.WindowSizeMsg{Width: 100, Height: 30})

	tests := []struct {
		cmd  string
		want []string
	}{
		{"/status", []string{"boop", "provider", "tools", "web access"}},
		{"/permissions", []string{"permission policy", "shell.execute"}},
		{"/session", []string{"session session-9"}},
		{"/provider", []string{"configured:"}},
		{"/model", []string{"model"}},
	}
	for _, tc := range tests {
		m.transcript.Clear()
		typeText(m, tc.cmd)
		send(m, key("enter"))
		got := renderText(m.transcript, 100)
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("%s is missing %q:\n%s", tc.cmd, want, got)
			}
		}
	}
}

func TestProviderCommandRejectsAnUnknownName(t *testing.T) {
	application, approver := newTestApp(t, nil)
	m := newModel(context.Background(), application, approver, "s", "", "", nil)
	send(m, tea.WindowSizeMsg{Width: 100, Height: 30})

	before := application.Config().Provider
	typeText(m, "/provider nonesuch")
	send(m, key("enter"))
	if application.Config().Provider != before {
		t.Fatalf("an unknown provider was accepted: %q", application.Config().Provider)
	}
	if !strings.Contains(renderText(m.transcript, 100), "no provider named") {
		t.Fatalf("transcript = %s", renderText(m.transcript, 100))
	}
}

func TestPermissionsCommandSwitchesMode(t *testing.T) {
	application, approver := newTestApp(t, nil)
	m := newModel(context.Background(), application, approver, "s", "", "", nil)
	send(m, tea.WindowSizeMsg{Width: 100, Height: 30})

	typeText(m, "/permissions mode auto")
	send(m, key("enter"))
	if application.Evaluator.Policy().Mode != permissions.ModeAuto {
		t.Fatalf("mode = %q", application.Evaluator.Policy().Mode)
	}
	if application.Config().Execution.Mode != permissions.ModeAuto {
		t.Fatalf("config mode = %q", application.Config().Execution.Mode)
	}

	m.transcript.Clear()
	typeText(m, "/permissions mode sideways")
	send(m, key("enter"))
	if !strings.Contains(renderText(m.transcript, 100), "confirm|auto") {
		t.Fatal("an invalid mode was not rejected with usage")
	}
}

func TestPermissionsCommandClearsSessionGrants(t *testing.T) {
	application, approver := newTestApp(t, nil)
	broker := approver.Broker()
	if !broker.AllowCategoryForSession(permissions.CatFilesystemRead) {
		t.Fatal("could not create a grant to clear")
	}
	m := newModel(context.Background(), application, approver, "s", "", "", nil)
	send(m, tea.WindowSizeMsg{Width: 100, Height: 30})

	typeText(m, "/permissions")
	send(m, key("enter"))
	if !strings.Contains(renderText(m.transcript, 100), "standing session grants") {
		t.Fatal("standing grants are not shown, so they cannot be revoked")
	}

	typeText(m, "/permissions clear")
	send(m, key("enter"))
	if len(broker.SessionGrants()) != 0 {
		t.Fatal("/permissions clear left grants behind")
	}
}

func TestWaitForTurnsReturnsWhenWorkFinishes(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		time.Sleep(10 * time.Millisecond)
		wg.Done()
	}()
	start := time.Now()
	waitForTurns(&wg, time.Second)
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("waitForTurns did not notice the work finishing")
	}
}

func TestWaitForTurnsGivesUpAfterTheGrace(t *testing.T) {
	// A wedged tool must not stop Boop exiting (§58).
	var wg sync.WaitGroup
	wg.Add(1)
	defer wg.Done()

	start := time.Now()
	waitForTurns(&wg, 20*time.Millisecond)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waitForTurns blocked for %v", elapsed)
	}
	waitForTurns(nil, time.Millisecond)
}

// TestProgramLoopRunsAndQuits drives the model through a real Bubble Tea
// program over pipes rather than a terminal. It is the only test that proves
// Init, Update and View survive contact with the runtime loop; everything else
// exercises Update directly.
func TestProgramLoopRunsAndQuits(t *testing.T) {
	application, approver := newTestApp(t, nil)
	m := newModel(context.Background(), application, approver, "s", "", "", nil)
	m.greet(application, false, false)

	input, _ := io.Pipe()
	var output bytes.Buffer
	program := tea.NewProgram(m,
		tea.WithInput(input),
		tea.WithOutput(&output),
		tea.WithoutSignalHandler(),
	)
	m.pump.setSend(program.Send)

	done := make(chan error, 1)
	go func() {
		_, err := program.Run()
		done <- err
	}()

	// Drive it the way the runtime would: a resize, a streamed token, then a
	// quit command submitted through the composer.
	program.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.pump.push(uiEvent{kind: evToken, text: "streamed"})
	for _, r := range "/quit" {
		program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	program.Send(tea.KeyMsg{Type: tea.KeyEnter})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("program.Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		program.Kill()
		t.Fatal("the program never quit")
	}

	if !m.quitting {
		t.Fatal("the model did not record the quit")
	}
	if !strings.Contains(renderText(m.transcript, 100), "streamed") {
		t.Fatal("the streamed token never reached the transcript")
	}
	if output.Len() == 0 {
		t.Fatal("the program rendered nothing")
	}
}
