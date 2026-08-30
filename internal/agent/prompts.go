package agent

import (
	_ "embed"
	"strings"
)

// The agent runtime's prompts are embedded rather than read from disk so a
// released binary behaves identically wherever it runs, matching how
// app.DefaultSystemPrompt handles the main system prompt (§29). Go's embed
// directive cannot reach outside the package directory, so these are copies of
// the repository's prompts/planner.md and prompts/agent.md; keep them in sync
// when the originals change.

//go:embed prompts/planner.md
var plannerPrompt string

//go:embed prompts/worker.md
var workerPrompt string

//go:embed prompts/reviewer.md
var reviewerPrompt string

// planResponseContract is appended to the planner prompt.
//
// It lives here rather than in the prompt file because it is a wire format
// belonging to parsePlan, not planning guidance: if the parser changes, this
// must change with it, in the same commit.
const planResponseContract = `

## Response format

Reply with a single JSON object and no other text:

{
  "tasks": [
    {
      "id": "t1",
      "description": "what to do, in one or two sentences",
      "dependencies": ["id of a task that must finish first"],
      "reads": ["paths this task needs to look at"],
      "writes": ["paths this task will modify"],
      "tools": ["read", "write"],
      "validation": "how completion is checked"
    }
  ]
}

Rules:

- Ids are short and unique. Dependencies name ids from this same list.
- "writes" must be accurate: two tasks that write the same path are forced to
  run one after the other, and a task that omits a path it writes will collide
  with another agent.
- Grant the smallest tool set each task needs.
- Never produce a dependency cycle.`

// PlannerPrompt returns the planner system prompt including the JSON response
// contract the plan parser expects.
func PlannerPrompt() string {
	return strings.TrimSpace(plannerPrompt) + planResponseContract
}

// WorkerPrompt returns the system prompt given to a worker agent.
func WorkerPrompt() string { return strings.TrimSpace(workerPrompt) }

// ReviewerPrompt returns the system prompt given to a code reviewer agent.
func ReviewerPrompt() string { return strings.TrimSpace(reviewerPrompt) }
