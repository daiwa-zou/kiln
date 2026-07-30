package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Role distinguishes the two server roles, which differ in how they use
// Postgres and therefore in how their connection pools are sized.
type Role string

const (
	RoleServer Role = "server"
	RoleWorker Role = "worker"
)

// Config is the fully resolved configuration. Nothing downstream re-merges:
// Load returns this ready to use.
type Config struct {
	Role     Role   `mapstructure:"-"`
	HTTPAddr string `mapstructure:"http_addr"`
	// MetricsAddr serves Prometheus metrics on its own listener. Separate
	// from the API on purpose: metrics describe the deployment, not a tenant,
	// so exposing them on the public router would mean either publishing
	// queue depth and spend to every reader or inventing an auth scheme
	// Prometheus does not want to use. Empty disables the listener.
	MetricsAddr string `mapstructure:"metrics_addr"`
	PublicURL   string `mapstructure:"public_url"`
	LogLevel    string `mapstructure:"log_level"`
	// CORSOrigins are browser origins allowed to call the API, for a
	// separately hosted frontend. Empty means same-origin only.
	CORSOrigins []string `mapstructure:"cors_origins"`

	Database Database `mapstructure:"database"`
	Storage  Storage  `mapstructure:"storage"`
	Agent    Agent    `mapstructure:"agent"`
	Auth     Auth     `mapstructure:"auth"`
	Worker   Worker   `mapstructure:"worker"`
	GitHub   GitHub   `mapstructure:"github"`
	Secrets  Secrets  `mapstructure:"-"`
}

// GitHub configures the GitHub App integration: OAuth sign-in and
// installation tokens for clones. Everything here is public identity; the
// client secret, webhook secret, and private key travel through Secrets.
type GitHub struct {
	// ClientID is the App's OAuth client id. Empty disables sign-in.
	ClientID string `mapstructure:"client_id"`
	// AppID is the numeric App id, for installation tokens.
	AppID int64 `mapstructure:"app_id"`
	// AppSlug names the App's public install page.
	AppSlug string `mapstructure:"app_slug"`
	// BaseURL and APIBaseURL override github.com, for GitHub Enterprise and
	// for tests.
	BaseURL    string `mapstructure:"base_url"`
	APIBaseURL string `mapstructure:"api_base_url"`
	// SessionTTL is how long a browser session lives.
	SessionTTL time.Duration `mapstructure:"session_ttl"`
	// WebhookCooldown is the quiet period after a finished run during which
	// further pushes do not enqueue another, so a push storm costs one
	// rebuild. Zero disables the cooldown.
	WebhookCooldown time.Duration `mapstructure:"webhook_cooldown"`
}

// Worker holds settings for the build worker loop.
type Worker struct {
	// PollInterval is how often an idle worker checks the queue.
	PollInterval time.Duration `mapstructure:"poll_interval"`
	// StaleAfter is how long a claimed run may hold 'running' before it is
	// assumed orphaned by a dead worker and returned to the queue.
	StaleAfter time.Duration `mapstructure:"stale_after"`
	// SourcePollInterval is how often trigger_mode='poll' connectors are due
	// for a refresh. Zero disables the poll scheduler.
	SourcePollInterval time.Duration `mapstructure:"source_poll_interval"`
	// DrainGrace is how long an in-flight build may continue after shutdown
	// is requested before it is interrupted and requeued.
	DrainGrace time.Duration `mapstructure:"drain_grace"`
	// PermittedSourceRoots are the only directories a database-configured
	// connector may read from. Empty means every local path is denied: a
	// connector config is API-writable data, and an unchecked path would be a
	// local-file-inclusion primitive. The CLI, whose paths come from the
	// operator's own command line, is not subject to this list.
	PermittedSourceRoots []string `mapstructure:"permitted_source_roots"`
}

// AuthMode selects how the HTTP API authenticates callers.
type AuthMode string

const (
	// AuthToken requires a bearer token from the tokens table on every API
	// request. This is the default: a team deployment must opt out of
	// authentication deliberately, never arrive without it by omission.
	AuthToken AuthMode = "token"
	// AuthNone disables authentication. Only defensible for a single-user
	// instance bound to localhost.
	AuthNone AuthMode = "none"
)

// Auth holds API authentication settings.
type Auth struct {
	Mode AuthMode `mapstructure:"mode"`
}

// Database holds Postgres connection and pool settings.
//
// URL wins when set; otherwise the discrete fields are assembled into a DSN, so
// Kubernetes deployments can source the password from a separate secret without
// templating a whole connection string.
type Database struct {
	URL         string `mapstructure:"url"`
	Host        string `mapstructure:"host"`
	Port        int    `mapstructure:"port"`
	Name        string `mapstructure:"name"`
	User        string `mapstructure:"user"`
	Password    string `mapstructure:"password"`
	SSLMode     string `mapstructure:"sslmode"`
	SSLRootCert string `mapstructure:"sslrootcert"`

	MaxConns        int32         `mapstructure:"max_conns"`
	MinConns        int32         `mapstructure:"min_conns"`
	MaxConnLifetime time.Duration `mapstructure:"max_conn_lifetime"`
	MaxConnIdleTime time.Duration `mapstructure:"max_conn_idle_time"`
}

// StorageBackend selects where blobs live.
type StorageBackend string

const (
	// BackendS3 targets any S3-compatible service: MinIO, AWS S3, R2, Backblaze.
	BackendS3 StorageBackend = "s3"
	// BackendFS writes to a mounted volume. Included so a single-node deployment
	// needs no object store at all; unsuitable once workers span hosts.
	BackendFS StorageBackend = "fs"
)

