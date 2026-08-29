package tools

import "testing"

func TestZZDump(t *testing.T) {
	cases := map[string]string{
		"secret.pdf":  attachEncryptedPDF(),
		"scan.pdf":    attachScannedPDF(),
		"broken.pdf":  attachCorruptPDF(),
		"old.doc":     attachLegacyDOC(),
		"guide.pdf":   attachTextPDF("Hello page one", "Hello page two"),
		"diagram.png": attachPNG(t, 40, 20),
		"report.docx": attachDOCX(t, "First paragraph", "Second paragraph"),
		"data.bin":    "\x00\x01\x02\x03binarygarbage\x00\xff",
	}
	for name, body := range cases {
		ws := fsTestWorkspace(t, map[string]string{name: body})
		res := fsTestExec(t, NewAttachTool(ws), map[string]any{"path": name})
		t.Logf("=== %s err=%v display=%q\n%s\n", name, res.IsError, res.Display, res.Content)
		a, _ := tPerm(t, ws, name)
		t.Logf("--- permission summary: %s", a)
	}
}

func tPerm(t *testing.T, ws *Workspace, path string) (string, error) {
	tool := NewAttachTool(ws)
	a, err := tool.Permission(fsTestCall(t, tool.Name(), map[string]any{"path": path}))
	return a.Summary, err
}
