package config

import (
	"strings"
	"testing"
	"time"
)

// stale_after is a deadline on heartbeat silence now, so what has to hold is
// that it leaves room for the heartbeat that keeps a live build under it. A
// deadline barely above the interval requeues a healthy run on one slow beat,
// and the run is then built twice.
func TestValidateStaleAfterLeavesRoomForHeartbeats(t *testing.T) {
	base := func() *Config {
		return &Config{
			Role: RoleServer, HTTPAddr: ":8080",
			Database: Database{Host: "h", Port: 5432, Name: "kiln", MaxConns: 10, MinConns: 2},
			Storage:  Storage{Backend: BackendFS, Path: "/tmp/blobs"},
			Agent:    Agent{Runner: RunnerFake, Timeout: time.Minute, MaxPagesPerRun: 12, UnitConcurrency: 1},
			Worker:   Worker{StaleAfter: 15 * time.Minute, HeartbeatInterval: time.Minute},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("the shipped Helm values were rejected: %v", err)
	}

	c := base()
	c.Worker.StaleAfter = 2 * time.Minute // under three beats
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "heartbeat_interval") {
		t.Fatalf("a deadline with one beat of slack was accepted: %v", err)
	}

	c = base()
	c.Worker.StaleAfter = 3*time.Minute + 30*time.Second
	if err := c.Validate(); err != nil {
		t.Errorf("three intervals of slack rejected: %v", err)
	}

	c = base()
	c.Worker.HeartbeatInterval = -time.Second
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "heartbeat_interval") {
		t.Errorf("a negative heartbeat interval was accepted: %v", err)
	}

	// The existing rule is untouched.
	c = base()
	c.Worker.StaleAfter = 30 * time.Second
	c.Worker.HeartbeatInterval = time.Second
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "agent.timeout") {
		t.Errorf("stale_after under agent.timeout was accepted: %v", err)
	}
}
