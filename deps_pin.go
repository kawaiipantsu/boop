//go:build pin

// Package boop pins dependencies that are resolved centrally.
//
// Parallel work across packages must never race on go.mod, so every module the
// implementation milestones need is required here up front. Delete an entry
// once a real import in the tree keeps the module in go.mod on its own.
package boop

import (
	_ "github.com/charmbracelet/bubbles/textarea"
	_ "github.com/charmbracelet/bubbletea"
	_ "github.com/charmbracelet/lipgloss"
	_ "github.com/coder/websocket"
	_ "github.com/google/uuid"
	_ "gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)
