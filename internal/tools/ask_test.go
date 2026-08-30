package tools

import (
	"context"
	"strings"
	"testing"
)

type fakeQuestioner struct {
	answer string
	err    error
	asked  Question
}

func (f *fakeQuestioner) Ask(ctx context.Context, q Question) (string, error) {
	f.asked = q
	return f.answer, f.err
}

func TestAskToolSuccess(t *testing.T) {
	fq := &fakeQuestioner{answer: "Option 2"}
	tool := NewAskTool(fq)

	res, err := tool.Execute(context.Background(), Call{
		ID:   "call_ask_1",
		Name: "ask",
		Arguments: []byte(`{
			"question": "Which database should we use?",
			"choices": ["PostgreSQL", "SQLite", "MySQL"]
		}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("ask failed: %v, content: %s", err, res.Content)
	}

	if fq.asked.Prompt != "Which database should we use?" {
		t.Errorf("prompt = %q, want 'Which database should we use?'", fq.asked.Prompt)
	}
	if len(fq.asked.Choices) != 3 {
		t.Errorf("choices count = %d, want 3", len(fq.asked.Choices))
	}
	if !strings.Contains(res.Content, "User Answer: Option 2") {
		t.Errorf("content missing answer:\n%s", res.Content)
	}
}

func TestAskToolRateLimitPerTurn(t *testing.T) {
	fq := &fakeQuestioner{answer: "yes"}
	tool := NewAskTool(fq)
	tool.MaxPerTurn = 2

	for i := 0; i < 2; i++ {
		res, err := tool.Execute(context.Background(), Call{
			ID:        "call_ask_loop",
			Name:      "ask",
			Arguments: []byte(`{"question": "Continue?"}`),
		})
		if err != nil || res.IsError {
			t.Fatalf("call %d failed unexpectedly: %v", i+1, res.Content)
		}
	}

	// 3rd call should fail rate limit
	res, err := tool.Execute(context.Background(), Call{
		ID:        "call_ask_loop_3",
		Name:      "ask",
		Arguments: []byte(`{"question": "Continue again?"}`),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "exceeded maximum questions") {
		t.Errorf("expected rate limit error, got: %s", res.Content)
	}
}
