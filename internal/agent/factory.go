package agent

import (
	"github.com/daiwa-zou/kiln/internal/config"
)

// Built-in providers. Registered here rather than switched on in New, so that
// the three kiln ships and one a fork adds are wired the same way -- and so
// that the set of runner names is a fact about the binary, discoverable via
// Providers(), rather than a list duplicated across the factory, config
// validation, and the documentation.
func init() {
	RegisterProvider(ProviderFunc(string(config.RunnerAPI), func(o Options) (Runner, error) {
		return NewAPIRunner(APIOptions{
			APIKey:  o.APIKey,
			BaseURL: o.BaseURL,
			Model:   o.Model,
			Effort:  o.Effort,
		}), nil
	}))

	RegisterProvider(ProviderFunc(string(config.RunnerCLI), func(o Options) (Runner, error) {
		r := NewClaudeRunner(o.Binary)
		// The subprocess reads untrusted source content; give it an explicit
		// minimal environment so it can never inherit KILN_* secrets.
		r.Env = MinimalChildEnv(o.APIKey)
		return r, nil
	}))

	RegisterProvider(ProviderFunc(string(config.RunnerFake), func(o Options) (Runner, error) {
		cost, _ := o.Settings.Float("fake_cost_usd")
		fail, _ := o.Settings.StringSlice("fake_fail_units")
		latency, _ := o.Settings.Duration("fake_latency")
		return NewFakeRunner(FakeOptions{
			CostPerCall: cost,
			FailUnits:   fail,
			Latency:     latency,
		}), nil
	}))
}

// New builds the runner selected by configuration.
//
// Every runner satisfies one interface, so nothing downstream branches on which
// is in use. Where they differ -- whether pages come back as files or as data,
// whether cost is measured or estimated -- they say so through Capabilities
// rather than being recognised by type.
func New(cfg *config.Config) (Runner, error) {
	name := string(cfg.Agent.Runner)
	if name == "" {
		name = string(config.RunnerAPI)
	}

	p, err := LookupProvider(name)
	if err != nil {
		return nil, err
	}
	return p.New(optionsFrom(cfg))
}

// optionsFrom resolves configuration into the provider-neutral Options. The
// credential is the one place this is still Anthropic-shaped: secrets.
// AnthropicAPIKey is the only model credential config carries today, so a
// provider that needs a different one reads it from Settings.
func optionsFrom(cfg *config.Config) Options {
	return Options{
		APIKey:        cfg.Secrets.AnthropicAPIKey,
		BaseURL:       cfg.Agent.BaseURL,
		Binary:        cfg.Agent.Binary,
		Model:         cfg.Agent.Model,
		AnalyzeModel:  cfg.Agent.AnalyzeModel,
		FallbackModel: cfg.Agent.FallbackModel,
		Effort:        cfg.Agent.Effort,
		Timeout:       cfg.Agent.Timeout,
		Settings:      settingsFrom(cfg),
	}
}

// settingsFrom merges the operator's `agent.settings` with the typed fake-runner
// knobs, which predate this and stay on config.Agent so existing configuration
// files keep working. Explicit settings win.
func settingsFrom(cfg *config.Config) Settings {
	s := Settings{
		"fake_cost_usd":   cfg.Agent.FakeCostUSD,
		"fake_fail_units": cfg.Agent.FakeFailUnits,
		"fake_latency":    cfg.Agent.FakeLatency,
	}
	for k, v := range cfg.Agent.Settings {
		s[k] = v
	}
	return s
}

// WritesFiles reports whether a runner produces pages on disk rather than as
// structured data. The pipeline uses this to decide whether it needs a scratch
// directory at all -- the API runner needs none, which is most of why it exists.
//
// A runner that states its own capabilities is believed. The type checks below
// are the fallback for those that do not, which keeps every existing fake in
// the test suite working without implementing an interface to say "no".
func WritesFiles(r Runner) bool {
	if c, ok := r.(CapableRunner); ok {
		return c.Capabilities().WritesFiles
	}
	_, isCLI := r.(*ClaudeRunner)
	return isCLI
}

// EstimatesCost reports whether a runner's TotalCostUSD comes from a local
// pricing table rather than being reported authoritatively by the provider.
// Budget enforcement needs to know: an estimated cost of zero means "unknown
// model", not "free".
func EstimatesCost(r Runner) bool {
	if c, ok := r.(CapableRunner); ok {
		return c.Capabilities().EstimatesCost
	}
	_, isAPI := r.(*APIRunner)
	return isAPI
}
