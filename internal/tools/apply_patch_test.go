package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyPatchSingleFile(t *testing.T) {
	origContent := `package main

import "fmt"

func main() {
	fmt.Println("Hello World")
}
`
	root := execWriteFixture(t, map[string]string{
		"main.go": origContent,
	})
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}

	patch := `--- a/main.go
+++ b/main.go
@@ -4,3 +4,3 @@
 func main() {
-	fmt.Println("Hello World")
+	fmt.Println("Hello Boop")
 }
`

	tool := NewApplyPatchTool(ws)
	call := Call{
		ID:   "call_patch_1",
		Name: "apply_patch",
		Arguments: []byte(`{
			"patch": ` + strconvQuote(patch) + `
		}`),
	}

	res, err := tool.Execute(context.Background(), call)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if res.IsError {
		t.Fatalf("Expected success, got error: %s", res.Content)
	}

	newBytes, err := os.ReadFile(filepath.Join(root, "main.go"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(newBytes), `fmt.Println("Hello Boop")`) {
		t.Errorf("Patched file missing replacement:\n%s", string(newBytes))
	}
}

func TestApplyPatchFuzzyContextMatch(t *testing.T) {
	origContent := `// Header comment
// Extra line
package main

import "fmt"

func helper() string {
	return "help"
}

func main() {
	fmt.Println("Hello World")
}
`
	root := execWriteFixture(t, map[string]string{
		"main.go": origContent,
	})
	ws, _ := NewWorkspace(root)

	// Hunk header has intentional line offset (@@ -20,3 +20,3 @@ instead of line 11)
	patch := `--- a/main.go
+++ b/main.go
@@ -20,3 +20,3 @@
 func main() {
-	fmt.Println("Hello World")
+	fmt.Println("Hello Fuzzy Patch")
 }
`

	tool := NewApplyPatchTool(ws)
	call := Call{
		ID:   "call_patch_2",
		Name: "apply_patch",
		Arguments: []byte(`{
			"patch": ` + strconvQuote(patch) + `
		}`),
	}

	res, err := tool.Execute(context.Background(), call)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if res.IsError {
		t.Fatalf("Expected success with fuzzy context matching, got error: %s", res.Content)
	}

	newBytes, _ := os.ReadFile(filepath.Join(root, "main.go"))
	if !strings.Contains(string(newBytes), `fmt.Println("Hello Fuzzy Patch")`) {
		t.Errorf("Fuzzy patched content incorrect:\n%s", string(newBytes))
	}
}

func strconvQuote(s string) string {
	b, _ := jsonMarshal(s)
	return string(b)
}

func jsonMarshal(v any) ([]byte, error) {
	return []byte(`"` + strings.ReplaceAll(strings.ReplaceAll(v.(string), `\`, `\\`), `"`, `\"`) + `"`), nil
}
