//go:build pin

// Package boop pins dependencies that no real import holds yet.
//
// Parallel work across packages must never race on go.mod, so modules the
// remaining milestones need are required here up front. yaml, sqlite and uuid
// have been removed: real imports now keep them in go.mod on their own.
// Delete an entry once its milestone lands and imports it for real.
package boop

import (
	// Milestone 3, the TUI.
	_ "github.com/charmbracelet/bubbles/textarea"
	_ "github.com/charmbracelet/bubbletea"
	_ "github.com/charmbracelet/lipgloss"

	// Milestone 9, the WebUI event stream.
	_ "github.com/coder/websocket"
)
