package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestStatusSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	// runStatus will attempt to load config and check active provider
	err := runStatus(context.Background(), options{}, &out, &errOut)
	// Output should at least have version header and provider line
	output := out.String()
	if !strings.Contains(output, "boop") {
		t.Errorf("output missing boop header:\n%s", output)
	}
	if !strings.Contains(output, "provider") {
		t.Errorf("output missing provider line:\n%s", output)
	}
	if !strings.Contains(output, "model") {
		t.Errorf("output missing model line:\n%s", output)
	}
	if !strings.Contains(output, "mode") {
		t.Errorf("output missing mode line:\n%s", output)
	}
	_ = err
}

func TestStatusFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	_ = run([]string{"--status"}, &out, &errOut)
	output := out.String()
	if !strings.Contains(output, "boop") {
		t.Errorf("output missing boop header:\n%s", output)
	}
	if !strings.Contains(output, "provider") {
		t.Errorf("output missing provider line:\n%s", output)
	}
}
