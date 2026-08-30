package project

import "testing"

// FuzzParseRenderRoundTrip exercises the property Render documents: for a
// document that has not been mutated, Render(Parse(b)) == b. Losing a byte of a
// hand-edited Boop.md on a round trip is the failure §16 forbids, and a text
// scanner over user input is exactly what fuzzing is good at breaking.
func FuzzParseRenderRoundTrip(f *testing.F) {
	seeds := []string{
		"",
		"just some text\nwith no headings at all\n",
		handEdited,
		"## Goals\n\n- one thing",
		"# Boop Project Memory\r\n\r\n## Project\r\n\r\nName: x\r\n",
		"## Goals\n\na\n\n## Goals\n\nb\n",
		"##Goals\n\nnot a section heading\n",
		"## Notes\n\n    ## indented code\n\ntext\n",
		"## Notes\n\n~~~\n## fenced\n~~~\n\ntail\n",
		"## Goals   \n\n- item with trailing space   \n\n\n",
		"## A\n```\n## still fenced\n````\n## out\n",
		NewDocument().String(),
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		got := Parse(data).Render()
		if string(got) != string(data) {
			t.Fatalf("round trip changed the document.\n--- in  ---\n%q\n--- out ---\n%q", data, got)
		}

		// A second round trip must be a no-op too: whatever Render produced,
		// Parse must accept it unchanged.
		if again := Parse(got).Render(); string(again) != string(got) {
			t.Fatalf("second round trip is not idempotent.\n--- in  ---\n%q\n--- out ---\n%q", got, again)
		}
	})
}
