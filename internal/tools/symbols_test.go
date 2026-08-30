package tools

import (
	"context"
	"strings"
	"testing"
)

func TestSymbolsToolExtraction(t *testing.T) {
	root := execWriteFixture(t, map[string]string{
		"sample.go": `package sample

const MaxRetries = 3

type Greeter interface {
	Greet(name string) string
}

type User struct {
	Name string
}

func (u *User) Greet(name string) string {
	return "Hello " + name
}

func HelperFunc(a int, b int) (int, error) {
	return a + b, nil
}
`,
	})

	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}

	tool := NewSymbolsTool(ws)

	res, err := tool.Execute(context.Background(), Call{
		ID:   "call_sym_1",
		Name: "symbols",
		Arguments: []byte(`{
			"path": "sample.go"
		}`),
	})
	if err != nil || res.IsError {
		t.Fatalf("symbols execution failed: %v, content: %s", err, res.Content)
	}

	for _, want := range []string{"MaxRetries", "Greeter", "User", "Greet", "HelperFunc"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("symbols output missing %q:\n%s", want, res.Content)
		}
	}
}
