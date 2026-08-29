package project

import (
	"context"
	"fmt"
	"strings"
)

// Report is the ready-state summary produced by /prep (§17).
type Report struct {
	// Root is the detected project root.
	Root string
	// Info is the full discovery result.
	Info *Info
	// MemoryPath is the Boop.md that was created or updated.
	MemoryPath string
	// MemoryCreated is true when Boop.md did not exist before.
	MemoryCreated bool
	// Sensitive repeats the production-sensitive files, so callers that only
	// keep the report still see them.
	Sensitive []SensitiveFile
	// Warnings are things the user should know before Boop starts working.
	Warnings []string
}

// Prep runs the §17 project initialization sequence against dir.
//
// It detects the root, inspects languages, build systems, Git state, project
// files and documentation, identifies build/test/lint/format commands, flags
// production-sensitive files, and creates or updates Boop.md.
//
// Prep never modifies source code. The only file it writes is Boop.md, and
// within Boop.md it only rewrites the blocks it owns, so hand-written prose
// survives every run.
func Prep(ctx context.Context, dir string) (*Report, error) {
	return PrepWithOptions(ctx, dir, Options{})
}

// PrepWithOptions is Prep with explicit discovery options.
func PrepWithOptions(ctx context.Context, dir string, opts Options) (*Report, error) {
	info, err := Discover(ctx, dir, opts)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	mem, err := LoadOrCreate(info.Root)
	if err != nil {
		return nil, err
	}
	created := mem.Created()

	doc := mem.Document()
	doc.EnsureCanonical()
	doc.SetManagedText(SectionProject, projectBlock(info))
	doc.SetManagedText(SectionArchitecture, architectureBlock(info))
	doc.SetManagedText(SectionImportantFiles, importantFilesBlock(info))
	doc.SetManagedText(SectionTests, testsBlock(info))
	doc.SetManagedText(SectionUsefulCommands, commandsBlock(info))

	if err := mem.Save(); err != nil {
		return nil, err
	}

	return &Report{
		Root:          info.Root,
		Info:          info,
		MemoryPath:    mem.Path(),
		MemoryCreated: created,
		Sensitive:     info.Sensitive,
		Warnings:      warnings(info),
	}, nil
}

// warnings collects everything worth saying out loud before work starts.
func warnings(info *Info) []string {
	var out []string
	if !info.Git.Present {
		out = append(out, "no Git repository: changes are not tracked and cannot be reverted")
	} else {
		if !info.Git.HasRemote() {
			out = append(out, "no Git remote configured: work is local only")
		}
		if info.Git.DirtyKnown && info.Git.Dirty {
			out = append(out, fmt.Sprintf("working tree has %d uncommitted change(s)", info.Git.DirtyFiles))
		}
		if !info.Git.DirtyKnown {
			out = append(out, "could not determine working-tree state (git binary unavailable)")
		}
	}
	if !info.HasTests() {
		out = append(out, "no test files detected: repair loops cannot be validated by tests")
	}
	if _, ok := info.Command(KindTest); !ok {
		out = append(out, "no test command detected")
	}
	if n := countAtLeast(info.Sensitive, SensitivityHigh); n > 0 {
		out = append(out, fmt.Sprintf("%d production-sensitive file(s) detected: changes there require deliberate intent", n))
	}
	if info.Truncated {
		out = append(out, "project scan was truncated: counts are lower bounds")
	}
	return out
}

func countAtLeast(files []SensitiveFile, min Sensitivity) int {
	n := 0
	for _, f := range files {
		if sensitivityRank(f.Sensitivity) >= sensitivityRank(min) {
			n++
		}
	}
	return n
}

// Summary renders the concise ready-state summary §17 step 12 asks for.
func (r *Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Project: %s\n", r.Info.Name)
	fmt.Fprintf(&b, "Root: %s\n", r.Root)
	if langs := languageLine(r.Info); langs != "" {
		fmt.Fprintf(&b, "Languages: %s\n", langs)
	}
	if len(r.Info.Frameworks) > 0 {
		fmt.Fprintf(&b, "Frameworks: %s\n", strings.Join(r.Info.Frameworks, ", "))
	}
	fmt.Fprintf(&b, "Git: %s\n", gitLine(r.Info.Git))
	for _, kind := range []CommandKind{KindBuild, KindTest, KindLint, KindFormat} {
		if c, ok := r.Info.Command(kind); ok {
			fmt.Fprintf(&b, "%s: %s%s\n", titleCase(string(kind)), c.Line, sourceNote(c))
		}
	}
	if n := len(r.Sensitive); n > 0 {
		fmt.Fprintf(&b, "Production-sensitive files: %d\n", n)
		for i, f := range r.Sensitive {
			if i == 5 {
				fmt.Fprintf(&b, "  … and %d more\n", n-i)
				break
			}
			fmt.Fprintf(&b, "  [%s] %s — %s\n", f.Sensitivity, f.Path, f.Reason)
		}
	}
	action := "updated"
	if r.MemoryCreated {
		action = "created"
	}
	fmt.Fprintf(&b, "Memory: %s %s\n", action, r.MemoryPath)
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "Warning: %s\n", w)
	}
	return b.String()
}

