package tools

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// DefaultMaxProcessLogLines is the circular buffer line cap per process.
const DefaultMaxProcessLogLines = 500

// BackgroundProcess tracks a single running command.
type BackgroundProcess struct {
	ID        string
	Command   string
	PID       int
	StartedAt time.Time
	cmd       *exec.Cmd
	mu        sync.RWMutex
	logs      []string
	done      bool
	exitCode  int
}

func (p *BackgroundProcess) appendLog(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logs = append(p.logs, line)
	if len(p.logs) > DefaultMaxProcessLogLines {
		p.logs = p.logs[len(p.logs)-DefaultMaxProcessLogLines:]
	}
}

func (p *BackgroundProcess) getLogs(limit int) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if limit <= 0 || limit > len(p.logs) {
		limit = len(p.logs)
	}
	start := len(p.logs) - limit
	out := make([]string, limit)
	copy(out, p.logs[start:])
	return out
}

// ProcessManager manages background process lifecycles.
type ProcessManager struct {
	ws    *Workspace
	mu    sync.RWMutex
	procs map[string]*BackgroundProcess
	order []string
	seq   int
}

// NewProcessManager creates a manager for ws.
func NewProcessManager(ws *Workspace) *ProcessManager {
	return &ProcessManager{
		ws:    ws,
		procs: make(map[string]*BackgroundProcess),
	}
}

// Start launches a background process in the workspace.
func (pm *ProcessManager) Start(command string, dir string) (*BackgroundProcess, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	workDir := pm.ws.Root()
	if dir != "" {
		resolved, err := pm.ws.Resolve(dir)
		if err != nil {
			return nil, err
		}
		workDir = resolved
	}

	pm.seq++
	id := fmt.Sprintf("bg-%d", pm.seq)

	cmd := exec.Command("sh", "-c", command)
	if os.Getenv("OS") == "Windows_NT" && os.Getenv("SHELL") == "" {
		cmd = exec.Command("powershell", "-NoProfile", "-Command", command)
	}
	cmd.Dir = workDir
	cmd.Env = os.Environ()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	proc := &BackgroundProcess{
		ID:        id,
		Command:   command,
		PID:       cmd.Process.Pid,
		StartedAt: time.Now(),
		cmd:       cmd,
	}

	// Capture stdout & stderr into circular buffer
	go func() {
		reader := io.MultiReader(stdout, stderr)
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			proc.appendLog(scanner.Text())
		}
		_ = cmd.Wait()
		proc.mu.Lock()
		proc.done = true
		if cmd.ProcessState != nil {
			proc.exitCode = cmd.ProcessState.ExitCode()
		}
		proc.mu.Unlock()
	}()

	pm.procs[id] = proc
	pm.order = append(pm.order, id)
	return proc, nil
}

// Logs returns recent output from the process.
func (pm *ProcessManager) Logs(id string, lines int) ([]string, bool, error) {
	pm.mu.RLock()
	proc, ok := pm.procs[id]
	pm.mu.RUnlock()

	if !ok {
		return nil, false, fmt.Errorf("background process %q not found", id)
	}

	proc.mu.RLock()
	done := proc.done
	proc.mu.RUnlock()

	return proc.getLogs(lines), done, nil
}

// Stop terminates a background process.
func (pm *ProcessManager) Stop(id string) error {
	pm.mu.RLock()
	proc, ok := pm.procs[id]
	pm.mu.RUnlock()

	if !ok {
		return fmt.Errorf("background process %q not found", id)
	}

	proc.mu.Lock()
	defer proc.mu.Unlock()
	if proc.done || proc.cmd == nil || proc.cmd.Process == nil {
		return nil
	}

	_ = proc.cmd.Process.Kill()
	proc.done = true
	return nil
}

// List returns all registered background processes.
func (pm *ProcessManager) List() []*BackgroundProcess {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	out := make([]*BackgroundProcess, 0, len(pm.order))
	for _, id := range pm.order {
		out = append(out, pm.procs[id])
	}
	return out
}

// StopAll reaps all active background processes on session shutdown.
func (pm *ProcessManager) StopAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for _, proc := range pm.procs {
		proc.mu.Lock()
		if !proc.done && proc.cmd != nil && proc.cmd.Process != nil {
			_ = proc.cmd.Process.Kill()
			proc.done = true
		}
		proc.mu.Unlock()
	}
}

// ProcessTool exposes background process control to models.
type ProcessTool struct {
	Manager *ProcessManager
}

// NewProcessTool creates a tool backed by manager.
func NewProcessTool(manager *ProcessManager) *ProcessTool {
	return &ProcessTool{Manager: manager}
}

type processArgs struct {
	Action     string `json:"action"`
	Command    string `json:"command,omitempty"`
	ID         string `json:"id,omitempty"`
	WorkingDir string `json:"working_dir,omitempty"`
	Lines      int    `json:"lines,omitempty"`
}

