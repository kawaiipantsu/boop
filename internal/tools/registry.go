// Package tools is the schema-driven registry of actions a model may request.
//
// Models never receive process handles. Every model-initiated action arrives
// here as a Call, is classified for permission, and returns a structured
// Result — including on failure, so the model can diagnose and repair.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/kawaiipantsu/boop/internal/permissions"
	"github.com/kawaiipantsu/boop/internal/provider"
)

// Call is a decoded model request to invoke a tool.
type Call struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Bind decodes the call arguments into dst.
func (c Call) Bind(dst any) error {
	if len(c.Arguments) == 0 {
		return nil
	}
	if err := json.Unmarshal(c.Arguments, dst); err != nil {
		return fmt.Errorf("tool %s: invalid arguments: %w", c.Name, err)
	}
	return nil
}

// Result is the structured outcome of a tool invocation.
//
// IsError reports a failed operation, which is still a valid Result: the
// content is returned to the model rather than aborting the exchange.
type Result struct {
	CallID string `json:"call_id"`
	Tool   string `json:"tool"`
	// Content is the text representation returned to the model.
	Content string `json:"content"`
	// Data is the structured payload for UIs and persistence, such as a
	// execution.RunResult. It is not sent to the model directly.
	Data     any           `json:"data,omitempty"`
	IsError  bool          `json:"is_error"`
	Duration time.Duration `json:"duration"`
}

// Errorf builds a failed Result carrying a formatted message.
func Errorf(call Call, format string, args ...any) Result {
	return Result{CallID: call.ID, Tool: call.Name, Content: fmt.Sprintf(format, args...), IsError: true}
}

// Tool is one registered capability.
//
// Permission is consulted before Execute so the permission engine can classify
// the action without the tool performing side effects.
type Tool interface {
	Name() string
	Description() string
	// Schema is the JSON Schema object describing Arguments.
	Schema() map[string]any
	// Permission classifies what this call would do.
	Permission(call Call) (permissions.Action, error)
	Execute(ctx context.Context, call Call) (Result, error)
}

// Registry holds the tools available to a session. It is safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds t, replacing any tool already registered under the same name.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
}

// Get returns the named tool.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names lists registered tool names in sorted order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Definitions renders the registry as provider tool definitions, restricted to
// allowed when it is non-nil. A nil allowed set exposes every tool.
func (r *Registry) Definitions(allowed []string) []provider.ToolDefinition {
	var filter map[string]struct{}
	if allowed != nil {
		filter = make(map[string]struct{}, len(allowed))
		for _, name := range allowed {
			filter[name] = struct{}{}
		}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]provider.ToolDefinition, 0, len(r.tools))
	for name, t := range r.tools {
		if filter != nil {
			if _, ok := filter[name]; !ok {
				continue
			}
		}
		defs = append(defs, provider.ToolDefinition{Name: t.Name(), Description: t.Description(), Schema: t.Schema()})
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}
