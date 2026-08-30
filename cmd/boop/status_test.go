package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/kawaiipantsu/boop/internal/version"
)

func sampleStatus() statusInfo {
	return statusInfo{
		Version: version.Info{Version: "1.2.3"},
		Project: "/work/proj",
		Provider: providerStatus{
			Name:      "ollama",
			BaseURL:   "http://127.0.0.1:11434",
			Healthy:   true,
			LatencyMS: 12,
		},
		Model: modelStatus{
			Name:          "qwen2.5:7b",
			Explicit:      true,
			CapsKnown:     true,
			Tools:         true,
			ContextWindow: 32768,
		},
		Mode:     "confirm",
		Agents:   agentStatusInfo{Enabled: false, Max: 5},
		Network:  false,
		Session:  "none",
		Warnings: []string{"provider \"openai\" unavailable: OPENAI_API_KEY is not set"},
	}
}

func TestRenderStatusText(t *testing.T) {
	var buf bytes.Buffer
	if err := renderStatus(&buf, sampleStatus(), false); err != nil {
		t.Fatalf("renderStatus() = %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"boop v1.2.3", "/work/proj",
		"provider", "ollama", "http://127.0.0.1:11434", "healthy (12ms)",
		"qwen2.5:7b", "tools ✓", "vision ✗", "32768 ctx",
		"agents off (max 5)", "network off",
		"session   none",
		"OPENAI_API_KEY is not set",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("text report missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderStatusUnhealthyHidesCapabilities(t *testing.T) {
	s := sampleStatus()
	s.Provider.Healthy = false
	s.Provider.Error = "connection refused"
	s.Model.CapsKnown = false

	var buf bytes.Buffer
	if err := renderStatus(&buf, s, false); err != nil {
		t.Fatalf("renderStatus() = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "unhealthy: connection refused") {
		t.Errorf("want the failure reason, got:\n%s", out)
	}
	if !strings.Contains(out, "capabilities unknown (provider unreachable)") {
		t.Errorf("want a capabilities placeholder, got:\n%s", out)
	}
	if strings.Contains(out, "tools ✓") {
		t.Errorf("capabilities must not be shown for an unreachable provider:\n%s", out)
	}
}

func TestRenderStatusJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := renderStatus(&buf, sampleStatus(), true); err != nil {
		t.Fatalf("renderStatus() = %v", err)
	}
	var round statusInfo
	if err := json.Unmarshal(buf.Bytes(), &round); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if round.Provider.Name != "ollama" || !round.Provider.Healthy {
		t.Errorf("provider did not round-trip: %+v", round.Provider)
	}
	if round.Model.ContextWindow != 32768 {
		t.Errorf("context window did not round-trip: %d", round.Model.ContextWindow)
	}
}

func TestStatusSubcommandRejectsPositionalArgs(t *testing.T) {
	err := runStatusCommand(context.Background(), []string{"extra"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no positional arguments") {
		t.Fatalf("runStatusCommand(extra) = %v, want a positional-argument error", err)
	}
}

func TestStatusFlagIsParsed(t *testing.T) {
	got, err := parse([]string{"--status", "--status-json"}, io.Discard)
	if err != nil {
		t.Fatalf("parse() = %v", err)
	}
	if !got.showStatus || !got.statusJSON {
		t.Errorf("showStatus=%v statusJSON=%v, want both true", got.showStatus, got.statusJSON)
	}
}
