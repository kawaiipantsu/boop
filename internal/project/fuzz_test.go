package project

import (
	"testing"
)

func FuzzParseBoopMarkdown(f *testing.F) {
	seeds := []string{
		"# Boop Project Memory\n\n## Project\nTest project.\n\n## Goals\n- Goal 1\n",
		"# Custom Title\n\n## Architecture\nOverview text\n",
		"arbitrary text without headings\n",
		"```markdown\n## Fenced Heading\n```\n## Real Heading\n",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data string) {
		// Parsing arbitrary markdown must never panic.
		doc := Parse([]byte(data))
		// Rendering parsed document back must never panic.
		_ = doc.String()
	})
}
