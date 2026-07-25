package connector

import (
	"context"
	"testing"
)

type stubConnector struct {
	kind    string
	trigger TriggerMode
}

func (s *stubConnector) Kind() string         { return s.kind }
func (s *stubConnector) Trigger() TriggerMode { return s.trigger }
func (s *stubConnector) Sync(context.Context, Config, string) (*SourceSet, error) {
	return &SourceSet{Kind: s.kind}, nil
}

func TestConfigAccessors(t *testing.T) {
	cfg := Config{"path": "/tmp/x", "empty": "", "number": 42}

	if got, ok := cfg.String("path"); !ok || got != "/tmp/x" {
		t.Errorf("String(path) = %q, %v", got, ok)
	}
	if _, ok := cfg.String("absent"); ok {
		t.Error("String reported a missing key as present")
	}
	// A non-string value is not a string; reporting it as one would produce a
	// confusing zero value downstream.
	if _, ok := cfg.String("number"); ok {
		t.Error("String accepted a non-string value")
	}

	if _, err := cfg.RequireString("path"); err != nil {
		t.Errorf("RequireString(path): %v", err)
	}
	// An empty value is as unusable as a missing one, so both must error.
	for _, key := range []string{"absent", "empty"} {
		if _, err := cfg.RequireString(key); err == nil {
			t.Errorf("RequireString(%q) accepted an unusable value", key)
		}
	}
}

func TestRegistry(t *testing.T) {
	// Registration is process-global, so this uses names no real connector uses.
	Register(&stubConnector{kind: "test-alpha", trigger: TriggerManual})
	Register(&stubConnector{kind: "test-beta", trigger: TriggerPoll})

	got, err := Get("test-alpha")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Kind() != "test-alpha" {
		t.Errorf("Kind() = %q", got.Kind())
	}

	// The error should say what is available, since the usual cause is a
	// missing blank import rather than a typo.
	_, err = Get("test-nonexistent")
	if err == nil {
		t.Fatal("Get accepted an unregistered kind")
	}
	if !contains(Kinds(), "test-alpha") || !contains(Kinds(), "test-beta") {
		t.Errorf("Kinds() = %v", Kinds())
	}
}

func TestRegisterTwicePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("registering a kind twice should panic; it means two implementations claim one source type")
		}
	}()

	Register(&stubConnector{kind: "test-duplicate"})
	Register(&stubConnector{kind: "test-duplicate"})
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
