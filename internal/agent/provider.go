package agent

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// A Provider builds a Runner from configuration, registered under the name
// `agent.runner` selects. It is the seam that makes the model behind generation
// a deployment decision rather than a kiln decision.
//
// The shape deliberately mirrors internal/connector, which solved the same
// problem for sources: one registry, registration in init(), a panic on
// duplicate names. A reader who has understood how a source connector is added
// already knows how a model provider is added.
//
// Everything structural stays outside a provider's reach. What to regenerate,
// the page index, the log, validation, the budget ledger, and the content-hash
// gate are decided deterministically in Go, and a provider that returned
// nonsense would fail validation rather than corrupt the wiki. A provider
// supplies prose and nothing else.
type Provider interface {
	// Name is the value `agent.runner` is matched against.
	Name() string
	// New builds a runner. Returning an error here fails startup, which is the
	// right moment: a misconfigured provider should stop a deployment from
	// booting rather than fail every unit of the first build.
	New(opts Options) (Runner, error)
}

// Options is the resolved configuration handed to a provider.
//
// The fields are the ones every provider plausibly needs. Anything specific to
// one -- a project id, a deployment name, a local socket path -- belongs in
// Settings rather than here, so adding a provider never widens this struct.
type Options struct {
	// APIKey is the credential for the selected provider. Empty is legitimate:
	// a provider may fall back to its SDK's own resolution, to ambient cloud
	// identity, or to a logged-in CLI session.
	APIKey string
	// BaseURL redirects at a gateway, proxy, or self-hosted endpoint.
	BaseURL string
	// Binary is the executable for providers that shell out rather than call
	// an API.
	Binary string

	// Model, and the two roles that may differ from it: a cheaper model for
	// the analyze pass, and one to fall back to when the first is overloaded.
	Model         string
	AnalyzeModel  string
	FallbackModel string
	// Effort tunes thinking depth where the provider supports it.
	Effort string
	// Timeout bounds a single call.
	Timeout time.Duration

	// Settings carries provider-specific configuration from `agent.settings`,
	// so a provider kiln has never heard of can still be configured without a
	// change to config.Agent.
	Settings Settings
}

// Settings is untyped provider configuration with checked accessors, matching
// connector.Config. Untyped because the set of keys is open by construction:
// the whole point is that kiln does not know what a provider needs.
type Settings map[string]any

// String reads a string setting.
func (s Settings) String(key string) (string, bool) {
	v, ok := s[key]
	if !ok {
		return "", false
	}
	str, ok := v.(string)
	return str, ok
}

// RequireString reads a setting a provider cannot run without.
func (s Settings) RequireString(key string) (string, error) {
	v, ok := s.String(key)
	if !ok || v == "" {
		return "", fmt.Errorf("agent: setting %q is required", key)
	}
	return v, nil
}

// Bool reads a boolean setting.
func (s Settings) Bool(key string) (bool, bool) {
	v, ok := s[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// Float reads a numeric setting. Both int and float64 are accepted because a
// value's Go type here depends on whether it arrived from TOML, JSON, or an
// environment variable, which is not a distinction a provider should care about.
func (s Settings) Float(key string) (float64, bool) {
	switch v := s[key].(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}

// StringSlice reads a list setting, accepting a single string as a list of one
// so `x = "a"` and `x = ["a"]` mean the same thing.
func (s Settings) StringSlice(key string) ([]string, bool) {
	switch v := s[key].(type) {
	case []string:
		return v, true
	case string:
		return []string{v}, true
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			str, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, str)
		}
		return out, true
	default:
		return nil, false
	}
}

// Duration reads a duration setting, accepting either a time.Duration passed
// through in Go or a string like "2s" from a config file.
func (s Settings) Duration(key string) (time.Duration, bool) {
	switch v := s[key].(type) {
	case time.Duration:
		return v, true
	case string:
		d, err := time.ParseDuration(v)
		return d, err == nil
	default:
		return 0, false
	}
}

// Capabilities describes what the pipeline must do differently for a runner.
//
// These were type assertions against the two built-in runners, which meant a
// third could not state either fact and would be silently treated as
// API-shaped: no scratch directory allocated for the files it writes, and
// budget enforcement disabled because its costs were assumed authoritative.
type Capabilities struct {
	// WritesFiles is true when pages come back on disk rather than as
	// structured data on Result.Generation. The pipeline allocates a scratch
	// directory only for these, which is most of why the API runner exists.
	WritesFiles bool
	// EstimatesCost is true when Result.TotalCostUSD is computed locally from
	// a pricing table rather than reported by the provider. Budget enforcement
	// needs to know, because an estimated zero means "unknown model", not
	// "free", and silently treating it as free would uncap a run.
	EstimatesCost bool
}

// A CapableRunner states its own capabilities. Optional: a runner that does not
// implement it is described by the built-in fallbacks in WritesFiles and
// EstimatesCost, so existing runners and test fakes keep working unchanged.
type CapableRunner interface {
	Runner
	Capabilities() Capabilities
}

var (
	providersMu sync.RWMutex
	providers   = map[string]Provider{}
)

// RegisterProvider makes a provider selectable by name. Registering the same
// name twice panics: it means two implementations disagree about who serves a
// runner name, which is a wiring bug rather than a runtime condition.
func RegisterProvider(p Provider) {
	providersMu.Lock()
	defer providersMu.Unlock()

	if _, exists := providers[p.Name()]; exists {
		panic(fmt.Sprintf("agent: provider %q registered twice", p.Name()))
	}
	providers[p.Name()] = p
}

// unregisterProvider removes a registration. Unexported and test-only: a
// running process has no reason to withdraw a provider, and offering it would
// invite exactly the mid-flight reconfiguration the panic above guards against.
func unregisterProvider(name string) {
	providersMu.Lock()
	defer providersMu.Unlock()
	delete(providers, name)
}

// LookupProvider returns a registered provider.
func LookupProvider(name string) (Provider, error) {
	providersMu.RLock()
	defer providersMu.RUnlock()

	p, ok := providers[name]
	if !ok {
		return nil, fmt.Errorf("agent: no provider registered for %q (have: %v)", name, providerNamesLocked())
	}
	return p, nil
}

// Providers lists the registered provider names, sorted. Errors quote this
// rather than a hardcoded list, so what a reader is told is available is what
// this binary actually has.
func Providers() []string {
	providersMu.RLock()
	defer providersMu.RUnlock()
	return providerNamesLocked()
}

func providerNamesLocked() []string {
	out := make([]string, 0, len(providers))
	for name := range providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// providerFunc adapts a function to the Provider interface, for the built-ins
// and for tests that register a one-off.
type providerFunc struct {
	name string
	new  func(Options) (Runner, error)
}

func (p providerFunc) Name() string                  { return p.name }
func (p providerFunc) New(o Options) (Runner, error) { return p.new(o) }

// ProviderFunc builds a Provider from a name and a constructor.
func ProviderFunc(name string, newRunner func(Options) (Runner, error)) Provider {
	return providerFunc{name: name, new: newRunner}
}
