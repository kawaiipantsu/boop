package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"

	"github.com/kawaiipantsu/boop/internal/permissions"
)

// DefaultMaxQuestionsPerTurn caps in-turn questions to prevent infinite loops or model laziness.
const DefaultMaxQuestionsPerTurn = 3

// Question carries the prompt and choices submitted to the user.
type Question struct {
	Prompt  string   `json:"prompt"`
	Choices []string `json:"choices,omitempty"`
}

// Questioner is the contract implemented by frontends (TUI, WebUI, CLI) to prompt the user.
type Questioner interface {
	Ask(ctx context.Context, q Question) (string, error)
}

// DefaultCLIQuestioner handles interactive terminal prompts.
type DefaultCLIQuestioner struct {
	In  io.Reader
	Out io.Writer
}

func (q *DefaultCLIQuestioner) Ask(ctx context.Context, question Question) (string, error) {
	in := q.In
	if in == nil {
		in = os.Stdin
	}
	out := q.Out
	if out == nil {
		out = os.Stderr
	}

	fmt.Fprintf(out, "\n❓ [Question from Agent]\n%s\n", question.Prompt)
	if len(question.Choices) > 0 {
		fmt.Fprintln(out, "Choices:")
		for i, c := range question.Choices {
			fmt.Fprintf(out, "  %d) %s\n", i+1, c)
		}
	}
	fmt.Fprint(out, "Your answer: ")

	scanner := bufio.NewScanner(in)
	if scanner.Scan() {
		answer := strings.TrimSpace(scanner.Text())
		return answer, nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", errors.New("no answer provided (end of input)")
}

// AskTool lets the model ask the user a clarifying question mid-turn without ending the session turn.
type AskTool struct {
	Questioner Questioner
	MaxPerTurn int
	count      atomic.Int32
}

// NewAskTool returns an ask tool backed by questioner.
func NewAskTool(questioner Questioner) *AskTool {
	if questioner == nil {
		questioner = &DefaultCLIQuestioner{}
	}
	return &AskTool{
		Questioner: questioner,
		MaxPerTurn: DefaultMaxQuestionsPerTurn,
	}
}

type askArgs struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices,omitempty"`
}

// AskData is the structured result payload of an ask call.
type AskData struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices,omitempty"`
	Answer   string   `json:"answer"`
}

// Name implements Tool.
func (t *AskTool) Name() string { return "ask" }

// Description implements Tool.
func (t *AskTool) Description() string {
	return "Ask the user a clarifying question mid-turn when faced with genuine ambiguity. " +
		"Use this instead of guessing or abandoning work in progress. Do not use for trivial decisions."
}

// Schema implements Tool.
func (t *AskTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{
				"type":        "string",
				"description": "The specific question you need the user to clarify.",
			},
			"choices": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "string",
				},
				"description": "Optional list of distinct choices (e.g. ['option A', 'option B']).",
			},
		},
		"required":             []string{"question"},
		"additionalProperties": false,
	}
}

// Permission implements Tool.
func (t *AskTool) Permission(call Call) (permissions.Action, error) {
	var a askArgs
	if err := call.Bind(&a); err != nil {
		return permissions.Action{}, err
	}
	return permissions.Action{
		Category: permissions.CatFilesystemRead,
		Risk:     permissions.RiskLow,
		Tool:     t.Name(),
		Summary:  fmt.Sprintf("ask user: %s", a.Question),
	}, nil
}

// ResetTurn clears the per-turn question counter.
func (t *AskTool) ResetTurn() {
	t.count.Store(0)
}

// Execute asks the question via the Questioner interface.
func (t *AskTool) Execute(ctx context.Context, call Call) (Result, error) {
	var a askArgs
	if err := call.Bind(&a); err != nil {
		return Errorf(call, "ask: %v", err), nil
	}
	if strings.TrimSpace(a.Question) == "" {
		return Errorf(call, "ask: question must not be empty"), nil
	}

	max := t.MaxPerTurn
	if max <= 0 {
		max = DefaultMaxQuestionsPerTurn
	}
	if int(t.count.Add(1)) > max {
		return Errorf(call, "ask: exceeded maximum questions per turn (%d); proceed with your best judgement and document your assumptions", max), nil
	}

	answer, err := t.Questioner.Ask(ctx, Question{
		Prompt:  a.Question,
		Choices: a.Choices,
	})
	if err != nil {
		return Errorf(call, "ask: failed to get user answer: %v", err), nil
	}

	return Result{
		CallID:  call.ID,
		Tool:    t.Name(),
		Content: fmt.Sprintf("User Answer: %s", answer),
		Data: AskData{
			Question: a.Question,
			Choices:  a.Choices,
			Answer:   answer,
		},
		Display: fmt.Sprintf("answered: %s", answer),
	}, nil
}
