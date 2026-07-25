// Package diff decides what regenerates.
//
// It compares a freshly built map against the sources recorded by the last
// successful run and produces the set of units that are dirty. When nothing is
// dirty, the run finishes without a single LLM call -- which is the primary
// cost control in kiln, not a nice-to-have.
package diff

import (
	"fmt"
	"strings"
)

// Key namespaces cache entries so each maps to exactly one producer. Without
// namespacing, a document named "overview" and the architecture synthesis would
// collide and silently overwrite one another.
type Key string

// Key prefixes.
const (
	PrefixModule = "module"
	PrefixDoc    = "doc"
	PrefixArch   = "arch"
	PrefixEntry  = "entry"
)

// ArchOverview is the singleton key for the synthesis pages that describe the
// whole workspace: architecture, data flow, dependency map.
const ArchOverview Key = "arch:overview"

// ModuleKey builds the key for a code module.
func ModuleKey(slug string) Key { return Key(PrefixModule + ":" + slug) }

// DocKey builds the key for an ingested document, URL, or research result.
func DocKey(id string) Key { return Key(PrefixDoc + ":" + id) }

// EntryKey builds the key for an entry point.
func EntryKey(name string) Key { return Key(PrefixEntry + ":" + name) }

// Prefix returns the namespace of a key.
func (k Key) Prefix() string {
	if i := strings.Index(string(k), ":"); i >= 0 {
		return string(k)[:i]
	}
	return ""
}

// ID returns the portion after the namespace.
func (k Key) ID() string {
	if i := strings.Index(string(k), ":"); i >= 0 {
		return string(k)[i+1:]
	}
	return string(k)
}

// Valid reports whether a key is well-formed.
func (k Key) Valid() bool {
	switch k.Prefix() {
	case PrefixModule, PrefixDoc, PrefixArch, PrefixEntry:
		return k.ID() != ""
	}
	return false
}

func (k Key) String() string { return string(k) }

// ParseKey validates and returns a key.
func ParseKey(s string) (Key, error) {
	k := Key(s)
	if !k.Valid() {
		return "", fmt.Errorf("diff: malformed cache key %q", s)
	}
	return k, nil
}