// Storage holds object storage settings.
type Storage struct {
	Backend   StorageBackend `mapstructure:"backend"`
	Endpoint  string         `mapstructure:"endpoint"`
	Region    string         `mapstructure:"region"`
	Bucket    string         `mapstructure:"bucket"`
	AccessKey string         `mapstructure:"access_key"`
	SecretKey string         `mapstructure:"secret_key"`
	UseSSL    bool           `mapstructure:"use_ssl"`
	// PathStyle is required by MinIO; AWS uses virtual-host addressing.
	PathStyle bool   `mapstructure:"path_style"`
	Path      string `mapstructure:"path"`

	RunArtifactRetention time.Duration `mapstructure:"run_artifact_retention"`
	SoftDeleteRetention  time.Duration `mapstructure:"soft_delete_retention"`
}

// AgentRunner selects how kiln reaches Claude.
type AgentRunner string

const (
	// RunnerAPI calls the Anthropic API directly and asks for structured
	// output, so the model returns page content as data and never touches a
	// filesystem. This is the default: it needs no sandbox, no write guard, and
	// no path-escape checks, and it allows explicit prompt caching across units.
	RunnerAPI AgentRunner = "api"
	// RunnerCLI shells out to the claude binary, which writes pages into a
	// scratch directory. Kept for local development against an existing CLI
	// session, and for anyone who would rather not manage an API key.
	RunnerCLI AgentRunner = "cli"
	// RunnerFake generates deterministic placeholder pages with zero API
	// spend, for local development and end-to-end testing. Every pipeline
	// stage except the prose itself is real: sync, mapping, validation,
	// import, the queue, and the budget ledger all run exactly as production.
	RunnerFake AgentRunner = "fake"
)

// Agent holds settings for reaching Claude.
type Agent struct {
	// Runner picks the transport. Defaults to the API.
	Runner AgentRunner `mapstructure:"runner"`
	// Binary is the claude executable, used only by the CLI runner.
	Binary string `mapstructure:"binary"`
	// BaseURL overrides the API endpoint, mainly for testing.
	BaseURL string `mapstructure:"base_url"`
	// Effort tunes thinking depth and spend: low, medium, high, xhigh, max.
	Effort string `mapstructure:"effort"`

	Model         string        `mapstructure:"model"`
	AnalyzeModel  string        `mapstructure:"analyze_model"`
	FallbackModel string        `mapstructure:"fallback_model"`
	Timeout       time.Duration `mapstructure:"timeout"`

	AnalyzeBudgetUSD float64 `mapstructure:"analyze_budget_usd"`
	PageBudgetUSD    float64 `mapstructure:"page_budget_usd"`
	RunBudgetUSD     float64 `mapstructure:"run_budget_usd"`
	MaxPagesPerRun   int     `mapstructure:"max_pages_per_run"`
	// BudgetWindow is the rolling window a workspace's budget_usd cap applies
	// to, enforced when runs are enqueued. Zero disables window enforcement.
	BudgetWindow time.Duration `mapstructure:"budget_window"`
	// WarnTurns triggers a log warning; the installed claude CLI has no
	// --max-turns, so wall-clock capping is done with Timeout instead.
	WarnTurns int `mapstructure:"warn_turns"`

	// FakeCostUSD is the fake runner's synthetic per-call cost. Non-zero by
	// default so the spend ledger, cost estimates, and budget windows behave
	// realistically in development.
	FakeCostUSD float64 `mapstructure:"fake_cost_usd"`
	// FakeFailUnits makes the fake runner fail any unit whose key contains
	// one of these substrings, for exercising partial-run and retry paths.
	FakeFailUnits []string `mapstructure:"fake_fail_units"`
	// FakeLatency delays each fake call, for watching queue and UI
	// transitions happen at human speed.
	FakeLatency time.Duration `mapstructure:"fake_latency"`
}

// Secrets holds values that must never be logged or serialized. They are kept
// off the main structs so an accidental %+v on Config cannot leak them.
type Secrets struct {
	MasterKey           string
	AnthropicAPIKey     string
	SessionSecret       string
	GitHubClientSecret  string
	GitHubPrivateKey    string
	GitHubWebhookSecret string
	StorageSecretKey    string
}

// DSN returns the Postgres connection string, preferring an explicitly
// configured URL and otherwise assembling one from the discrete fields.
func (d Database) DSN() string {
	if strings.TrimSpace(d.URL) != "" {
		return d.URL
	}

	q := url.Values{}
	if d.SSLMode != "" {
		q.Set("sslmode", d.SSLMode)
	}
	if d.SSLRootCert != "" {
		q.Set("sslrootcert", d.SSLRootCert)
	}

	u := &url.URL{
		Scheme:   "postgres",
		Host:     fmt.Sprintf("%s:%d", d.Host, d.Port),
		Path:     "/" + d.Name,
		RawQuery: q.Encode(),
	}
	if d.User != "" {
		if d.Password != "" {
			u.User = url.UserPassword(d.User, d.Password)
		} else {
			u.User = url.User(d.User)
		}
	}
	return u.String()
}

// Redacted returns the DSN with any password replaced, for logging.
func (d Database) Redacted() string {
	u, err := url.Parse(d.DSN())
	if err != nil {
		return "postgres://<unparseable>"
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			u.User = url.UserPassword(u.User.Username(), "xxxxx")
		}
	}
	return u.String()
}
