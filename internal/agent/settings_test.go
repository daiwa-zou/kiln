package agent

import (
	"strings"
	"testing"
	"time"
)

// Settings is the knob bag a fork's provider reads its configuration from, so
// the accessors' coercions are contract: what a TOML file, a JSON body, and Go
// code hand over must all read back the same way.

func TestSettingsRequireString(t *testing.T) {
	s := Settings{"model": "opus", "empty": ""}

	if v, err := s.RequireString("model"); err != nil || v != "opus" {
		t.Errorf("RequireString(model) = %q, %v", v, err)
	}
	// Empty and absent are the same failure: the provider cannot run either way,
	// and the error must name the setting so the operator knows what to set.
	for _, key := range []string{"empty", "absent"} {
		if _, err := s.RequireString(key); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("RequireString(%s) err = %v, want an error naming the key", key, err)
		}
	}
}

func TestSettingsBool(t *testing.T) {
	s := Settings{"on": true, "off": false, "str": "true"}

	if v, ok := s.Bool("on"); !ok || !v {
		t.Errorf("Bool(on) = %v, %v", v, ok)
	}
	if v, ok := s.Bool("off"); !ok || v {
		t.Errorf("Bool(off) = %v, %v", v, ok)
	}
	// A string "true" is not a bool; coercing it would guess.
	if _, ok := s.Bool("str"); ok {
		t.Error("Bool coerced a string")
	}
	if _, ok := s.Bool("absent"); ok {
		t.Error("Bool reported an absent key")
	}
}

func TestSettingsFloatAcceptsEveryNumericArrival(t *testing.T) {
	// The same setting arrives as float64 from JSON, int64 from TOML, and
	// int or float32 from Go code; the provider should not care which.
	s := Settings{
		"f64": float64(1.5), "f32": float32(2.5), "int": int(3), "i64": int64(4),
		"str": "5",
	}
	for key, want := range map[string]float64{"f64": 1.5, "f32": 2.5, "int": 3, "i64": 4} {
		if v, ok := s.Float(key); !ok || v != want {
			t.Errorf("Float(%s) = %v, %v; want %v", key, v, ok, want)
		}
	}
	if _, ok := s.Float("str"); ok {
		t.Error("Float coerced a string")
	}
	if _, ok := s.Float("absent"); ok {
		t.Error("Float reported an absent key")
	}
}

func TestSettingsStringSlice(t *testing.T) {
	s := Settings{
		"list":  []string{"a", "b"},
		"one":   "solo",
		"anys":  []any{"x", "y"},
		"mixed": []any{"x", 7},
	}

	if v, ok := s.StringSlice("list"); !ok || len(v) != 2 {
		t.Errorf("StringSlice(list) = %v, %v", v, ok)
	}
	// `x = "a"` and `x = ["a"]` mean the same thing in a config file.
	if v, ok := s.StringSlice("one"); !ok || len(v) != 1 || v[0] != "solo" {
		t.Errorf("StringSlice(one) = %v, %v", v, ok)
	}
	// TOML/JSON decode lists as []any; strings inside convert, anything else
	// fails whole rather than dropping elements silently.
	if v, ok := s.StringSlice("anys"); !ok || len(v) != 2 || v[1] != "y" {
		t.Errorf("StringSlice(anys) = %v, %v", v, ok)
	}
	if _, ok := s.StringSlice("mixed"); ok {
		t.Error("StringSlice accepted a list with a non-string")
	}
}

func TestSettingsDuration(t *testing.T) {
	s := Settings{"typed": 2 * time.Second, "str": "150ms", "bad": "soon", "num": 5}

	if v, ok := s.Duration("typed"); !ok || v != 2*time.Second {
		t.Errorf("Duration(typed) = %v, %v", v, ok)
	}
	if v, ok := s.Duration("str"); !ok || v != 150*time.Millisecond {
		t.Errorf("Duration(str) = %v, %v", v, ok)
	}
	for _, key := range []string{"bad", "num", "absent"} {
		if _, ok := s.Duration(key); ok {
			t.Errorf("Duration(%s) accepted an unparseable value", key)
		}
	}
}