// String returns the ready-state summary.
func (r *Report) String() string { return r.Summary() }

// ---------------------------------------------------------------------------
// Boop.md blocks

func projectBlock(info *Info) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Name: %s\n", info.Name)
	fmt.Fprintf(&b, "Root: %s\n", info.Root)
	fmt.Fprintf(&b, "Languages: %s\n", orNone(languageLine(info)))
	fmt.Fprintf(&b, "Frameworks: %s\n", orNone(strings.Join(info.Frameworks, ", ")))
	fmt.Fprintf(&b, "Git: %s\n", gitLine(info.Git))
	if len(info.Docs) > 0 {
		fmt.Fprintf(&b, "Documentation: %s\n", strings.Join(info.Docs, ", "))
	}
	return b.String()
}

func architectureBlock(info *Info) string {
	var b strings.Builder
	if p := info.PrimaryLanguage(); p != "" {
		fmt.Fprintf(&b, "Primary language: %s (%d files scanned)\n\n", p, info.FileCount)
	} else {
		fmt.Fprintf(&b, "%d files scanned; no dominant programming language detected.\n\n", info.FileCount)
	}
	if len(info.TopLevelDirs) > 0 {
		b.WriteString("Top-level layout:\n\n")
		for i, d := range info.TopLevelDirs {
			if i == 12 {
				fmt.Fprintf(&b, "- … and %d more directories\n", len(info.TopLevelDirs)-i)
				break
			}
			fmt.Fprintf(&b, "- `%s/` — %d file(s)\n", d.Name, d.Files)
		}
	}
	if entries := entryPointsOf(info); len(entries) > 0 {
		b.WriteString("\nEntry points:\n\n")
		for _, e := range entries {
			fmt.Fprintf(&b, "- `%s`\n", e)
		}
	}
	return b.String()
}

func entryPointsOf(info *Info) []string {
	var out []string
	for _, f := range info.ImportantFiles {
		base := f
		if i := strings.LastIndex(f, "/"); i >= 0 {
			base = f[i+1:]
		}
		switch base {
		case "main.go", "main.rs", "main.py", "app.py", "manage.py", "index.js", "index.ts", "index.php", "main.ts", "main.js", "App.tsx", "artisan":
			out = append(out, f)
		}
	}
	return out
}

func importantFilesBlock(info *Info) string {
	var b strings.Builder
	if len(info.ImportantFiles) == 0 {
		b.WriteString("No manifests or entry points detected.\n")
	}
	for _, f := range info.ImportantFiles {
		fmt.Fprintf(&b, "- `%s`\n", f)
	}
	if len(info.Sensitive) > 0 {
		b.WriteString("\n### Production-sensitive\n\n")
		b.WriteString("Changes to these files can affect production; treat them with deliberate intent.\n\n")
		for i, f := range info.Sensitive {
			if i == 30 {
				fmt.Fprintf(&b, "- … and %d more\n", len(info.Sensitive)-i)
				break
			}
			fmt.Fprintf(&b, "- `%s` — %s (%s, %s)\n", f.Path, f.Reason, f.Category, f.Sensitivity)
		}
	}
	return b.String()
}

func testsBlock(info *Info) string {
	var b strings.Builder
	if info.HasTests() {
		fmt.Fprintf(&b, "%d test file(s) detected.\n", info.TestFiles)
	} else {
		b.WriteString("No test files detected.\n")
	}
	cmds := info.CommandsFor(KindTest)
	if len(cmds) == 0 {
		b.WriteString("\nNo test command detected.\n")
		return b.String()
	}
	b.WriteString("\nTest commands:\n\n")
	for _, c := range cmds {
		fmt.Fprintf(&b, "- `%s`%s\n", c.Line, sourceNote(c))
	}
	return b.String()
}

func commandsBlock(info *Info) string {
	var b strings.Builder
	any := false
	for _, kind := range []CommandKind{KindBuild, KindTest, KindLint, KindFormat} {
		cmds := info.CommandsFor(kind)
		if len(cmds) == 0 {
			continue
		}
		any = true
		fmt.Fprintf(&b, "%s:\n\n", titleCase(string(kind)))
		for _, c := range cmds {
			fmt.Fprintf(&b, "- `%s`%s\n", c.Line, sourceNote(c))
		}
		b.WriteString("\n")
	}
	if !any {
		b.WriteString("No build, test, lint or format commands detected.\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// small formatting helpers

func sourceNote(c Command) string {
	switch {
	case c.Inferred && c.Source != "":
		return fmt.Sprintf(" (inferred from %s)", c.Source)
	case c.Inferred:
		return " (inferred)"
	case c.Source != "":
		return fmt.Sprintf(" (from %s)", c.Source)
	default:
		return ""
	}
}

func languageLine(info *Info) string {
	parts := make([]string, 0, len(info.Languages))
	for i, l := range info.Languages {
		if i == 6 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s (%d)", l.Name, l.Files))
	}
	return strings.Join(parts, ", ")
}

func gitLine(g GitInfo) string {
	if !g.Present {
		return "not a repository"
	}
	parts := []string{}
	switch {
	case g.Detached:
		parts = append(parts, "detached HEAD")
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

func orNone(s string) string {
	if s == "" {
		return "none detected"
	}
	return s
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
