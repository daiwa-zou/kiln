// Package connector acquires raw material and materializes it locally.
//
// A Connector is the only thing that knows where sources come from. Everything
// after it -- mapping, diffing, generation, validation, import -- operates on a
// SourceSet and never learns whether the material arrived from a git checkout,
// an upload, or a web fetch.
package connector

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// TriggerMode describes how a connector learns it has work to do.
type TriggerMode string

const (
	// TriggerManual only syncs when asked.
	TriggerManual TriggerMode = "manual"
	// TriggerWebhook syncs when the origin pushes an event.
	TriggerWebhook TriggerMode = "webhook"
	// TriggerPoll syncs on a schedule, for origins with no useful webhook.
	TriggerPoll TriggerMode = "poll"
)

// Item is one piece of raw material.
type Item struct {
	// Key is the namespaced cache key. It must be stable across syncs: the
	// hash comparison that makes an unchanged run free is keyed on it.
	Key string
	// Kind distinguishes what the item is -- module, doc, page.
	Kind string
	// Title is human-readable.
	Title string
	// Hash is a digest of normalized content. This is the skip gate, so it
	// must depend only on content, never on where the material was staged.
	Hash string
	// Path is where the materialized content lives, relative to Root.
	Path string
	// Origin deep-links back to the source, for evidence links.
	Origin string
	Meta   map[string]any
}

// SourceSet is what a Connector materializes for a Mapper to read.
type SourceSet struct {
	// Root is the local directory holding the materialized material.
	Root string
	// Kind names the connector that produced this, so the pipeline can pick a
	// matching Mapper.
	Kind  string
	Items []Item
	// Native carries the connector's own richer description, for consumers that
	// know the kind and need more than a flat item list -- the git connector
	// puts its *repomap.RepoMap here so routing and prompt grounding get the
	// module graph without a second scan. Consumers that do not recognize the
	// kind ignore it, which keeps the generic path genuinely generic.
	Native any
}

// Connector acquires source material.
//
// Sync is the whole interface on purpose. Anything a connector needs to do
// incrementally -- shallow fetch, cursor, conditional GET -- is its own business
// and is expressed through the item hashes it reports, not through extra methods
// the pipeline would have to understand.
type Connector interface {
	// Kind identifies the connector: git, upload, web.
	Kind() string
	// Trigger says how this connector expects to be scheduled.
	Trigger() TriggerMode
	// Sync materializes source material into dst and describes what it found.
	// dst may be ignored by connectors whose material is already local.
	Sync(ctx context.Context, cfg Config, dst string) (*SourceSet, error)
}

// Config is connector-specific settings, kept opaque so adding a connector does
// not widen a shared struct.
type Config map[string]any

// String reads a string setting, reporting whether it was present.
func (c Config) String(key string) (string, bool) {
	v, ok := c[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// RequireString reads a setting that has no sensible default.
func (c Config) RequireString(key string) (string, error) {
	s, ok := c.String(key)
	if !ok || s == "" {
		return "", fmt.Errorf("connector: %q is required", key)
	}
	return s, nil
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Connector{}
)

// Register makes a connector available by kind. Registering the same kind twice
// panics: it means two implementations disagree about who owns a source type,
// which is a wiring bug rather than a runtime condition.
func Register(c Connector) {
	registryMu.Lock()
	defer registryMu.Unlock()

	if _, exists := registry[c.Kind()]; exists {
		panic(fmt.Sprintf("connector: %q registered twice", c.Kind()))
	}
	registry[c.Kind()] = c
}

// Get returns a registered connector.
func Get(kind string) (Connector, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	c, ok := registry[kind]
	if !ok {
		return nil, fmt.Errorf("connector: no connector registered for %q (have: %v)", kind, kindsLocked())
	}
	return c, nil
}

// Kinds lists the registered connector kinds.
func Kinds() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return kindsLocked()
}

func kindsLocked() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
