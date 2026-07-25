package agent

import (
	"testing"

	"github.com/daiwa-zou/kiln/internal/config"
)

func TestNewSelectsRunner(t *testing.T) {
	tests := []struct {
		name       string
		runner     config.AgentRunner
		wantAPI    bool
		wantsFiles bool
	}{
		{"api", config.RunnerAPI, true, false},
		{"cli", config.RunnerCLI, false, true},
		{"empty defaults to api", "", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{Agent: config.Agent{Runner: tt.runner, Binary: "claude"}}

			r, err := New(cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			_, isAPI := r.(*APIRunner)
			if isAPI != tt.wantAPI {
				t.Errorf("got APIRunner=%v, want %v", isAPI, tt.wantAPI)
			}

			// The pipeline uses this to decide whether it needs a scratch
			// directory at all; the API runner needs none.
			if got := WritesFiles(r); got != tt.wantsFiles {
				t.Errorf("WritesFiles() = %v, want %v", got, tt.wantsFiles)
			}
		})
	}
}

func TestNewRejectsUnknownRunner(t *testing.T) {
	cfg := &config.Config{Agent: config.Agent{Runner: "carrier-pigeon"}}
	if _, err := New(cfg); err == nil {
		t.Error("New accepted an unknown runner")
	}
}

func TestNewPassesModelThrough(t *testing.T) {
	cfg := &config.Config{Agent: config.Agent{
		Runner: config.RunnerAPI, Model: ModelOpus5, Effort: "max",
	}}

	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	api, ok := r.(*APIRunner)
	if !ok {
		t.Fatal("expected an APIRunner")
	}
	if api.Model != ModelOpus5 {
		t.Errorf("Model = %q, want %q", api.Model, ModelOpus5)
	}
	if string(api.Effort) != "max" {
		t.Errorf("Effort = %q, want max", api.Effort)
	}
}

func TestNewDefaultsToSonnet(t *testing.T) {
	// A config with no model must not silently pick the expensive one.
	r, err := New(&config.Config{Agent: config.Agent{Runner: config.RunnerAPI}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if api := r.(*APIRunner); api.Model != ModelSonnet5 {
		t.Errorf("default model = %q, want %q", api.Model, ModelSonnet5)
	}
}

func TestBothRunnersSatisfyTheInterface(t *testing.T) {
	// The whole point of keeping both: one interface, so nothing downstream
	// branches on which transport is configured.
	var _ Runner = (*APIRunner)(nil)
	var _ Runner = (*ClaudeRunner)(nil)
}
