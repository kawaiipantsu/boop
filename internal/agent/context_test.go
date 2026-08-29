package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/project"
	"github.com/kawaiipantsu/boop/internal/provider"
)

func TestRestrictTools(t *testing.T) {
	full, _ := fullRegistry("read", "write", "run", "search")

	tests := []struct {
		name    string
		allowed []string
		want    []string
	}{
		{"nil grants nothing", nil, nil},
		{"empty grants nothing", []string{}, nil},
		{"single tool", []string{"read"}, []string{"read"}},
		{"several tools", []string{"read", "search"}, []string{"read", "search"}},
		{"unknown names ignored", []string{"read", "teleport"}, []string{"read"}},
		{"blank names ignored", []string{" ", "read"}, []string{"read"}},
		{"everything", []string{"read", "write", "run", "search"}, []string{"read", "run", "search", "write"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			restricted := RestrictTools(full, tc.allowed)

			got := restricted.Names()
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("Names() = %v, want %v", got, tc.want)
			}
			// The definitions offered to the model must match exactly.
			defs := restricted.Definitions(nil)
			if len(defs) != len(tc.want) {
				t.Errorf("Definitions() has %d entries, want %d", len(defs), len(tc.want))
			}
			// And dispatch must be impossible for anything not granted.
			for _, name := range []string{"read", "write", "run", "search"} {
				_, ok := restricted.Get(name)
				wantOK := false
				for _, allowed := range tc.want {
					if allowed == name {
						wantOK = true
					}
				}
				if ok != wantOK {
					t.Errorf("Get(%q) present = %v, want %v", name, ok, wantOK)
				}
			}
		})
	}

	if got := RestrictTools(nil, []string{"read"}).Names(); len(got) != 0 {
		t.Errorf("RestrictTools(nil, ...) = %v, want an empty registry", got)
	}
	// The source registry must be untouched.
	if len(full.Names()) != 4 {
		t.Errorf("source registry now has %v, want all four tools", full.Names())
	}
}

func TestDefaultAllowedTools(t *testing.T) {
	tests := []struct {
		name string
		task Task
		want []string
	}{
		{"read-only task", Task{ID: "a"}, []string{"find", "list", "read", "search"}},
		{
			name: "writing task",
			task: Task{ID: "a", Writes: []string{"internal/x.go"}},
			want: []string{"edit", "find", "list", "read", "search", "write"},
		},
		{
			name: "blank writes do not grant write access",
			task: Task{ID: "a", Writes: []string{"  "}},
			want: []string{"find", "list", "read", "search"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DefaultAllowedTools(tc.task)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("DefaultAllowedTools() = %v, want %v", got, tc.want)
			}
			for _, name := range got {
				if name == "run" || name == "test" || name == "git" {
					t.Errorf("default grant includes %q; command execution must be explicit", name)
				}
			}
		})
	}
}

func TestScopeBriefIsolatesContext(t *testing.T) {
	scope := &Scope{
		Environment: "linux/amd64, workspace /tmp/proj",
		Memory: MemorySourceFunc(func(Task) string {
			return "The router is provider-neutral."
		}),
	}
	task := Task{
		ID:           "implement",
		Description:  "Add the retry path to the router.",
		Reads:        []string{"./internal/provider/router.go"},
		Writes:       []string{"internal/provider/router.go"},
		Requirements: []string{"Keep the Provider interface unchanged."},
		Validation:   "go test ./internal/provider/...",
	}

	brief := scope.Brief(BriefRequest{
		Task:         task,
		Objective:    "Make routing survive a flaky provider.",
		Requirements: []string{"No new dependencies."},
	})

	if brief.Task != task.Description {
		t.Errorf("Task = %q, want the task description", brief.Task)
	}
	if len(brief.Requirements) != 2 {
		t.Errorf("Requirements = %v, want the run's and the task's", brief.Requirements)
	}
	if len(brief.Writes) != 1 || brief.Writes[0] != "internal/provider/router.go" {
		t.Errorf("Writes = %v, want the normalised path", brief.Writes)
	}
	if len(brief.Files) != 1 {
		t.Errorf("Files = %+v, want one entry: read and write name the same file", brief.Files)
	}
	if brief.Memory == "" {
		t.Error("Memory is empty, want the relevant project memory")
	}

	msgs := brief.Messages("")
	if len(msgs) != 2 {
		t.Fatalf("Messages() returned %d messages, want exactly a system prompt and the brief", len(msgs))
	}
	if msgs[0].Role != provider.RoleSystem || msgs[1].Role != provider.RoleUser {
		t.Errorf("roles = %s, %s; want system then user", msgs[0].Role, msgs[1].Role)
	}

	system := msgs[0].Content
	for _, want := range []string{"worker agent", "Allowed tools", "linux/amd64", "edit", "write"} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt is missing %q", want)
		}
	}

	body := msgs[1].Content
	for _, want := range []string{
		"Make routing survive a flaky provider.",
		"Add the retry path to the router.",
		"No new dependencies.",
		"Keep the Provider interface unchanged.",
		"internal/provider/router.go",
		"The router is provider-neutral.",
		"go test ./internal/provider/...",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("brief is missing %q", want)
		}
	}
}

