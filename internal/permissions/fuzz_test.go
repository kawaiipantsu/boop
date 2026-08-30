package permissions

import (
	"testing"
)

func FuzzClassifyCommand(f *testing.F) {
	seeds := []string{
		"git status",
		"rm -rf /",
		"curl -s http://example.com",
		"cat README.md",
		"ls -la",
		"sudo reboot",
		":(){ :|:& };:",
		"echo 'hello world'",
		"kill -9 1234",
		"chmod 777 script.sh",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, cmd string) {
		// Classification must not panic on any arbitrary string input.
		cl := ClassifyCommand(cmd)
		if cl.Risk < RiskLow || cl.Risk > RiskCritical {
			t.Errorf("invalid risk level %v for cmd %q", cl.Risk, cmd)
		}
	})
}
