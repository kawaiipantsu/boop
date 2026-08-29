package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kawaiipantsu/boop/internal/project"
)

// runPrep implements `boop prep`, the §17 project initialization command.
//
// It was reachable only through a slash command that did not exist yet, so the
// implementation had no caller at all. Exposing it as a subcommand makes it
// usable from a shell and from CI, and needs no model, no provider and no
// session — which is why it does not build the full runtime.
func runPrep(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	dir := ""
	if len(args) > 0 {
		dir = args[0]
	}
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("cannot determine the working directory: %w", err)
		}
		dir = cwd
	}

	report, err := project.Prep(ctx, dir)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "project: %s\n", report.Root)
	if info := report.Info; info != nil {
		if len(info.Languages) > 0 {
			names := make([]string, 0, len(info.Languages))
			for _, l := range info.Languages {
				names = append(names, fmt.Sprintf("%s (%d)", l.Name, l.Files))
			}
			fmt.Fprintf(stdout, "languages: %s\n", strings.Join(names, ", "))
		}
		if len(info.Frameworks) > 0 {
			fmt.Fprintf(stdout, "frameworks: %s\n", strings.Join(info.Frameworks, ", "))
		}
		if info.Git.Present {
			state := info.Git.Branch
			if info.Git.Detached {
				state = "detached at " + shortHead(info.Git.Head)
			}
			if info.Git.DirtyKnown && info.Git.Dirty {
				state += ", uncommitted changes"
			}
			fmt.Fprintf(stdout, "git:     %s\n", state)
		}
		for _, c := range info.Commands {
			note := ""
			if c.Inferred {
				note = "  (inferred)"
			}
			fmt.Fprintf(stdout, "%-8s %s%s\n", string(c.Kind)+":", c.Line, note)
		}
	}

	action := "updated"
	if report.MemoryCreated {
		action = "created"
	}
	fmt.Fprintf(stdout, "\n%s %s\n", action, report.MemoryPath)

	if len(report.Sensitive) > 0 {
		fmt.Fprintf(stdout, "\nproduction-sensitive files (%d):\n", len(report.Sensitive))
		for i, f := range report.Sensitive {
			if i == 12 {
				fmt.Fprintf(stdout, "  ... and %d more\n", len(report.Sensitive)-i)
				break
			}
			fmt.Fprintf(stdout, "  %-10s %s\n", f.Sensitivity, f.Path)
		}
	}
	for _, w := range report.Warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	return nil
}

// shortHead abbreviates a commit hash for display.
func shortHead(head string) string {
	if len(head) > 8 {
		return head[:8]
	}
	return head
}