// Name implements Tool.
func (t *ProcessTool) Name() string { return "process" }

// Description implements Tool.
func (t *ProcessTool) Description() string {
	return "Manage background processes (e.g. dev servers, watchers). Actions: start, logs, stop, list."
}

// Schema implements Tool.
func (t *ProcessTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"start", "logs", "stop", "list"},
				"description": "The process operation to perform.",
			},
			"command": map[string]any{
				"type":        "string",
				"description": "Command line to start (required for start).",
			},
			"id": map[string]any{
				"type":        "string",
				"description": "Process handle ID (e.g. 'bg-1') for logs or stop.",
			},
			"working_dir": map[string]any{
				"type":        "string",
				"description": "Working directory relative to project root.",
			},
			"lines": map[string]any{
				"type":        "integer",
				"default":     100,
				"description": "Number of log lines to return (for logs action).",
			},
		},
		"required":             []string{"action"},
		"additionalProperties": false,
	}
}

// Permission implements Tool.
func (t *ProcessTool) Permission(call Call) (permissions.Action, error) {
	var a processArgs
	if err := call.Bind(&a); err != nil {
		return permissions.Action{}, err
	}
	if a.Action == "start" {
		return permissions.Action{
			Category: permissions.CatShellExecute,
			Risk:     permissions.RiskMedium,
			Tool:     t.Name(),
			Summary:  fmt.Sprintf("Start background process: %s", a.Command),
			Detail:   a.Command,
		}, nil
	}
	return permissions.Action{
		Category: permissions.CatFilesystemRead,
		Risk:     permissions.RiskLow,
		Tool:     t.Name(),
		Summary:  fmt.Sprintf("Process %s", a.Action),
	}, nil
}

// Execute performs the process management action.
func (t *ProcessTool) Execute(ctx context.Context, call Call) (Result, error) {
	var a processArgs
	if err := call.Bind(&a); err != nil {
		return Errorf(call, "process: %v", err), nil
	}

	switch strings.ToLower(a.Action) {
	case "start":
		if strings.TrimSpace(a.Command) == "" {
			return Errorf(call, "process start: command is required"), nil
		}
		proc, err := t.Manager.Start(a.Command, a.WorkingDir)
		if err != nil {
			return Errorf(call, "process start: %v", err), nil
		}
		return Result{
			CallID:  call.ID,
			Tool:    t.Name(),
			Content: fmt.Sprintf("Started background process %s (PID %d): %s", proc.ID, proc.PID, proc.Command),
			Display: fmt.Sprintf("%s started (PID %d)", proc.ID, proc.PID),
		}, nil

	case "logs":
		if a.ID == "" {
			return Errorf(call, "process logs: id is required (e.g. 'bg-1')"), nil
		}
		lines := a.Lines
		if lines <= 0 {
			lines = 100
		}
		logLines, done, err := t.Manager.Logs(a.ID, lines)
		if err != nil {
			return Errorf(call, "process logs: %v", err), nil
		}
		status := "running"
		if done {
			status = "exited"
		}
		content := strings.Join(logLines, "\n")
		if content == "" {
			content = "(no output recorded yet)"
		}
		return Result{
			CallID:  call.ID,
			Tool:    t.Name(),
			Content: fmt.Sprintf("[%s status: %s]\n%s", a.ID, status, content),
			Display: fmt.Sprintf("%s logs (%d lines)", a.ID, len(logLines)),
		}, nil

	case "stop":
		if a.ID == "" {
			return Errorf(call, "process stop: id is required (e.g. 'bg-1')"), nil
		}
		if err := t.Manager.Stop(a.ID); err != nil {
			return Errorf(call, "process stop: %v", err), nil
		}
		return Result{
			CallID:  call.ID,
			Tool:    t.Name(),
			Content: fmt.Sprintf("Stopped background process %s", a.ID),
			Display: fmt.Sprintf("%s stopped", a.ID),
		}, nil

	case "list":
		list := t.Manager.List()
		if len(list) == 0 {
			return Result{
				CallID:  call.ID,
				Tool:    t.Name(),
				Content: "No background processes active.",
				Display: "no background processes",
			}, nil
		}
		var sb strings.Builder
		sb.WriteString("Background Processes:\n")
		for _, p := range list {
			p.mu.RLock()
			status := "running"
			if p.done {
				status = fmt.Sprintf("exited (%d)", p.exitCode)
			}
			fmt.Fprintf(&sb, "- %s (PID %d) [%s]: %s\n", p.ID, p.PID, status, p.Command)
			p.mu.RUnlock()
		}
		return Result{
			CallID:  call.ID,
			Tool:    t.Name(),
			Content: strings.TrimSpace(sb.String()),
			Display: fmt.Sprintf("%d background processes", len(list)),
		}, nil

	default:
		return Errorf(call, "unknown action %q; use start, logs, stop, or list", a.Action), nil
	}
}
