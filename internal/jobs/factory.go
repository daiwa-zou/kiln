package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/daiwa-zou/kiln/internal/agent"
	"github.com/daiwa-zou/kiln/internal/config"
)

// NewPipeline assembles a Pipeline from resolved configuration. The CLI and
// the worker both construct through here, so budget and model wiring cannot
// drift between the two.
func NewPipeline(cfg *config.Config, st Store, runner agent.Runner, log *slog.Logger) *Pipeline {
	return &Pipeline{
		Store: st, Runner: runner, Log: log,
		Budget: Budget{
			AnalyzeUSD: cfg.Agent.AnalyzeBudgetUSD,
			PageUSD:    cfg.Agent.PageBudgetUSD,
			RunUSD:     cfg.Agent.RunBudgetUSD,
			MaxPages:   cfg.Agent.MaxPagesPerRun,
		},
		Model:         cfg.Agent.Model,
		AnalyzeModel:  cfg.Agent.AnalyzeModel,
		FallbackModel: cfg.Agent.FallbackModel,
		Timeout:       cfg.Agent.Timeout,
		MaxRetries:    1,
		WarnTurns:     cfg.Agent.WarnTurns,
	}
}

// NewRunID mints an identifier for a run started outside the queue. Queued
// runs use their database UUID instead, which is how RecordRun knows to finish
// the existing row rather than insert a new one.
func NewRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return "run-" + hex.EncodeToString(b[:])
}
