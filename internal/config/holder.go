package config

import (
	"encoding/json"
	"sync/atomic"
)

// Holder is a concurrency-safe container for the live *Config.
//
// The runtime reads configuration from many goroutines at once — the tool loop,
// the model router, the permission evaluator, the WebUI's HTTP handlers. A
// Holder lets a setting change while Boop is running: a reader takes a snapshot
// with Load(), and Store() publishes a replacement wholesale. The *Config a
// Holder points at must never be mutated after it is stored; build a fresh one
// with Config.Clone, change that, and Store it.
type Holder struct {
	ptr atomic.Pointer[Config]
}

// NewHolder returns a Holder wrapping c. c may be nil.
func NewHolder(c *Config) *Holder {
	h := &Holder{}
	h.ptr.Store(c)
	return h
}

// Load returns the current configuration snapshot. It never blocks.
func (h *Holder) Load() *Config { return h.ptr.Load() }

// Store publishes next as the current configuration. Callers must not mutate
// next afterwards.
func (h *Holder) Store(next *Config) { h.ptr.Store(next) }

// Clone returns a deep copy of c that is safe to mutate without affecting the
// original. It round-trips through JSON, so every field that already crosses
// the /api/config boundary round-trips here too, and a new field is covered the
// moment it is given a json tag.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	data, err := json.Marshal(c)
	if err != nil {
		// Config is plain data with a json tag on every field; a marshal
		// failure would be a programming error, not a runtime condition.
		panic("config: Clone: marshal: " + err.Error())
	}
	var out Config
	if err := json.Unmarshal(data, &out); err != nil {
		panic("config: Clone: unmarshal: " + err.Error())
	}
	return &out
}
