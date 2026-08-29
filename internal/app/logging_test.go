package app

import (
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/config"
	"github.com/kawaiipantsu/boop/internal/logging"
)

// A resolved credential must be registered with the redactor, because
// shape-based detection cannot recognise a key that looks like an ordinary
// word — which is exactly what a self-hosted gateway often issues (§45).
func TestBuildProviderRegistersItsCredentialForRedaction(t *testing.T) {
	logging.ClearSecrets()
	t.Cleanup(logging.ClearSecrets)

	const key = "totally-ordinary-looking-value"
	t.Setenv("BOOP_TEST_KEY_VAR", key)

	_, err := BuildProvider("gateway", config.ProviderConfig{
		Type: "openai", BaseURL: "https://gateway.example/v1", APIKeyEnv: "BOOP_TEST_KEY_VAR",
	}, nil)
	if err != nil {
		t.Fatalf("BuildProvider() = %v", err)
	}

	got := logging.RedactString("calling with " + key)
	if strings.Contains(got, key) {
		t.Errorf("the credential survived redaction: %q", got)
	}
	if !strings.Contains(got, "calling with ") {
		t.Errorf("surrounding text was mangled: %q", got)
	}
}

// A local provider needs no credential, and starting one must not register an
// empty string as a secret — that would blank out ordinary log text.
func TestLocalProviderRegistersNothing(t *testing.T) {
	logging.ClearSecrets()
	t.Cleanup(logging.ClearSecrets)

	if _, err := BuildProvider("ollama", config.ProviderConfig{
		Type: "ollama", BaseURL: "http://127.0.0.1:11434",
	}, nil); err != nil {
		t.Fatalf("BuildProvider() = %v", err)
	}
	if n := logging.RegisteredSecretCount(); n != 0 {
		t.Errorf("RegisteredSecretCount() = %d, want 0", n)
	}
}