func TestScopeBriefReadOnlyTaskSaysSo(t *testing.T) {
	brief := (&Scope{}).Brief(BriefRequest{Task: Task{ID: "look", Description: "Inspect the layout."}})
	body := brief.Text()
	if !strings.Contains(body, "read-only") {
		t.Errorf("a task with no writes must say so; got:\n%s", body)
	}
	if strings.Join(brief.AllowedTools, ",") != "find,list,read,search" {
		t.Errorf("AllowedTools = %v, want the read-only default", brief.AllowedTools)
	}
}

func TestScopeBriefTaskToolsWin(t *testing.T) {
	scope := &Scope{DefaultTools: []string{"read"}}
	brief := scope.Brief(BriefRequest{Task: Task{ID: "a", AllowedTools: []string{"run", "run", " "}}})
	if strings.Join(brief.AllowedTools, ",") != "run" {
		t.Errorf("AllowedTools = %v, want the task's own de-duplicated grant", brief.AllowedTools)
	}

	brief = scope.Brief(BriefRequest{Task: Task{ID: "a"}})
	if strings.Join(brief.AllowedTools, ",") != "read" {
		t.Errorf("AllowedTools = %v, want the scope default", brief.AllowedTools)
	}
}

func TestScopeBriefTruncatesMemory(t *testing.T) {
	long := strings.Repeat("a line of durable project knowledge\n", 500)
	scope := &Scope{
		Memory:         MemorySourceFunc(func(Task) string { return long }),
		MaxMemoryBytes: 200,
	}
	brief := scope.Brief(BriefRequest{Task: Task{ID: "a", Description: "d"}})
	if len(brief.Memory) > 260 {
		t.Errorf("memory is %d bytes, want it truncated near the 200-byte limit", len(brief.Memory))
	}
	if !strings.Contains(brief.Memory, "truncated") {
		t.Error("truncated memory must say that it was truncated")
	}
}

func TestScopeBriefInlinesResolvedFiles(t *testing.T) {
	scope := &Scope{
		Files: func(path string) (FileRef, bool) {
			if path != "internal/x.go" {
				return FileRef{}, false
			}
			return FileRef{Path: path, Note: "the file to change", Content: "package x\n"}, true
		},
	}
	brief := scope.Brief(BriefRequest{Task: Task{ID: "a", Description: "d", Writes: []string{"internal/x.go"}, Reads: []string{"other.go"}}})
	body := brief.Text()
	for _, want := range []string{"the file to change", "package x", "other.go"} {
		if !strings.Contains(body, want) {
			t.Errorf("brief is missing %q\n%s", want, body)
		}
	}
}

func TestProjectMemorySelectsDurableSections(t *testing.T) {
	dir := t.TempDir()
	mem, err := project.LoadOrCreate(filepath.Join(dir, "Boop.md"))
	if err != nil {
		t.Fatalf("LoadOrCreate() = %v", err)
	}
	doc := mem.Document()
	doc.SetText(project.SectionArchitecture, "Core is UI independent.")
	doc.SetText(project.SectionSessionSummaries, "Chatted about the weather.")
	doc.SetText(project.SectionGoals, "Ship it.")

	got := ProjectMemory(mem).Relevant(Task{ID: "a"})
	if !strings.Contains(got, "Core is UI independent.") {
		t.Errorf("architecture is missing from:\n%s", got)
	}
	if strings.Contains(got, "Chatted about the weather.") {
		t.Error("session summaries must not be pasted into a worker's brief")
	}
	if strings.Contains(got, "Ship it.") {
		t.Error("goals are conversation-shaped and must not be included")
	}

	if got := ProjectMemory(nil).Relevant(Task{ID: "a"}); got != "" {
		t.Errorf("ProjectMemory(nil) = %q, want empty", got)
	}
}

func TestBriefMessagesWithoutToolsSaysSo(t *testing.T) {
	msgs := Brief{Task: "think"}.Messages("")
	if !strings.Contains(msgs[0].Content, "None.") {
		t.Errorf("a worker with no tools must be told so:\n%s", msgs[0].Content)
	}
}
